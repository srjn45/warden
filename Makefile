.PHONY: build test lint fmt fmt-check verify run-daemon ui ui-dev web-test release install-skill install-hooks install uninstall reinstall

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

# Run the same checks CI runs, in the same order: gofmt, vet, Go tests,
# web unit tests, and a full release build with a binary smoke test.
verify: fmt-check lint test web-test release
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
