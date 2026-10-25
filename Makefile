.PHONY: build test vet lint fmt fmt-check vulncheck completions release-check release-snapshot release-cask package-check smoke-live cost-check

GORELEASER ?= goreleaser
GOVULNCHECK ?= govulncheck
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0

build:
	go build -o bin/zalki ./cmd/zalki

test: build
	ZALKI_TEST_BINARY="$(CURDIR)/bin/zalki" go test -race ./...

vet:
	go vet ./...

lint:
	$(GOLANGCI_LINT) run ./cmd/... ./internal/... ./scripts/...

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
