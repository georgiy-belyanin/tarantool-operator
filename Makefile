# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 1.0.0-rc1

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "candidate,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=candidate,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="candidate,fast,stable")
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# tarantool.io/tarantool-operator-ce-bundle:$VERSION and tarantool.io/tarantool-operator-ce-catalog:$VERSION.
IMAGE_TAG_BASE ?= tarantool.io/tarantool-operator-ce

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

# Image URL to use all building/pushing image targets
REPO ?= tarantool/tarantool-operator
IMG ?= ${REPO}:${VERSION}
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.31.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
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
# target descriptions by '##'. The awk commands is responsible for reading the
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
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	# Scope to the marker-bearing dirs; a ./... glob descends into a local
	# tarantool/ source checkout whose vendored Go has unfetchable deps.
	$(CONTROLLER_GEN) rbac:roleName=manager-role webhook paths="./apis/..." paths="./controllers/..."
	# CRDs are emitted into per-API subdirectories so config/crd/bases mirrors the
	# code layout: cartridge/ (legacy tarantool.io) and tarantool3/ (db.tarantool.io).
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true,maxDescLen=0 \
		paths="./apis/cartridge/..." output:crd:artifacts:config=config/crd/bases/cartridge
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true,maxDescLen=0 \
		paths="./apis/v2alpha1/..." output:crd:artifacts:config=config/crd/bases/tarantool3

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./apis/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

lint: ## Lint the code
	golangci-lint run -v --fix

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test ./... -coverprofile cover.out

.PHONY: verify
verify: manifests generate ## Fail if generated manifests/code are not up to date (CI guard).
	@git diff --exit-code -- config/crd config/rbac ':(glob)**/zz_generated.deepcopy.go' || { echo "ERROR: generated files are out of date — run 'make manifests generate' and commit."; exit 1; }

.PHONY: test-e2e
test-e2e: ## Run the kind-based end-to-end smoke test (needs kind + docker). Set KEEP=1 to keep the cluster.
	./test/e2e/kind-e2e.sh

.PHONY: test-e2e-scaling
test-e2e-scaling: ## Run the kind-based scale up/down end-to-end test (needs kind + docker).
	./test/e2e/kind-scaling.sh

.PHONY: test-e2e-config
test-e2e-config: ## Run the kind-based config-propagation end-to-end test (needs kind + docker).
	./test/e2e/kind-config.sh

.PHONY: test-e2e-badconfig
test-e2e-badconfig: ## Run the kind-based bad-config e2e (config-first contract: typo rendered verbatim, CrashLoop not masked, recovers on fix; needs kind + docker).
	./test/e2e/kind-badconfig.sh

.PHONY: test-e2e-large
test-e2e-large: ## Run the kind-based large-topology end-to-end test (~10 instances; needs kind + docker).
	./test/e2e/kind-large.sh

.PHONY: test-e2e-luaapp
test-e2e-luaapp: ## Run the kind-based user-Lua-application end-to-end test (needs kind + docker).
	./test/e2e/kind-luaapp.sh

.PHONY: test-e2e-roles
test-e2e-roles: ## Run the kind-based application-roles end-to-end test (needs kind + docker).
	./test/e2e/kind-roles.sh

.PHONY: test-e2e-deliver-role
test-e2e-deliver-role: ## Run the kind-based deliver-role e2e (operator in-cluster; ./deliver-role ships + enables a role, hot-reloaded with no restart; needs kind + docker + python3).
	./test/e2e/kind-deliver-role.sh

.PHONY: test-e2e-stack
test-e2e-stack: ## Run the kind-based full-stack e2e (large replicated cluster + role + Lua app; needs kind + docker).
	./test/e2e/kind-stack.sh

.PHONY: test-e2e-leader
test-e2e-leader: ## Run the kind-based leader-observation e2e (operator in-cluster; election failover; needs kind + docker).
	./test/e2e/kind-leader.sh

.PHONY: test-e2e-reload
test-e2e-reload: ## Run the kind-based config hot-reload e2e (operator in-cluster; dynamic config change without restart; needs kind + docker).
	./test/e2e/kind-reload.sh

.PHONY: test-e2e-persistence
test-e2e-persistence: ## Run the kind-based data-persistence e2e (operator in-cluster; WAL replay, rollout, CR recreate, expel round trip; needs kind + docker).
	./test/e2e/kind-persistence.sh

.PHONY: test-e2e-helm
test-e2e-helm: ## Run the kind-based Helm chart e2e (install, operate, hot reload, upgrade, uninstall; needs kind + helm + docker).
	./test/e2e/kind-helm.sh

.PHONY: test-e2e-nodeloss
test-e2e-nodeloss: ## Run the kind-based dead-node remediation e2e (multi-node kind; force-reschedule + rejoin; needs kind + docker).
	./test/e2e/kind-nodeloss.sh

