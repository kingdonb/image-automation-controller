/*
Copyright 2020 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"golang.org/x/crypto/openpgp"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	gogit "github.com/go-git/go-git/v5"
	libgit2 "github.com/libgit2/git2go/v31"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kuberecorder "k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	imagev1_reflect "github.com/fluxcd/image-reflector-controller/api/v1alpha1"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/metrics"
	"github.com/fluxcd/pkg/runtime/predicates"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	git "github.com/fluxcd/source-controller/pkg/git"
	gitstrat "github.com/fluxcd/source-controller/pkg/git/strategy"

	imagev1 "github.com/fluxcd/image-automation-controller/api/v1alpha2"
	"github.com/fluxcd/image-automation-controller/pkg/update"
)

// log level for debug info
const debug = 1
const originRemote = "origin"

const defaultMessageTemplate = `Update from image update automation`

const repoRefKey = ".spec.gitRepository"

const signingSecretKey = "git.asc"

// TemplateData is the type of the value given to the commit message
// template.
type TemplateData struct {
	AutomationObject types.NamespacedName
	Updated          update.Result
}

// ImageUpdateAutomationReconciler reconciles a ImageUpdateAutomation object
type ImageUpdateAutomationReconciler struct {
	client.Client
	Scheme                *runtime.Scheme
	EventRecorder         kuberecorder.EventRecorder
	ExternalEventRecorder *events.Recorder
	MetricsRecorder       *metrics.Recorder
}

// +kubebuilder:rbac:groups=image.toolkit.fluxcd.io,resources=imageupdateautomations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=image.toolkit.fluxcd.io,resources=imageupdateautomations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories,verbs=get;list;watch

func (r *ImageUpdateAutomationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContext(ctx)
	now := time.Now()
	var templateValues TemplateData

	var auto imagev1.ImageUpdateAutomation
	if err := r.Get(ctx, req.NamespacedName, &auto); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// record suspension metrics
	defer r.recordSuspension(ctx, auto)

	if auto.Spec.Suspend {
		log.Info("ImageUpdateAutomation is suspended, skipping automation run")
		return ctrl.Result{}, nil
	}

	templateValues.AutomationObject = req.NamespacedName

	// Record readiness metric when exiting; if there's any points at
	// which the readiness is updated _without also exiting_, they
	// should also record the readiness.
	defer r.recordReadinessMetric(ctx, &auto)
	// Record reconciliation duration when exiting
	if r.MetricsRecorder != nil {
		objRef, err := reference.GetReference(r.Scheme, &auto)
		if err != nil {
			return ctrl.Result{}, err
		}
		defer r.MetricsRecorder.RecordDuration(*objRef, now)
	}

	// whatever else happens, we've now "seen" the reconcile
	// annotation if it's there
	if token, ok := meta.ReconcileAnnotationValue(auto.GetAnnotations()); ok {
		auto.Status.SetLastHandledReconcileRequest(token)

		if err := r.patchStatus(ctx, req, auto.Status); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
	}

	// failWithError is a helper for bailing on the reconciliation.
	failWithError := func(err error) (ctrl.Result, error) {
		r.event(ctx, auto, events.EventSeverityError, err.Error())
		imagev1.SetImageUpdateAutomationReadiness(&auto, metav1.ConditionFalse, meta.ReconciliationFailedReason, err.Error())
		if err := r.patchStatus(ctx, req, auto.Status); err != nil {
			log.Error(err, "failed to reconcile")
		}
		return ctrl.Result{Requeue: true}, err
	}

	// get the git repository object so it can be checked out

	// only GitRepository objects are supported for now
	if kind := auto.Spec.SourceRef.Kind; kind != sourcev1.GitRepositoryKind {
		return failWithError(fmt.Errorf("source kind %q not supported", kind))
	}
	gitSpec := auto.Spec.GitSpec
	if gitSpec == nil {
		return failWithError(fmt.Errorf("source kind %s neccessitates field .spec.git", sourcev1.GitRepositoryKind))
	}

	var origin sourcev1.GitRepository
	originName := types.NamespacedName{
		Name:      auto.Spec.SourceRef.Name,
		Namespace: auto.GetNamespace(),
	}
	if err := r.Get(ctx, originName, &origin); err != nil {
		if client.IgnoreNotFound(err) == nil {
			imagev1.SetImageUpdateAutomationReadiness(&auto, metav1.ConditionFalse, imagev1.GitNotAvailableReason, "referenced git repository is missing")
			log.Error(err, "referenced git repository does not exist")
			if err := r.patchStatus(ctx, req, auto.Status); err != nil {
				return ctrl.Result{Requeue: true}, err
			}
			return ctrl.Result{}, nil // and assume we'll hear about it when it arrives
		}
		return ctrl.Result{}, err
	}

	log.V(debug).Info("found git repository", "gitrepository", originName)

	// validate the git spec and default any values needed later, before proceeding
	var ref *sourcev1.GitRepositoryRef
	if gitSpec.Checkout != nil {
		ref = &gitSpec.Checkout.Reference
	} else if r := origin.Spec.Reference; r != nil {
		ref = r
	} // else remain as `nil`, which is an acceptable value for cloneInto, later.

	var pushBranch string
	if gitSpec.Push != nil {
		pushBranch = gitSpec.Push.Branch
	} else {
		// Here's where it gets constrained. If there's no push branch
		// given, then the checkout ref must include a branch, and
		// that can be used.
		if ref.Branch == "" {
			failWithError(fmt.Errorf("Push branch not given explicitly, and cannot be inferred from .spec.git.checkout.ref or GitRepository .spec.ref"))
		}
		pushBranch = ref.Branch
	}

	tmp, err := ioutil.TempDir("", fmt.Sprintf("%s-%s", originName.Namespace, originName.Name))
	if err != nil {
		return failWithError(err)
	}
	defer os.RemoveAll(tmp)

	// FIXME use context with deadline for at least the following ops

	access, err := r.getRepoAccess(ctx, &origin)
	if err != nil {
		return failWithError(err)
	}

	var repo *gogit.Repository
	if repo, err = cloneInto(ctx, access, ref, tmp, origin.Spec.GitImplementation); err != nil {
		return failWithError(err)
	}

	// When there's a push spec, the pushed-to branch is where commits
	// shall be made

	if gitSpec.Push != nil {
		if err := fetch(ctx, tmp, repo, pushBranch, access, origin.Spec.GitImplementation); err != nil && err != errRemoteBranchMissing {
			return failWithError(err)
		}
		if err = switchBranch(repo, pushBranch); err != nil {
			return failWithError(err)
		}
	}

	log.V(debug).Info("cloned git repository", "gitrepository", originName, "ref", ref, "working", tmp)

	manifestsPath := tmp
	if auto.Spec.Update.Path != "" {
		if p, err := securejoin.SecureJoin(tmp, auto.Spec.Update.Path); err != nil {
			return failWithError(err)
		} else {
			manifestsPath = p
		}
	}

	switch {
	case auto.Spec.Update != nil && auto.Spec.Update.Strategy == imagev1.UpdateStrategySetters:
		// For setters we first want to compile a list of _all_ the
		// policies in the same namespace (maybe in the future this
		// could be filtered by the automation object).
		var policies imagev1_reflect.ImagePolicyList
		if err := r.List(ctx, &policies, &client.ListOptions{Namespace: req.NamespacedName.Namespace}); err != nil {
			return failWithError(err)
		}

		if result, err := updateAccordingToSetters(ctx, manifestsPath, policies.Items); err != nil {
			return failWithError(err)
		} else {
			templateValues.Updated = result
		}
	default:
		log.Info("no update strategy given in the spec")
		// no sense rescheduling until this resource changes
		r.event(ctx, auto, events.EventSeverityInfo, "no known update strategy in spec, failing trivially")
		imagev1.SetImageUpdateAutomationReadiness(&auto, metav1.ConditionFalse, imagev1.NoStrategyReason, "no known update strategy is given for object")
		return ctrl.Result{}, r.patchStatus(ctx, req, auto.Status)
	}

	log.V(debug).Info("ran updates to working dir", "working", tmp)

	var statusMessage string

	var signingEntity *openpgp.Entity
	if gitSpec.Commit.SigningKey != nil {
		signingEntity, err = r.getSigningEntity(ctx, auto)
	}

	// construct the commit message from template and values
	msgTmpl := gitSpec.Commit.MessageTemplate
	if msgTmpl == "" {
		msgTmpl = defaultMessageTemplate
	}
	tmpl, err := template.New("commit message").Parse(msgTmpl)
	if err != nil {
		return failWithError(fmt.Errorf("unable to create commit message template from spec: %w", err))
	}
	messageBuf := &strings.Builder{}
	if err := tmpl.Execute(messageBuf, templateValues); err != nil {
		return failWithError(fmt.Errorf("failed to run template from spec: %w", err))
	}

	// The status message depends on what happens next. Since there's
	// more than one way to succeed, there's some if..else below, and
	// early returns only on failure.
	author := &object.Signature{
		Name:  gitSpec.Commit.Author.Name,
		Email: gitSpec.Commit.Author.Email,
		When:  time.Now(),
	}
	if rev, err := commitChangedManifests(repo, tmp, signingEntity, author, messageBuf.String()); err != nil {
		if err == errNoChanges {
			r.event(ctx, auto, events.EventSeverityInfo, "no updates made")
			log.V(debug).Info("no changes made in working directory; no commit")
			statusMessage = "no updates made"
			if lastCommit, lastTime := auto.Status.LastPushCommit, auto.Status.LastPushTime; lastCommit != "" {
				statusMessage = fmt.Sprintf("%s; last commit %s at %s", statusMessage, lastCommit[:7], lastTime.Format(time.RFC3339))
			}
		} else {
			return failWithError(err)
		}
	} else {
		if err := push(ctx, tmp, repo, pushBranch, access, origin.Spec.GitImplementation); err != nil {
			return failWithError(err)
		}

		r.event(ctx, auto, events.EventSeverityInfo, "committed and pushed change "+rev+" to "+pushBranch)
		log.Info("pushed commit to origin", "revision", rev, "branch", pushBranch)
		auto.Status.LastPushCommit = rev
		auto.Status.LastPushTime = &metav1.Time{Time: now}
		statusMessage = "committed and pushed " + rev + " to " + pushBranch
	}

	// Getting to here is a successful run.
	auto.Status.LastAutomationRunTime = &metav1.Time{Time: now}
	imagev1.SetImageUpdateAutomationReadiness(&auto, metav1.ConditionTrue, meta.ReconciliationSucceededReason, statusMessage)
	if err := r.patchStatus(ctx, req, auto.Status); err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	// We're either in this method because something changed, or this
	// object got requeued. Either way, once successful, we don't need
	// to see the object again until Interval has passed, or something
	// changes again.

	interval := intervalOrDefault(&auto)
	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *ImageUpdateAutomationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	// Index the git repository object that each I-U-A refers to
	if err := mgr.GetFieldIndexer().IndexField(ctx, &imagev1.ImageUpdateAutomation{}, repoRefKey, func(obj client.Object) []string {
		updater := obj.(*imagev1.ImageUpdateAutomation)
		ref := updater.Spec.SourceRef
		return []string{ref.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&imagev1.ImageUpdateAutomation{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}))).
		Watches(&source.Kind{Type: &sourcev1.GitRepository{}}, handler.EnqueueRequestsFromMapFunc(r.automationsForGitRepo)).
		Watches(&source.Kind{Type: &imagev1_reflect.ImagePolicy{}}, handler.EnqueueRequestsFromMapFunc(r.automationsForImagePolicy)).
		Complete(r)
}

func (r *ImageUpdateAutomationReconciler) patchStatus(ctx context.Context,
	req ctrl.Request,
	newStatus imagev1.ImageUpdateAutomationStatus) error {

	var auto imagev1.ImageUpdateAutomation
	if err := r.Get(ctx, req.NamespacedName, &auto); err != nil {
		return err
	}

	patch := client.MergeFrom(auto.DeepCopy())
	auto.Status = newStatus

	return r.Status().Patch(ctx, &auto, patch)
}

// intervalOrDefault gives the interval specified, or if missing, the default
func intervalOrDefault(auto *imagev1.ImageUpdateAutomation) time.Duration {
	if auto.Spec.Interval.Duration < time.Second {
		return time.Second
	}
	return auto.Spec.Interval.Duration
}

// durationSinceLastRun calculates how long it's been since the last
// time the automation ran (which you can then use to find how long to
// wait until the next run).
func durationSinceLastRun(auto *imagev1.ImageUpdateAutomation, now time.Time) time.Duration {
	last := auto.Status.LastAutomationRunTime
	if last == nil {
		return time.Duration(math.MaxInt64) // a fairly long time
	}
	return now.Sub(last.Time)
}

// automationsForGitRepo fetches all the automations that refer to a
// particular source.GitRepository object.
func (r *ImageUpdateAutomationReconciler) automationsForGitRepo(obj client.Object) []reconcile.Request {
	ctx := context.Background()
	var autoList imagev1.ImageUpdateAutomationList
	if err := r.List(ctx, &autoList, client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{repoRefKey: obj.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, len(autoList.Items), len(autoList.Items))
	for i := range autoList.Items {
		reqs[i].NamespacedName.Name = autoList.Items[i].GetName()
		reqs[i].NamespacedName.Namespace = autoList.Items[i].GetNamespace()
	}
	return reqs
}

// automationsForImagePolicy fetches all the automation objects that
// might depend on a image policy object. Since the link is via
// markers in the git repo, _any_ automation object in the same
// namespace could be affected.
func (r *ImageUpdateAutomationReconciler) automationsForImagePolicy(obj client.Object) []reconcile.Request {
	ctx := context.Background()
	var autoList imagev1.ImageUpdateAutomationList
	if err := r.List(ctx, &autoList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, len(autoList.Items), len(autoList.Items))
	for i := range autoList.Items {
		reqs[i].NamespacedName.Name = autoList.Items[i].GetName()
		reqs[i].NamespacedName.Namespace = autoList.Items[i].GetNamespace()
	}
	return reqs
}

// --- git ops

type repoAccess struct {
	auth *git.Auth
	url  string
}

func (r *ImageUpdateAutomationReconciler) getRepoAccess(ctx context.Context, repository *sourcev1.GitRepository) (repoAccess, error) {
	var access repoAccess
	access.auth = &git.Auth{}
	access.url = repository.Spec.URL
	authStrat, err := gitstrat.AuthSecretStrategyForURL(access.url, repository.Spec.GitImplementation)
	if err != nil {
		return access, err
	}

	if repository.Spec.SecretRef != nil && authStrat != nil {
		name := types.NamespacedName{
			Namespace: repository.GetNamespace(),
			Name:      repository.Spec.SecretRef.Name,
		}

		var secret corev1.Secret
		err := r.Client.Get(ctx, name, &secret)
		if err != nil {
			err = fmt.Errorf("auth secret error: %w", err)
			return access, err
		}

		access.auth, err = authStrat.Method(secret)
		if err != nil {
			err = fmt.Errorf("auth error: %w", err)
			return access, err
		}
	}
	return access, nil
}

func (r repoAccess) remoteCallbacks() libgit2.RemoteCallbacks {
	return libgit2.RemoteCallbacks{
		CertificateCheckCallback: r.auth.CertCallback,
		CredentialsCallback:      r.auth.CredCallback,
	}
}

// cloneInto clones the upstream repository at the `ref` given (which
// can be `nil`), using the git library indicated by `impl`. It
// returns a `*gogit.Repository` regardless of the git library, since
// that is used for committing changes.
func cloneInto(ctx context.Context, access repoAccess, ref *sourcev1.GitRepositoryRef, path, impl string) (*gogit.Repository, error) {
	checkoutStrat, err := gitstrat.CheckoutStrategyForRef(ref, impl)
	if err == nil {
		_, _, err = checkoutStrat.Checkout(ctx, path, access.url, access.auth)
	}
	if err != nil {
		return nil, err
	}

	return gogit.PlainOpen(path)
}

// switchBranch switches the repo from the current branch to the
// branch given. If the branch does not exist, it is created using the
// head as the starting point.
func switchBranch(repo *gogit.Repository, pushBranch string) error {
	localBranch := plumbing.NewBranchReferenceName(pushBranch)

	// is the branch already present?
	_, err := repo.Reference(localBranch, false)
	switch {
	case err == plumbing.ErrReferenceNotFound:
		// make a new branch, starting at HEAD
		head, err := repo.Head()
		if err != nil {
			return err
		}
		branchRef := plumbing.NewHashReference(localBranch, head.Hash())
		if err = repo.Storer.SetReference(branchRef); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		// local branch found, great
		break
	}

	tree, err := repo.Worktree()
	if err != nil {
		return err
	}

	return tree.Checkout(&gogit.CheckoutOptions{
		Branch: localBranch,
	})
}

var errNoChanges error = errors.New("no changes made to working directory")

func commitChangedManifests(repo *gogit.Repository, absRepoPath string, ent *openpgp.Entity, author *object.Signature, message string) (string, error) {
	working, err := repo.Worktree()
	if err != nil {
		return "", err

	}
	status, err := working.Status()
	if err != nil {
		return "", err
	}

	// go-git has [a bug](https://github.com/go-git/go-git/issues/253)
	// whereby it thinks broken symlinks to absolute paths are
	// modified. There's no circumstance in which we want to commit a
	// change to a broken symlink: so, detect and skip those.
	var changed bool
	for file, _ := range status {
		abspath := filepath.Join(absRepoPath, file)
		info, err := os.Lstat(abspath)
		if err != nil {
			return "", fmt.Errorf("checking if %s is a symlink: %w", file, err)
		}
		if info.Mode()&os.ModeSymlink > 0 {
			// symlinks are OK; broken symlinks are probably a result
			// of the bug mentioned above, but not of interest in any
			// case.
			if _, err := os.Stat(abspath); os.IsNotExist(err) {
				continue
			}
		}
		working.Add(file)
		changed = true
	}

	if !changed {
		return "", errNoChanges
	}

	var rev plumbing.Hash
	if rev, err = working.Commit(message, &gogit.CommitOptions{
		Author:  author,
		SignKey: ent,
	}); err != nil {
		return "", err
	}

	return rev.String(), nil
}

// getSigningEntity retrieves an OpenPGP entity referenced by the
// provided imagev1.ImageUpdateAutomation for git commit signing
func (r *ImageUpdateAutomationReconciler) getSigningEntity(ctx context.Context, auto imagev1.ImageUpdateAutomation) (*openpgp.Entity, error) {
	// get kubernetes secret
	secretName := types.NamespacedName{
		Namespace: auto.GetNamespace(),
		Name:      auto.Spec.GitSpec.Commit.SigningKey.SecretRef.Name,
	}
	var secret corev1.Secret
	if err := r.Get(ctx, secretName, &secret); err != nil {
		return nil, fmt.Errorf("could not find signing key secret '%s': %w", secretName, err)
	}

	// get data from secret
	data, ok := secret.Data[signingSecretKey]
	if !ok {
		return nil, fmt.Errorf("signing key secret '%s' does not contain a 'git.asc' key", secretName)
	}

	// read entity from secret value
	entities, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("could not read signing key from secret '%s': %w", secretName, err)
	}
	if len(entities) > 1 {
		return nil, fmt.Errorf("multiple entities read from secret '%s', could not determine which signing key to use", secretName)
	}
	return entities[0], nil
}

var errRemoteBranchMissing = errors.New("remote branch missing")

// fetch gets the remote branch given and updates the local branch
// head of the same name, so it can be switched to. If the fetch
// completes, it returns nil; if the remote branch is missing, it
// returns errRemoteBranchMissing (this is to work in sympathy with
// `switchBranch`, which will create the branch if it doesn't
// exist). For any other problem it will return the error.
func fetch(ctx context.Context, path string, repo *gogit.Repository, branch string, access repoAccess, impl string) error {
	refspec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch)
	switch impl {
	case sourcev1.LibGit2Implementation:
		lg2repo, err := libgit2.OpenRepository(path)
		if err != nil {
			return err
		}
		return fetchLibgit2(lg2repo, refspec, access)
	case sourcev1.GoGitImplementation:
		return fetchGoGit(ctx, repo, refspec, access)
	default:
		return fmt.Errorf("unknown git implementation %q", impl)
	}
}

func fetchLibgit2(repo *libgit2.Repository, refspec string, access repoAccess) error {
	origin, err := repo.Remotes.Lookup(originRemote)
	if err != nil {
		return err
	}
	err = origin.Fetch(
		[]string{refspec},
		&libgit2.FetchOptions{
			RemoteCallbacks: access.remoteCallbacks(),
		}, "",
	)
	if err != nil && libgit2.IsErrorCode(err, libgit2.ErrorCodeNotFound) {
		return errRemoteBranchMissing
	}
	return err
}

func fetchGoGit(ctx context.Context, repo *gogit.Repository, refspec string, access repoAccess) error {
	err := repo.FetchContext(ctx, &gogit.FetchOptions{
		RemoteName: originRemote,
		RefSpecs:   []config.RefSpec{config.RefSpec(refspec)},
		Auth:       access.auth.AuthMethod,
	})
	if err == gogit.NoErrAlreadyUpToDate {
		return nil
	}
	if _, ok := err.(gogit.NoMatchingRefSpecError); ok {
		return errRemoteBranchMissing
	}
	return err
}

// push pushes the branch given to the origin using the git library
// indicated by `impl`. It's passed both the path to the repo and a
// gogit.Repository value, since the latter may as well be used if the
// implementation is GoGit.
func push(ctx context.Context, path string, repo *gogit.Repository, branch string, access repoAccess, impl string) error {
	switch impl {
	case sourcev1.LibGit2Implementation:
		lg2repo, err := libgit2.OpenRepository(path)
		if err != nil {
			return err
		}
		return pushLibgit2(lg2repo, access, branch)
	case sourcev1.GoGitImplementation:
		return pushGoGit(ctx, repo, access, branch)
	default:
		return fmt.Errorf("unknown git implementation %q", impl)
	}
}

func pushGoGit(ctx context.Context, repo *gogit.Repository, access repoAccess, branch string) error {
	refspec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))
	err := repo.PushContext(ctx, &gogit.PushOptions{
		RemoteName: originRemote,
		Auth:       access.auth.AuthMethod,
		RefSpecs:   []config.RefSpec{refspec},
	})
	return gogitPushError(err)
}

func gogitPushError(err error) error {
	if err == nil {
		return nil
	}
	switch strings.TrimSpace(err.Error()) {
	case "unknown error: remote:":
		// this unhelpful error arises because go-git takes the first
		// line of the output on stderr, and for some git providers
		// (GitLab, at least) the output has a blank line at the
		// start. The rest of stderr is thrown away, so we can't get
		// the actual error; but at least we know what was being
		// attempted, and the likely cause.
		return fmt.Errorf("push rejected; check git secret has write access")
	default:
		return err
	}
}

func pushLibgit2(repo *libgit2.Repository, access repoAccess, branch string) error {
	origin, err := repo.Remotes.Lookup(originRemote)
	if err != nil {
		return err
	}
	err = origin.Push([]string{fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch)}, &libgit2.PushOptions{
		RemoteCallbacks: access.remoteCallbacks(),
	})
	return libgit2PushError(err)
}

func libgit2PushError(err error) error {
	if err == nil {
		return err
	}
	// libgit2 returns the whole output from stderr, and we only need
	// the message. GitLab likes to return a banner, so as an
	// heuristic, strip any lines that are just "remote:" and spaces
	// or fencing.
	msg := err.Error()
	lines := strings.Split(msg, "\n")
	if len(lines) == 1 {
		return err
	}
	var b strings.Builder
	// the following removes the prefix "remote:" from each line; to
	// retain a bit of fidelity to the original error, start with it.
	b.WriteString("remote: ")

	var appending bool
	for _, line := range lines {
		m := strings.TrimPrefix(line, "remote:")
		if m = strings.Trim(m, " \t="); m != "" {
			if appending {
				b.WriteString(" ")
			}
			b.WriteString(m)
			appending = true
		}
	}
	return errors.New(b.String())
}

// --- events, metrics

func (r *ImageUpdateAutomationReconciler) event(ctx context.Context, auto imagev1.ImageUpdateAutomation, severity, msg string) {
	if r.EventRecorder != nil {
		r.EventRecorder.Event(&auto, "Normal", severity, msg)
	}
	if r.ExternalEventRecorder != nil {
		objRef, err := reference.GetReference(r.Scheme, &auto)
		if err != nil {
			logr.FromContext(ctx).Error(err, "unable to send event")
			return
		}

		if err := r.ExternalEventRecorder.Eventf(*objRef, nil, severity, severity, msg); err != nil {
			logr.FromContext(ctx).Error(err, "unable to send event")
			return
		}
	}
}

func (r *ImageUpdateAutomationReconciler) recordReadinessMetric(ctx context.Context, auto *imagev1.ImageUpdateAutomation) {
	if r.MetricsRecorder == nil {
		return
	}

	objRef, err := reference.GetReference(r.Scheme, auto)
	if err != nil {
		logr.FromContext(ctx).Error(err, "unable to record readiness metric")
		return
	}
	if rc := apimeta.FindStatusCondition(auto.Status.Conditions, meta.ReadyCondition); rc != nil {
		r.MetricsRecorder.RecordCondition(*objRef, *rc, !auto.DeletionTimestamp.IsZero())
	} else {
		r.MetricsRecorder.RecordCondition(*objRef, metav1.Condition{
			Type:   meta.ReadyCondition,
			Status: metav1.ConditionUnknown,
		}, !auto.DeletionTimestamp.IsZero())
	}
}

// --- updates

// updateAccordingToSetters updates files under the root by treating
// the given image policies as kyaml setters.
func updateAccordingToSetters(ctx context.Context, path string, policies []imagev1_reflect.ImagePolicy) (update.Result, error) {
	return update.UpdateWithSetters(path, path, policies)
}

func (r *ImageUpdateAutomationReconciler) recordSuspension(ctx context.Context, auto imagev1.ImageUpdateAutomation) {
	if r.MetricsRecorder == nil {
		return
	}
	log := logr.FromContext(ctx)

	objRef, err := reference.GetReference(r.Scheme, &auto)
	if err != nil {
		log.Error(err, "unable to record suspended metric")
		return
	}

	if !auto.DeletionTimestamp.IsZero() {
		r.MetricsRecorder.RecordSuspend(*objRef, false)
	} else {
		r.MetricsRecorder.RecordSuspend(*objRef, auto.Spec.Suspend)
	}
}
