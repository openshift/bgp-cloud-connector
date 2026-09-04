# VERSION defines the project version for the bundle.
# It is read from the VERSION file by default.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
VERSION := $(shell cat VERSION)

CHANNELS ?= stable
DEFAULT_CHANNEL ?= stable
BUNDLE_CHANNELS := --channels=$(CHANNELS)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# openshift.io/bgp-cloud-connector-bundle:$VERSION and openshift.io/bgp-cloud-connector-catalog:$VERSION.
IMAGE_TAG_BASE ?= openshift.io/bgp-cloud-connector

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)

# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

CATALOG_DIR := catalog
OCP_VERSION ?= 4.22
OCP_CATALOG_DIR := $(CATALOG_DIR)/v$(OCP_VERSION)

# Set the Operator SDK version to use. By default, what is installed on the system is used.
# This is useful for CI or a project to utilize a specific version of the operator-sdk toolkit.
OPERATOR_SDK_VERSION ?= v1.42.2
# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Architectures for local multi-arch image builds (image-build, manifest-build).
MULTIARCH_TARGETS ?= amd64

ifeq ("$(CONTAINER_TOOL)","docker")
EXTRA_BUILD_FLAGS ?= --provenance=false
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: vendor
vendor: ## Tidy go.mod/go.sum and refresh the vendor directory.
	go mod tidy
	go mod vendor
	go mod verify

.PHONY: verify-vendor
verify-vendor: vendor ## Fail if go.mod, go.sum or vendor/ are out of date.
	git diff --exit-code --name-only go.mod go.sum vendor/

.PHONY: verify-version
verify-version: set-version ## Fail if Containerfile labels are out of sync with VERSION.
	git diff --exit-code --name-only Containerfile.bgp-cloud-connector Containerfile.bgp-cloud-connector-bundle

.PHONY: verify-bundle
verify-bundle: bundle ## Fail if bundle/ content is out of date.
	git diff --exit-code -I'createdAt' --name-only bundle/
	test -z "$$(git ls-files --others --exclude-standard -- bundle/)"

.PHONY: verify
verify: verify-vendor verify-version verify-bundle ## Run all verification checks.

.PHONY: install-git-hooks
install-git-hooks: ## Run the tracked hooks in hack/githooks, including a pre-push vendor check.
	git config core.hooksPath hack/githooks

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet ## Run platform-independent unit tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./internal/controller/... ./api/... ./cmd/... -coverprofile cover.out
	$(MAKE) test-scripts

.PHONY: test-scripts
test-scripts: ## Run unit tests for the shell under hack/.
	./hack/lib-test.sh

##@ CI jobs

# The prow jobs, runnable here. Named ci- rather than e2e- to keep them
# apart from test-e2e-aws above, which runs the Go suite against a
# cluster somebody else configured; these run the whole job, including
# standing that cluster's estate up and tearing it down. One target per
# platform, each depending on the CLI its scripts need, so Azure and GCP
# slot in beside this as their scripts land.
.PHONY: ci-e2e-aws
ci-e2e-aws: bin/aws ## Run the AWS e2e job: stand the estate up, then tear it down.
	./hack/ci-e2e-aws.sh

.PHONY: ci-e2e-aws-teardown
ci-e2e-aws-teardown: bin/aws ## Remove whatever an AWS e2e run left behind.
	./hack/ci-e2e-aws-teardown.sh

# A file target, so once something has been downloaded there is nothing
# left to do. The script decides whether to download at all: with a new
# enough aws already on PATH it installs nothing and this file is never
# created, so on a machine that packages the CLI the rule re-runs every
# time and costs a version check. That is the intended outcome, not an
# oversight -- the archive AWS ships is linked for a generic Linux and
# would not run here anyway.
bin/aws: | $(LOCALBIN)
	./hack/aws/ensure-cli.sh $(LOCALBIN) >/dev/null

.PHONY: test-aws
test-aws: ## Run AWS platform unit tests (mocked, no credentials needed).
	go test ./internal/platform/aws/... -v -count=1

