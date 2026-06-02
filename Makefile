.PHONY: build test lint run-daemon ui ui-dev web-test release install-skill install uninstall reinstall

build:
	go build -o bin/agentctl ./cmd/agentctl

test:
	go test ./...

lint:
	go vet ./...

run-daemon: build
	./bin/agentctl daemon

ui:
	cd web && npm ci && npm run build

ui-dev:
	cd web && npm run dev

web-test:
	cd web && npm test

# Full release build: build the UI first so go:embed picks up real assets.
release: ui build

# Symlink the agentctl Claude Code skill into ~/.claude/skills (idempotent).
install-skill:
	mkdir -p ~/.claude/skills
	ln -sfn $(PWD)/skills/agentctl ~/.claude/skills/agentctl
	@echo "linked ~/.claude/skills/agentctl -> $(PWD)/skills/agentctl"

# Install agentctl as a launchd service (build + binary + plist + skill + MCP).
# Use NO_BUILD=1 to skip the build and install the existing bin/agentctl.
install:
	./scripts/install.sh $(if $(NO_BUILD),--no-build,)

# Tear down the launchd service and integrations (preserves data + logs).
# Use KEEP_BINARY=1 to leave ~/.local/bin/agentctl in place.
uninstall:
	./scripts/uninstall.sh $(if $(KEEP_BINARY),--keep-binary,)

# Rebuild and redeploy the running daemon (use NO_BUILD=1 to skip the build).
reinstall:
	./scripts/reinstall.sh $(if $(NO_BUILD),--no-build,)