.PHONY: test-e2e-degraded
test-e2e-degraded: ## Run the kind-based Degraded-phase e2e (stuck TX thread: Running-but-NotReady instance => Degraded; needs kind + docker).
	./test/e2e/kind-degraded.sh

.PHONY: test-e2e-rolling
test-e2e-rolling: ## Run the kind-based rolling-update-under-load e2e (3.6 -> 3.7: no read downtime, no acked write lost; needs kind + docker).
	./test/e2e/kind-rolling.sh


.PHONY: test-e2e-rebalance
test-e2e-rebalance: ## Run the kind-based sharding scale-out/in e2e (add storage replica set -> vshard rebalance -> drain weight 0 -> remove; needs kind + docker).
	./test/e2e/kind-rebalance.sh

.PHONY: test-e2e-load
test-e2e-load: ## Run the long-lasting load+churn e2e (continuous routed load while scaling/rebalancing; no transaction lost; LOAD_CYCLES=N; needs kind + docker).
	./test/e2e/kind-load.sh

.PHONY: test-e2e-coexist
test-e2e-coexist: ## Run the coexistence e2e (Cartridge + Tarantool 3 operators side by side, same kv app on both; needs kind + docker).
	./test/e2e/kind-coexist.sh

.PHONY: test-e2e-upgrade
test-e2e-upgrade: ## Run the kind-based binary-upgrade e2e (operator in-cluster; 3.6->3.7 + schema upgrade; needs kind + docker).
	./test/e2e/kind-upgrade.sh

.PHONY: test-e2e-samples
test-e2e-samples: ## Render-validate all samples and runtime-test the community-image-runnable ones on kind (needs kind + docker).
	./test/e2e/kind-samples.sh

##@ Build

.PHONY: build
build: generate fmt vet ## Build manager binary.
	go build -o bin/manager main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./main.go

.PHONY: docker-build
docker-build: test ## Build docker image with the manager.
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG}

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest

## Tool Versions
KUSTOMIZE_VERSION ?= v3.8.7
CONTROLLER_TOOLS_VERSION ?= v0.21.0

KUSTOMIZE_INSTALL_SCRIPT ?= "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh"
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	test -s $(LOCALBIN)/kustomize || { curl -s $(KUSTOMIZE_INSTALL_SCRIPT) | bash -s -- $(subst v,,$(KUSTOMIZE_VERSION)) $(LOCALBIN); }

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	test -s $(LOCALBIN)/setup-envtest || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

.PHONY: bundle
bundle: manifests kustomize ## Generate bundle manifests and metadata, then validate generated files.
	operator-sdk generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/manifests | operator-sdk generate bundle $(BUNDLE_GEN_FLAGS)
	operator-sdk bundle validate ./bundle

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	docker build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) docker-push IMG=$(BUNDLE_IMG)

.PHONY: opm
OPM = ./bin/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.23.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

# A comma-separated list of bundle images (e.g. make catalog-build BUNDLE_IMGS=example.com/operator-bundle:v0.1.0,example.com/operator-bundle:v0.2.0).
# These images MUST exist in a registry and be pull-able.
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

# Build a catalog image by adding bundle images to an empty catalog using the operator package manager tool, 'opm'.
# This recipe invokes 'opm' in 'semver' bundle add mode. For more information on add modes, see:
# https://github.com/operator-framework/community-operators/blob/7f1438c/docs/packaging-operator.md#updating-your-existing-operator
.PHONY: catalog-build
catalog-build: opm ## Build a catalog image.
	$(OPM) index add --container-tool docker --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT)

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	$(MAKE) docker-push IMG=$(CATALOG_IMG)

# Kustomize
KUSTOMIZE_DIR = $(shell pwd)/.kustomize
kustomize-warn:
	@echo -e "$(CYELLOW)WARN$(CEND): After building, the manifests will be in:"
	@echo -e "*~*~* $(KUSTOMIZE_DIR)"
	@echo -e "*~*~* but $(CYELLOW)this is not a helm template yet and they need to be migrated to helm-charts.$(CEND)"
	@echo

kustomize-crds: kustomize-warn manifests generate kustomize ## Build rbac manifests for the helm chart using Kustomize.
	@mkdir -p $(KUSTOMIZE_DIR)/crds/
	$(KUSTOMIZE) build config/crd -o $(KUSTOMIZE_DIR)/crds/

kustomize-rbac: kustomize-warn manifests generate kustomize ## Build rbac manifests for the helm chart using Kustomize.
	@mkdir -p $(KUSTOMIZE_DIR)/rbac/
	$(KUSTOMIZE) build config/rbac -o $(KUSTOMIZE_DIR)/rbac/