.PHONY: up down run seed vapid build test test-race vet fmt docker

# Postgres for local development (host port 5433).
up:
	docker compose up -d db

down:
	docker compose down

run:
	go run ./cmd/api

# Load sample data that mirrors the frontend's lib/data.ts (empty database only).
seed:
	go run ./cmd/seed

# Print a fresh VAPID key pair for Web Push.
vapid:
	go run ./cmd/vapid

build:
	go build -o bin/api ./cmd/api

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Tests need Postgres: `make up` first. Each test gets its own throwaway database.
test:
	REQUIRE_DB=1 go test ./... -count=1

# The race detector needs cgo; on machines without a C compiler run it in Docker.
test-race:
	go test -race ./... -count=1

docker:
	docker build -t placementhub-api .
