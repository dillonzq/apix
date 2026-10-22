# Put flags in SHELL for macOS GNU Make 3.81, which lacks .SHELLFLAGS.
SHELL := /bin/bash -eu -o pipefail
.DEFAULT_GOAL := help

GO ?= go
GOLANGCI_LINT ?= golangci-lint
ACTIONLINT_VERSION := v1.7.12
GOVULNCHECK_VERSION := v1.9.0
FUZZTIME ?= 30s

# Avoid silently repairing dependencies during builds and tests. go mod tidy
# still owns module-file validation and reports changes through -diff.
export GOFLAGS := -mod=readonly
export GOWORK := off

.PHONY: help fmt fmt-check vet tidy-check deps build test coverage lint workflow-lint fuzz vuln check ci

help:
	@printf '%s\n' \
	  'make check          Format, vet, module/dependency checks, build, race tests' \
	  'make ci             All Linux CI checks (requires golangci-lint v2.14.0)' \
	  'make fmt            Format Go source files' \
	  'make coverage       Race tests with coverage.out and test.log' \
	  'make fuzz           Both stream fuzz targets (FUZZTIME=30s by default)' \
	  'make lint           Run the installed golangci-lint' \
	  'make workflow-lint  Validate GitHub Actions workflows' \
	  'make vuln           Scan reachable vulnerabilities'

fmt:
	$(GO) fmt ./...

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
	  printf 'Run make fmt for:\n%s\n' "$$unformatted"; exit 1; \
	fi

vet:
	$(GO) vet ./...

tidy-check:
	$(GO) mod tidy -diff

deps:
	GO="$(GO)" bash scripts/check-dependencies.sh

build:
	$(GO) build ./...

test:
	$(GO) test -race -shuffle=on -count=1 ./...

coverage:
	$(GO) test -race -shuffle=on -count=1 -covermode=atomic -coverprofile=coverage.out ./... 2>&1 | tee test.log
	$(GO) tool cover -func=coverage.out | tail -n 1

lint:
	$(GOLANGCI_LINT) run --timeout=5m

workflow-lint:
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) -color

fuzz:
	$(GO) test ./internal/llm/translate -run '^$$' -fuzz '^FuzzStreamChunkBoundaries$$' -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/llm/translate -run '^$$' -fuzz '^FuzzSSEFailureIsTerminal$$' -fuzztime=$(FUZZTIME)

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

check: fmt-check vet tidy-check deps build test
ci: fmt-check vet tidy-check deps build coverage lint workflow-lint fuzz vuln
