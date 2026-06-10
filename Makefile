.PHONY: build test lint fmt fmt-check verify verify-fast run-daemon ui ui-dev web-test release install-skill install-hooks install uninstall reinstall

build:
	go build -o bin/warden ./cmd/warden

test:
	go test ./...

lint:
	go vet ./...

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
verify: fmt-check lint test web-test release
	./bin/warden --help >/dev/null

# Fast, deterministic pre-push gate (no tmux-contention-prone Go tests):
# gofmt, vet, web unit tests, and a release build with a binary smoke test.
# Catches the formatting/compile/vet/web breakage that CI rejects, in well
# under a minute. CI's macOS runner owns the full `go test ./...` in isolation.
verify-fast: fmt-check lint web-test release
	./bin/warden --help >/dev/null

run-daemon: build
	./bin/warden daemon

ui:
	cd web && npm ci && npm run build

ui-dev:
	cd web && npm run dev

web-test:
	cd web && npm test

# Full release build: build the UI first so go:embed picks up real assets.
release: ui build

# Point git at the tracked hooks dir so pre-push runs `make verify` (CI parity).
install-hooks:
	git config core.hooksPath .githooks
	@echo "git hooks installed: core.hooksPath -> .githooks"

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
