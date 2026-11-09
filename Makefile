GO            ?= go
GOLANGCI_LINT ?= golangci-lint
# The golangci-lint version of make lint and of the CI (which reads it).
GOLANGCI_LINT_VERSION := v2.15.0
BIN_DIR       ?= bin
PKG           := github.com/kentrow/tapyr

# SemVer without the "v" prefix, from the closest vX.Y.Z tag.
VERSION ?= $(shell git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null | sed 's/^v//')
ifeq ($(strip $(VERSION)),)
VERSION := 0.0.0-dev
endif
REVISION ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
LDFLAGS  := -s -w \
	-X $(PKG)/internal/buildinfo.version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.revision=$(REVISION)

PROVIDER ?= example-sim
CONFIG   ?= ./examples/config

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-22s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ui build-go ## Build the web UI, then the static tapyr binary into ./bin

.PHONY: build-go
build-go: ## Build the static tapyr binary into ./bin, embedding the UI built last (if any)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/tapyr ./cmd/tapyr

.PHONY: test
test: ## Run unit tests with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: cover
cover: ## Run unit tests with coverage (coverage.out)
	$(GO) test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Run golangci-lint and repository checks
	@v="$$($(GOLANGCI_LINT) version --short 2>/dev/null)"; [ "v$$v" = "$(GOLANGCI_LINT_VERSION)" ] || \
		{ echo "golangci-lint $(GOLANGCI_LINT_VERSION) is required (found $${v:-none}): https://golangci-lint.run/welcome/install/" >&2; exit 1; }
	$(GOLANGCI_LINT) run
	./scripts/check-emdash.sh
	$(GO) run ./internal/tools/docsite check
	$(GO) run ./cmd/tapyr provider lint --catalog providers

.PHONY: docs-check
docs-check: ## Check the documentation site manifest and the links of the Markdown files (part of make lint)
	$(GO) run ./internal/tools/docsite check

# Fuzz targets, as package:function.
FUZZ_TARGETS := config:FuzzParseDocuments collect:FuzzPagination mapping:FuzzExtract sim:FuzzAnonymizer
FUZZTIME ?= 30s

.PHONY: fuzz
fuzz: ## Run every fuzz target for FUZZTIME (default 30s); a failing input lands in testdata/fuzz of its package
	@for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; fn=$${t#*:}; \
		echo "== $$fn ($$pkg, $(FUZZTIME))"; \
		$(GO) test -run '^$$' -fuzz "^$$fn\$$" -fuzztime $(FUZZTIME) ./internal/$$pkg/ || exit 1; \
	done

.PHONY: providers-update
providers-update: ## Rewrite the expected samples of the built-in provider contract tests
	$(GO) run ./cmd/tapyr provider lint --update providers

.PHONY: golangci-lint-version
golangci-lint-version: ## Print the golangci-lint version that make lint and the CI use
	@echo $(GOLANGCI_LINT_VERSION)

.PHONY: fmt
fmt: ## Format Go code
	$(GOLANGCI_LINT) fmt

.PHONY: schema
schema: ## Regenerate the JSON Schemas in ./schemas and the configuration reference in ./docs/reference from the Go types
	$(GO) run ./internal/tools/schemagen -out schemas/v1alpha1 -docs docs/reference

.PHONY: schema-check
schema-check: schema ## Fail if the committed JSON Schemas or the configuration reference are out of date
	@if [ -n "$$(git status --porcelain -- schemas docs/reference)" ]; then \
		git status --short -- schemas docs/reference; \
		echo "schemas or configuration reference out of date: run make schema and commit the result" >&2; exit 1; \
	fi

.PHONY: tidy-check
tidy-check: ## Fail if go.mod or go.sum are not tidy
	$(GO) mod tidy -diff

.PHONY: run
run: ## Run tapyr against ./examples/config (start make sim first)
	TAPYR_EXAMPLE_API_KEY=$${TAPYR_EXAMPLE_API_KEY:-sim-api-key} $(GO) run ./cmd/tapyr run --config $(CONFIG)

.PHONY: smoke
smoke: build-go ## Smoke test the built binary (tapyr sim + tapyr run)
	./scripts/smoke.sh $(BIN_DIR)/tapyr

.PHONY: sim
sim: ## Run the simulator for a provider (PROVIDER=name)
	$(GO) run ./cmd/tapyr sim --provider $(PROVIDER)

.PHONY: e2e
e2e: ## Run opt-in end-to-end tests against real providers
	$(GO) test -race -count=1 -tags e2e ./test/e2e/...

INTEGRATION_COMPOSE := docker compose -f test/integration/compose.yaml

.PHONY: integration
integration: ## Run the output integration tests against VictoriaMetrics, the OpenTelemetry Collector and Prometheus (Docker)
	mkdir -p test/integration/out && chmod 0777 test/integration/out && rm -f test/integration/out/metrics.json
	$(INTEGRATION_COMPOSE) up -d --wait && \
		$(GO) test -count=1 -tags integration -v ./test/integration/ ; status=$$?; \
		$(INTEGRATION_COMPOSE) down -v; exit $$status

# The Prometheus image of promtool (not watched by Dependabot: update it with
# the Prometheus releases).
PROMTOOL := docker run --rm -v "$(CURDIR)/examples/alerts:/rules:ro" -w /rules --entrypoint promtool prom/prometheus:v3.15.1

.PHONY: alerts-check
alerts-check: ## Check and unit test the example alert rules with promtool (Docker)
	$(PROMTOOL) check rules tapyr-alerts.yaml
	$(PROMTOOL) test rules tapyr-alerts.test.yaml

.PHONY: docker
docker: ## Build the tapyr image from source (tapyr:dev)
	docker build -t tapyr:dev --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) .

.PHONY: licenses
licenses: ## Write THIRD_PARTY_LICENSES (Go modules, Go, UI packages; run make ui first)
	$(GO) run ./internal/tools/licenses -ui ui/dist/licenses.txt -out THIRD_PARTY_LICENSES

.PHONY: ui
ui: ## Build the web UI into ui/dist/app (Node.js 22.12 or later), embedded by the next Go build
	cd ui && npm ci --no-audit --no-fund && npm run build

.PHONY: ui-check
ui-check: ## Type-check, lint and test the web UI
	cd ui && npm ci --no-audit --no-fund && npm run typecheck && npm run lint && npm test

.PHONY: ui-dev
ui-dev: ## Serve the UI with hot reload, proxying /api to tapyr run (TAPYR_URL, default http://127.0.0.1:9790)
	cd ui && npm run dev

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) dist coverage.out coverage.html tapyr THIRD_PARTY_LICENSES \
		ui/dist/app ui/dist/licenses.txt ui/*.tsbuildinfo test/integration/out