.PHONY: test-e2e
test-e2e: ## Run shared e2e tests (requires cluster + external BGP peer). Usage: make test-e2e <profile>
	$(eval E2E_PROFILE := $(filter-out $@,$(MAKECMDGOALS)))
	@[ -n "$(E2E_PROFILE)$(E2E_MANIFEST_DIR)" ] || { echo "Usage: make test-e2e <profile-name>, or set E2E_MANIFEST_DIR"; exit 1; }
	E2E_PROFILE=$(E2E_PROFILE) go test ./test/e2e/ -v -timeout 30m -count=1

.PHONY: test-e2e-aws
test-e2e-aws: ## Run AWS e2e tests (requires cluster + IRSA configured). Usage: make test-e2e-aws <profile>
	$(eval E2E_PROFILE := $(filter-out $@,$(MAKECMDGOALS)))
	@[ -n "$(E2E_PROFILE)$(E2E_MANIFEST_DIR)" ] || { echo "Usage: make test-e2e-aws <profile-name>, or set E2E_MANIFEST_DIR"; exit 1; }
	E2E_PROFILE=$(E2E_PROFILE) go test ./test/e2e/aws/ -v -timeout 60m -count=1

.PHONY: lint
lint: ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: set-version
set-version: ## Sync VERSION file to all Containerfile labels.
	@./hack/sync-version.sh

.PHONY: build-operator
build-operator: ## Build manager binary, no additional checks or code generation.
	go build -tags strictfipsruntime -o bin/manager cmd/main.go

.PHONY: build
build: manifests generate fmt vet build-operator ## Build manager binary.

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go --zap-devel

##@ Images

# Build a single arch target image: $(call build_target,<arch>)
define build_target
	echo 'building image for arch $(1)'; \
	if [ "$(CONTAINER_TOOL)" = "docker" ]; then \
		$(CONTAINER_TOOL) buildx build --load --platform=linux/$(1) --build-arg TARGETARCH=$(1) ${EXTRA_BUILD_FLAGS} -t ${IMG}-$(1) -f Dockerfile .; \
	else \
		$(CONTAINER_TOOL) build --platform=linux/$(1) --build-arg TARGETARCH=$(1) ${EXTRA_BUILD_FLAGS} -t ${IMG}-$(1) -f Dockerfile .; \
	fi;
endef

# Push a single arch target image: $(call push_target,<arch>)
define push_target
	echo 'pushing image ${IMG}-$(1)'; \
	$(CONTAINER_TOOL) push ${IMG}-$(1);
endef

# Add a single arch target to a manifest list: $(call manifest_add_target,<arch>)
define manifest_add_target
	echo 'manifest add target $(1)'; \
	$(CONTAINER_TOOL) manifest add ${IMG} ${IMG}-$(1);
endef

.PHONY: image-build
image-build: ## Build MULTIARCH_TARGETS images.
	trap 'exit' INT; \
	$(foreach target,$(MULTIARCH_TARGETS),$(call build_target,$(target)))

.PHONY: image-push
image-push: ## Push MULTIARCH_TARGETS images.
	trap 'exit' INT; \
	$(foreach target,$(MULTIARCH_TARGETS),$(call push_target,$(target)))

.PHONY: manifest-build
manifest-build: ## Build MULTIARCH_TARGETS manifest list.
	@echo 'building manifest $(IMG)'
	$(CONTAINER_TOOL) rmi ${IMG} -f 2>/dev/null || true
	$(CONTAINER_TOOL) manifest create ${IMG} $(foreach target,$(MULTIARCH_TARGETS), --amend ${IMG}-$(target));

.PHONY: manifest-push
manifest-push: ## Push MULTIARCH_TARGETS manifest list.
	@echo 'pushing manifest $(IMG)'
ifeq (${CONTAINER_TOOL}, docker)
	$(CONTAINER_TOOL) manifest push ${IMG};
else
	$(CONTAINER_TOOL) manifest push ${IMG} docker://${IMG};
endif


.PHONY: build-installer
build-installer: manifests generate ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
# kubectl when it is installed, otherwise oc. The image the CI jobs run
# in carries oc, from `cli: latest`, and no kubectl at all, so a bare
# kubectl default fails there -- and it fails after kustomize has
# rendered the manifests, so it reads like a kustomize problem rather
# than a missing client. oc is a superset for the apply and delete these
# targets do. Falls back to the name itself so that a machine with
# neither still says "kubectl: command not found" rather than running an
# empty command. Override with KUBECTL=<path> as before.
KUBECTL ?= $(shell command -v kubectl 2>/dev/null || command -v oc 2>/dev/null || echo kubectl)
KUSTOMIZE ?= go tool kustomize
CONTROLLER_GEN ?= go tool controller-gen
ENVTEST ?= go tool setup-envtest
GOLANGCI_LINT ?= go tool golangci-lint

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')

