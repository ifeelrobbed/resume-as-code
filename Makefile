# Local versions of the checks the CI workflows run, plus the ad-hoc
# verification steps (dry-run against the live cluster, a local container
# smoke test) that don't have a CI equivalent but are worth running before
# opening a PR. `make check` mirrors app-ci.yml + manifests-ci.yml exactly -
# if it passes here, CI should pass too.

SHELL := /bin/bash
.DEFAULT_GOAL := help

APP_DIR := app
# Two lists, not one: yamllint checks everything that is YAML, but kubeconform
# validates against Kubernetes schemas and so must only see actual manifests.
# promql/tests holds promtool unit tests - valid YAML, not Kubernetes objects.
YAMLLINT_DIRS := manifests promql
KUBECONFORM_DIRS := manifests promql/rules
BIN_DIR := .bin
KUBECONFORM := $(BIN_DIR)/kubeconform
# promtool needs a plain rules file (groups: at the top level), but the rules
# are committed as PrometheusRule CRs so Argo CD can apply them. Unwrap .spec
# into here; generated, gitignored, referenced by promql/tests/*.yaml.
PROMQL_RULES_OUT := $(BIN_DIR)/promql-rules
YAMLLINT_CONFIG := {extends: default, rules: {line-length: disable, document-start: disable}}
DOCKER_IMAGE := resume-site-local
DOCKER_PORT := 8081
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GIT_REVISION := $(shell git rev-parse --short=7 HEAD)

.PHONY: help fmt vet build test app-check yamllint kubeconform promql-test manifests-check \
	dry-run docker-build docker-run docker-test docker-stop docker-clean run check

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*##"}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## --- Go app (mirrors .github/workflows/app-ci.yml) ---

fmt: ## List any Go files gofmt would reformat (CI doesn't gate on this, but keep it clean)
	cd $(APP_DIR) && gofmt -l .

vet: ## go vet ./...
	cd $(APP_DIR) && go vet ./...

build: ## go build ./...
	cd $(APP_DIR) && go build ./...

test: ## go test ./...
	cd $(APP_DIR) && go test ./...

app-check: fmt vet build test ## Run all Go checks (fmt, vet, build, test)

## --- Manifests / promql (mirrors .github/workflows/manifests-ci.yml) ---

yamllint: ## Lint manifests/ and promql/ with yamllint
	@command -v yamllint >/dev/null || pip install --quiet yamllint
	yamllint -d "$(YAMLLINT_CONFIG)" $(YAMLLINT_DIRS)

$(KUBECONFORM):
	@mkdir -p $(BIN_DIR)
	@os=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	arch=$$(uname -m); \
	case $$arch in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; esac; \
	echo "Downloading kubeconform ($$os-$$arch) to $(BIN_DIR)/..."; \
	curl -sSL "https://github.com/yannh/kubeconform/releases/latest/download/kubeconform-$$os-$$arch.tar.gz" \
		| tar xz -C $(BIN_DIR) kubeconform

kubeconform: $(KUBECONFORM) ## Validate manifests/ and promql/ against Kubernetes schemas (downloads the binary to .bin/ once)
	$(KUBECONFORM) -strict -summary \
		-schema-location default \
		-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
		-ignore-missing-schemas \
		$(KUBECONFORM_DIRS)

promql-test: ## Unit-test the alerting rules in promql/rules with promtool
	@command -v promtool >/dev/null || { echo "promtool not on PATH - install prometheus tooling"; exit 1; }
	@rm -rf $(PROMQL_RULES_OUT) && mkdir -p $(PROMQL_RULES_OUT)
	@for f in promql/rules/*.yaml; do \
		python3 -c "import sys,yaml; yaml.safe_dump(yaml.safe_load(open(sys.argv[1]))['spec'], open(sys.argv[2],'w'))" \
			"$$f" "$(PROMQL_RULES_OUT)/$$(basename $$f)"; \
	done
	promtool check rules $(PROMQL_RULES_OUT)/*.yaml
	promtool test rules promql/tests/*.yaml

manifests-check: yamllint kubeconform promql-test ## Run all manifests/promql checks (yamllint, kubeconform, promtool)

## --- Live cluster (uses your current kubeconfig context - read-only/dry-run only) ---

dry-run: ## Server-side dry-run a manifest against the live cluster: make dry-run FILE=path/to/file.yaml
	@if [ -z "$(FILE)" ]; then echo "Usage: make dry-run FILE=path/to/file.yaml"; exit 1; fi
	kubectl apply --dry-run=server -f $(FILE)

## --- Docker (mirrors app-ci.yml's image build args) ---

docker-build: ## Build the app image locally, with the same build-args CI uses
	cd $(APP_DIR) && docker build \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		--build-arg GIT_REVISION=$(GIT_REVISION) \
		-t $(DOCKER_IMAGE) .

docker-run: docker-build ## Build and run the app image locally in the background
	@docker rm -f $(DOCKER_IMAGE) >/dev/null 2>&1 || true
	docker run --rm -d -p $(DOCKER_PORT):8080 --name $(DOCKER_IMAGE) $(DOCKER_IMAGE)
	@sleep 1
	@echo "Running at http://localhost:$(DOCKER_PORT) - try: curl http://localhost:$(DOCKER_PORT)/metrics"

docker-test: docker-run ## Build, run, smoke-test every route, and tear down - the full loop to run before opening an app/ PR
	@sleep 1
	curl -sf http://localhost:$(DOCKER_PORT)/ -o /dev/null && echo "GET /        OK"
	curl -sf http://localhost:$(DOCKER_PORT)/resume -o /dev/null && echo "GET /resume  OK"
	curl -sf http://localhost:$(DOCKER_PORT)/status -o /dev/null && echo "GET /status  OK"
	curl -sf http://localhost:$(DOCKER_PORT)/metrics -o /dev/null && echo "GET /metrics OK"
	$(MAKE) docker-stop

docker-stop: ## Stop the local test container
	-docker stop $(DOCKER_IMAGE) >/dev/null 2>&1

docker-clean: docker-stop ## Stop the container and remove the local test image
	-docker rmi $(DOCKER_IMAGE) >/dev/null 2>&1

## --- Local dev ---

run: ## go run . locally (no Docker) on :8080
	cd $(APP_DIR) && go run .

## --- Everything ---

check: app-check manifests-check ## Run every check (Go + manifests) - the fast pre-PR pass. Run docker-test too if app/ changed.
