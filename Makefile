.PHONY: build test test-required test-alias vet lint lint-strict arch-metrics fmt fmt-check vulncheck completions release-check release-snapshot release-cask package-check smoke-live cost-check

GORELEASER ?= goreleaser
GOVULNCHECK ?= govulncheck
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0
GOCOGNIT ?= go run github.com/uudashr/gocognit/cmd/gocognit@v1.2.0

build:
	go build -o bin/zalki ./cmd/zalki

test: build
	ZALKI_TEST_BINARY="$(CURDIR)/bin/zalki" go test -race ./...

# CI must prove that the real socket/lifecycle/emulated roundtrip tests ran.
# Preserve local sandbox skips in the ordinary test target.
test-required: build
	@set -eu; report="$$(mktemp)"; trap 'rm -f "$$report"' EXIT; \
	if ! ZALKI_TEST_BINARY="$(CURDIR)/bin/zalki" go test -race -count=1 -json ./... > "$$report"; then cat "$$report"; exit 1; fi; \
	go run ./scripts/check-integration "$$report"

# Full required suite with a proven symlinked temporary root (one full pass).
test-alias:
	GOFLAGS= GOWORK=off go run -p=1 -mod=readonly ./scripts/test-alias

vet:
	go vet ./...

# Any command that fails (a finding, a tool error) stops make with a non-zero exit.
# - noctx runs separately on production code; test subprocess calls also use
#   CommandContext.
# - The gocognit budget is a ratchet: no production function above 30. Lower it
#   over time. make lint-strict keeps reporting at 20 (.golangci.yml), so the
#   checkpoint numbers stay comparable with the old baseline.
lint:
	$(GOLANGCI_LINT) run ./cmd/... ./internal/... ./scripts/...
	$(GOLANGCI_LINT) run --enable-only noctx --tests=false ./cmd/... ./internal/... ./scripts/...
	$(GOCOGNIT) -over 30 -ignore '_test\.go' cmd internal scripts

# Report-only: stricter linters (complexity, function length, API hygiene) used
# at architecture checkpoints. Findings are informational and do not fail.
lint-strict:
	$(GOLANGCI_LINT) run --config .golangci.yml --enable gocyclo,gocognit,funlen,gocritic,unparam,prealloc,misspell,thelper,nilerr,noctx,errname,copyloopvar,perfsprint,unconvert,wastedassign,predeclared,usestdlibvars,nakedret,exhaustive --issues-exit-code 0 ./cmd/... ./internal/... ./scripts/...

# Structural report for architecture checkpoints; fails on a boundary violation.
arch-metrics:
	go run ./scripts/arch-metrics
	$(GOLANGCI_LINT) run --config .golangci.yml --enable-only depguard ./cmd/... ./internal/... ./scripts/...

fmt:
	gofmt -w cmd internal scripts

fmt-check:
	@set -eu; files="$$(gofmt -l cmd internal scripts)"; \
	if [ -n "$$files" ]; then printf 'Run make fmt for:\n%s\n' "$$files"; exit 1; fi

vulncheck:
	$(GOVULNCHECK) ./...

completions: build
	mkdir -p completions
	./bin/zalki completion bash > completions/zalki.bash
	./bin/zalki completion zsh > completions/_zalki
	./bin/zalki completion fish > completions/zalki.fish
	./bin/zalki completion powershell > completions/zalki.ps1

release-check:
	$(GORELEASER) check

release-snapshot: fmt-check test vet completions release-check
	$(GORELEASER) release --snapshot --clean
	$(MAKE) package-check

release-cask:
	mkdir -p dist
	sh scripts/homebrew-cask.sh "$(VERSION)" "dist/zalki_$(VERSION)_darwin_arm64.tar.gz" "dist/zalki_$(VERSION)_darwin_amd64.tar.gz" > dist/zalki.rb.tmp
	mv dist/zalki.rb.tmp dist/zalki.rb

package-check:
	sh scripts/check-release.sh dist

smoke-live:
	@test -n "$$ZALKI_SMOKE_STATE" -a -n "$$ZALKI_SMOKE_WORKSPACE" -a -n "$$ZALKI_SMOKE_TARGETS" || { echo 'Set ZALKI_SMOKE_STATE, ZALKI_SMOKE_WORKSPACE, and exact ZALKI_SMOKE_TARGETS first.' >&2; exit 2; }
	mkdir -p bin
	go test -c -o bin/live-smoke.test ./internal/bus
	ZALKI_LIVE_SMOKE=1 ./bin/live-smoke.test -test.v -test.run='^TestLiveClients$$'

cost-check:
	go test ./internal/bus -run '^$$' -bench '^BenchmarkMessageLookup$$' -benchtime=100ms -benchmem
	go test ./internal/mcpserver -run '^TestCollaborationCost$$' -v
