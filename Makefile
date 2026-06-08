.PHONY: build test lint run-daemon ui ui-dev web-test release install-skill install uninstall reinstall

build:
	go build -o bin/warden ./cmd/warden

test:
	go test ./...

lint:
	go vet ./...

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
