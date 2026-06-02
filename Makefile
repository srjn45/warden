.PHONY: build test lint mongo-up mongo-down run-daemon ui ui-dev web-test release install-skill

build:
	go build -o bin/agentctl ./cmd/agentctl

test:
	go test ./...

lint:
	go vet ./...

mongo-up:
	docker compose up -d mongo

mongo-down:
	docker compose down

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
