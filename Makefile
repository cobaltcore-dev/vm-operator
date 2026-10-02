# Copyright SAP SE
# SPDX-License-Identifier: Apache-2.0

# Phases run by `make all`, in order. Each is a normal target below.
PHASES ?= ctrlgen lint test crd-test

.PHONY: all
all: ## Run all phases (ctrlgen, lint, test, crd-test), quietly unless one fails.
	@rc=0; \
	for phase in $(PHASES); do \
	  printf '\033[1;34m▶ %-10s\033[0m ' "$$phase"; \
	  log="$$(mktemp)"; \
	  if $(MAKE) --no-print-directory "$$phase" >"$$log" 2>&1; then \
	    printf '\033[1;32mok\033[0m\n'; \
	  else \
	    printf '\033[1;31mFAILED\033[0m\n'; \
	    cat "$$log"; \
	    rc=1; \
	    rm -f "$$log"; \
	    break; \
	  fi; \
	  rm -f "$$log"; \
	done; \
	exit $$rc

.PHONY: test
test: ## Run the Go unit tests.
	go test ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint and apply fixes.
	$(GOLANGCI_LINT) run --fix

.PHONY: ctrlgen
ctrlgen: controller-gen
	$(CONTROLLER_GEN) crd:allowDangerousTypes=true paths="./..." \
	output:crd:artifacts:config=dist/files/crds \
	object:headerFile="hack/boilerplate.go.txt"

CRD ?= dist/files/crds/cobaltcore.cloud_virtualmachines.yaml
FIXTURES ?= test/crd/fixtures

.PHONY: crd-test
crd-test: kind ## Validate the generated CRD against valid/invalid VM manifests on a kind apiserver.
	@set -eu; \
	$(KIND) create cluster --name $(KIND_CLUSTER) >/dev/null; \
	trap '$(KIND) delete cluster --name $(KIND_CLUSTER) >/dev/null 2>&1' EXIT; \
	$(KIND) export kubeconfig --name $(KIND_CLUSTER) >/dev/null; \
	kubectl apply --server-side -f $(CRD) >/dev/null; \
	kubectl wait --for=condition=established --timeout=60s crd/virtualmachines.cobaltcore.cloud >/dev/null; \
	rc=0; \
	for f in $(FIXTURES)/valid/*.yaml; do \
	  if kubectl apply --server-side --dry-run=server -f "$$f" >/dev/null 2>&1; \
	    then echo "PASS (accepted) $$f"; else echo "FAIL (rejected) $$f"; rc=1; fi; \
	done; \
	for f in $(FIXTURES)/invalid/*.yaml; do \
	  if kubectl apply --server-side --dry-run=server -f "$$f" >/dev/null 2>&1; \
	    then echo "FAIL (accepted) $$f"; rc=1; else echo "PASS (rejected) $$f"; fi; \
	done; \
	exit $$rc


LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
KIND ?= $(LOCALBIN)/kind
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

CONTROLLER_TOOLS_VERSION ?= v0.21.0
KIND_VERSION ?= v0.32.0
GOLANGCI_LINT_VERSION ?= v2.14.0
KIND_CLUSTER ?= vm-crd-test

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind/cmd/kind,$(KIND_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

