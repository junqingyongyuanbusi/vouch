.PHONY: build test lint vet tidy canary clean install fmt fmt-check

BIN := bin/vouch
GO := go
GOFMT := gofmt

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/vouch

install: build
	cp $(BIN) $(shell go env GOPATH)/bin/vouch

test:
	$(GO) test ./... -count=1

test-race:
	$(GO) test -race ./... -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GOFMT) -l -w .

fmt-check:
	@test -z "$$($(GOFMT) -l . | grep -v '^testdata/')" || (echo "gofmt diff found"; $(GOFMT) -l .; exit 1)

lint:
	@which golangci-lint >/dev/null 2>&1 || (echo "install golangci-lint v2: https://golangci-lint.run/welcome/install/" && exit 1)
	golangci-lint run ./...

tidy:
	$(GO) mod tidy

canary:
	bash scripts/canary/run.sh

clean:
	rm -rf bin dist .vouch
	$(GO) clean -testcache

# CI entry: fmt-check is read-only, vet + test
ci: fmt-check vet test

# Validate the npm entry point (wrapper resolves the platform binary).
npm-wrapper:
	bash scripts/npm/build.sh

# Publish-path validation: pack, install into a clean prefix, run the CLI.
npm-roundtrip:
	bash scripts/npm/roundtrip.sh

# Verify-level canary: real repos, real `vouch verify` (needs network).
canary-verify:
	bash scripts/canary/verify.sh

# Deterministic agent-loop walkthrough (docs/agent-loop.md evidence).
demo-agent-loop:
	bash scripts/demo/agent-loop.sh
