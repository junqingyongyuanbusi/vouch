.PHONY: build test lint vet tidy canary canary-verify canary-regress canary-regress-fixtures clean install fmt fmt-check

BIN := bin/vouch
GO := go
# Pin gofmt to the toolchain in use. A bare `gofmt` resolves through PATH, which
# can be a different Go version than `go build` uses — and the two disagree on
# formatting, so CI and a developer's machine would reach opposite verdicts.
GOFMT := $(shell $(GO) env GOROOT)/bin/gofmt

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
	@bad="$$($(GOFMT) -l . | grep -v '^testdata/')"; \
	 if [ -n "$$bad" ]; then echo "gofmt diff found:"; echo "$$bad"; exit 1; fi

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

# Regression-injection canary: inject a change whose correct verdict is known
# and measure whether vouch reaches it. A clean repo returning VERIFIED proves
# nothing on its own -- this is what produces the false-clear rate.
canary-regress:
	bash scripts/canary/regress.sh

# Same arms against the local fixtures: no network, deterministic, seconds.
canary-regress-fixtures:
	bash scripts/canary/regress.sh --fixtures

# Deterministic agent-loop walkthrough (docs/agent-loop.md evidence).
demo-agent-loop:
	bash scripts/demo/agent-loop.sh
