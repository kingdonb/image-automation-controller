module github.com/fluxcd/image-automation-controller

go 1.16

replace github.com/fluxcd/image-automation-controller/api => ./api

require (
	github.com/Masterminds/sprig/v3 v3.2.2
	github.com/ProtonMail/go-crypto v0.0.0-20210428141323-04723f9f07d7
	github.com/cyphar/filepath-securejoin v0.2.2
	github.com/fluxcd/image-automation-controller/api v0.18.0
	// If you bump this, change REFLECTOR_VER in the Makefile to match
	github.com/fluxcd/image-reflector-controller/api v0.14.0
	github.com/fluxcd/pkg/apis/meta v0.10.1
	github.com/fluxcd/pkg/gittestserver v0.4.2
	github.com/fluxcd/pkg/runtime v0.12.1
	github.com/fluxcd/pkg/ssh v0.1.0
	// If you bump this, change SOURCE_VER in the Makefile to match
	github.com/fluxcd/source-controller v0.19.0
	github.com/fluxcd/source-controller/api v0.19.0
	github.com/go-git/go-billy/v5 v5.3.1
	github.com/go-git/go-git/v5 v5.4.2
	github.com/go-logr/logr v0.4.0
	github.com/google/go-containerregistry v0.6.0
	github.com/libgit2/git2go/v31 v31.6.1
	github.com/onsi/ginkgo v1.16.4
	github.com/onsi/gomega v1.15.0
	github.com/otiai10/copy v1.7.0
	github.com/spf13/pflag v1.0.5
	k8s.io/api v0.22.2
	k8s.io/apimachinery v0.22.2
	k8s.io/client-go v0.22.2
	k8s.io/kube-openapi v0.0.0-20210421082810-95288971da7e
	sigs.k8s.io/controller-runtime v0.10.2
	sigs.k8s.io/kustomize/kyaml v0.10.21
)

// Side-effect of depending on source-controller;
// required by https://github.com/helm/helm/blob/v3.6.3/go.mod,
// but overwritten with a newer version due to CVE-2017-11468.
replace github.com/docker/distribution => github.com/docker/distribution v2.7.0-rc.0+incompatible
