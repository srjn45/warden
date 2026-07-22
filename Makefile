.PHONY: build test test-integration bench fuzz cover lint fmt fmt-check generate generate-check gendocs gendocs-check verify verify-fast run-daemon ui ui-dev web-deps web-test release install-skill install-hooks install uninstall reinstall

# Stamp version/commit/date into source builds so `warden version` is useful
# locally. VERSION comes from `git describe` — exactly `vX.Y.Z` on a tagged
# commit, `vX.Y.Z-N-g<sha>[-dirty]` past it — falling back to "dev" only when
# no tag is reachable. Release builds get all three from goreleaser's ldflags.
VERSION_PKG := github.com/srjn45/warden/internal/cli
VERSION     := $(shell git describe --tags --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X $(VERSION_PKG).version=$(VERSION) -X $(VERSION_PKG).commit=$(COMMIT) -X $(VERSION_PKG).date=$(BUILD_DATE)

build:
	go build -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/warden ./cmd/warden

test:
	go test ./...

# End-to-end suite (build-tagged `integration`): boots a real warden daemon
# subprocess against an isolated HOME and drives it through the real CLI. The
# spawn lifecycle test self-skips unless tmux + claude are installed, so this is
# safe to run in CI; locally it also covers the live spawn->terminate->cleanup.
test-integration:
	go test -tags integration -count=1 ./test/integration/...

# Run every Benchmark* across the repo once (no timing assertions; for tracking
# spawn/list/store-I/O cost over time). Use -benchtime to lengthen.
bench:
	go test -run '^$$' -bench . -benchmem ./...

# Smoke each Fuzz* target for a few seconds to catch panics on malformed input.
# CI runs the seed corpora as part of `make test`; this is the deeper sweep.
fuzz:
	go test -run '^$$' -fuzz FuzzParseSpec   -fuzztime 20s ./internal/pipeline/
	go test -run '^$$' -fuzz FuzzParse       -fuzztime 20s ./internal/approval/
	go test -run '^$$' -fuzz FuzzReadSession -fuzztime 20s ./internal/store/

# Whole-repo statement coverage. Prints the total% on the last line — that's the
# number behind the README coverage badge, so refresh the badge when this moves.
cover:
	go test ./... -coverprofile=coverage.out
	@go tool cover -func=coverage.out | tail -1

lint:
	go vet ./...

# Regenerate the strict OpenAPI server (internal/daemon/oapi/api.gen.go) from the
# hand-authored spec. The spec is the single source of truth; the generated file
# is committed so normal builds need no codegen step.
generate:
	go generate ./...

# CI guard: regenerate and fail if the committed code drifts from the spec — i.e.
# someone edited openapi.yaml without running `make generate` (or vice versa).
generate-check: generate
	@if ! git diff --quiet -- internal/daemon/oapi; then \
		echo "internal/daemon/oapi is out of date — run 'make generate' and commit the result:"; \
		git --no-pager diff --stat -- internal/daemon/oapi; \
		exit 1; \
	fi

# Regenerate the CLI reference (site/src/content/docs/reference/cli.md) by
# walking the real cobra command tree. The command tree is the single source of
# truth; the page is committed so the website builds without a codegen step.
gendocs:
	go run ./cmd/gendocs

# CI guard: regenerate the CLI reference and fail if the committed page drifts —
# i.e. someone changed a command's Use/Short/Long or flags without running
# `make gendocs` (so cli.md can never merge stale). Mirrors generate-check.
gendocs-check: gendocs
	@if ! git diff --quiet -- site/src/content/docs/reference/cli.md; then \
		echo "site/src/content/docs/reference/cli.md is out of date — run 'make gendocs' and commit the result:"; \
		git --no-pager diff --stat -- site/src/content/docs/reference/cli.md; \
		exit 1; \
	fi

# Format all Go sources in place.
fmt:
	gofmt -w $$(git ls-files '*.go')

# Fail if any Go source is not gofmt-clean (mirrors the CI lint job).
fmt-check:
	@unformatted=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-clean:"; \
		echo "$$unformatted"; \
		echo "Run 'make fmt' to fix."; \
		exit 1; \
	fi

# Full CI parity (run on an idle machine): gofmt, vet, ALL Go tests, web unit
# tests, and a release build with a binary smoke test. NOTE: the tmux-driven
# daemon/lifecycle/pipeline/poller/tui packages can time out locally when live
# warden agents are competing for the machine — that's why the pre-push hook
# uses verify-fast instead and leaves the heavy suite to CI's isolated runner.
verify: fmt-check generate-check lint test web-test release
	./bin/warden --help >/dev/null

# Fast, deterministic pre-push gate (no tmux-contention-prone Go tests):
# gofmt, vet, web unit tests, and a release build with a binary smoke test.
# Catches the formatting/compile/vet/web breakage that CI rejects, in well
# under a minute. CI's macOS runner owns the full `go test ./...` in isolation.
verify-fast: fmt-check generate-check lint web-test release
	./bin/warden --help >/dev/null

run-daemon: build
	./bin/warden daemon

# Install web deps only when missing, so `make web-test`/`release`/`verify-fast`
# work in a fresh clone or worktree that never ran `npm install`. npm ci wipes
# and reinstalls (slow), so skip it when node_modules is already present.
web-deps:
	@[ -d web/node_modules ] || (cd web && npm ci)

ui: web-deps
	cd web && npm run build

ui-dev: web-deps
	cd web && npm run dev

web-test: web-deps
	cd web && npm test

# Full release build: build the UI first so go:embed picks up real assets.
release: ui build

# Point git at the tracked hooks dir so the pre-commit (gofmt/vet) and pre-push
# (make verify-fast) gates run automatically. scripts/install.sh does this too.
install-hooks:
	git config core.hooksPath .githooks
	@echo "git hooks installed: core.hooksPath -> .githooks (pre-commit: fmt/vet, pre-push: verify-fast)"

# Symlink the warden Claude Code skill into ~/.claude/skills (idempotent).
install-skill:
	mkdir -p ~/.claude/skills
	ln -sfn $(PWD)/skills/warden ~/.claude/skills/warden
	@echo "linked ~/.claude/skills/warden -> $(PWD)/skills/warden"

# Install warden as a launchd service (build + binary + plist + skill + MCP).
# Use NO_BUILD=1 to skip the build and install the existing bin/warden.
install:
	./scripts/install.sh $(if $(NO_BUILD),--no-build,)

# Tear down the launchd service and integrations (preserves data + logs).
# Use KEEP_BINARY=1 to leave ~/.local/bin/warden in place.
uninstall:
	./scripts/uninstall.sh $(if $(KEEP_BINARY),--keep-binary,)

# Rebuild and redeploy the running daemon (use NO_BUILD=1 to skip the build).
reinstall:
	./scripts/reinstall.sh $(if $(NO_BUILD),--no-build,)