.PHONY: operator-sdk
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif

.PHONY: bundle
bundle: manifests operator-sdk ## Generate bundle manifests and metadata, then validate generated files.
	rm -rf bundle/
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: bundle-build
bundle-build: bundle ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(CONTAINER_TOOL) push $(BUNDLE_IMG)

BUNDLE_NAMESPACE ?= openshift-bgp-cloud-connector
BUNDLE_RUN_FLAGS ?= --namespace=$(BUNDLE_NAMESPACE) --install-mode=OwnNamespace
BUNDLE_CLEANUP_FLAGS ?= --namespace=$(BUNDLE_NAMESPACE)

.PHONY: bundle-run
bundle-run: operator-sdk ## Deploy the operator from the bundle image using operator-sdk run bundle.
	$(KUBECTL) create namespace $(BUNDLE_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) label namespace $(BUNDLE_NAMESPACE) openshift.io/cluster-monitoring=true --overwrite
	$(OPERATOR_SDK) run bundle $(BUNDLE_IMG) $(BUNDLE_RUN_FLAGS)

.PHONY: bundle-clean
bundle-clean: operator-sdk ## Remove the operator deployed via bundle-run.
	$(OPERATOR_SDK) cleanup bgp-cloud-connector $(BUNDLE_CLEANUP_FLAGS)

.PHONY: opm
OPM = $(LOCALBIN)/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.55.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

.PHONY: generate-catalog
generate-catalog: opm ## Generate OCP version-based FBC catalog from template.
	mkdir -p $(OCP_CATALOG_DIR)
	$(OPM) alpha render-template basic --migrate-level bundle-object-to-csv-metadata -o yaml $(OCP_CATALOG_DIR)/catalog-template.yaml > $(OCP_CATALOG_DIR)/catalog.yaml

# The bundle image to include in the catalog (must exist in a registry and be pull-able).
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

CATALOG_BUILD_DIR := catalog/dev

.PHONY: catalog-build
catalog-build: opm ## Build an FBC catalog image from the bundle image.
	rm -rf $(CATALOG_BUILD_DIR)
	mkdir -p $(CATALOG_BUILD_DIR)
	$(OPM) render $(BUNDLE_IMGS) -o yaml > $(CATALOG_BUILD_DIR)/render.yaml
	@echo '---' >> $(CATALOG_BUILD_DIR)/render.yaml
	@printf 'schema: olm.package\nname: bgp-cloud-connector\ndefaultChannel: %s\n' "$(DEFAULT_CHANNEL)" >> $(CATALOG_BUILD_DIR)/render.yaml
	@echo '---' >> $(CATALOG_BUILD_DIR)/render.yaml
	@printf 'schema: olm.channel\nname: %s\npackage: bgp-cloud-connector\nentries:\n  - name: bgp-cloud-connector.v%s\n' "$(DEFAULT_CHANNEL)" "$(VERSION)" >> $(CATALOG_BUILD_DIR)/render.yaml
	$(OPM) validate $(CATALOG_BUILD_DIR)
	$(CONTAINER_TOOL) build --build-arg CATALOG_PATH=$(CATALOG_BUILD_DIR) -f catalog.Dockerfile -t $(CATALOG_IMG) .

.PHONY: catalog-push
catalog-push: ## Push the catalog image.
	$(CONTAINER_TOOL) push $(CATALOG_IMG)

.PHONY: catalog-deploy
catalog-deploy: ## Deploy a CatalogSource pointing to the catalog image.
	sed -e 's~<IMAGE>~$(CATALOG_IMG)~' ./config/samples/catalog/catalog.yaml | $(KUBECTL) apply -f -

.PHONY: catalog-undeploy
catalog-undeploy: ## Remove the dev CatalogSource.
	$(KUBECTL) delete -f ./config/samples/catalog/catalog.yaml --ignore-not-found

include .mk/sample.mk
include .mk/shortcuts.mk

# Catch-all so positional args (e.g. make test-e2e-aws my-cluster) don't error.
%:
	@:
