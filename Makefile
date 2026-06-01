.PHONY: build test lint mongo-up mongo-down run-daemon

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
