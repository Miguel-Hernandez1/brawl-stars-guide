ROOT := $(shell pwd)

# Load .env if it exists (never committed; see .env.example)
-include .env
export

.PHONY: dev test build collect migrate-up migrate-down seed-brawlers psql lint

## dev: start local PostgreSQL via docker compose
dev:
	docker compose -f deploy/docker-compose.yml up -d

## test: run all unit tests (no API or DB required)
test:
	go test ./...

## test-verbose: run tests with output
test-verbose:
	go test -v ./...

## build: compile both binaries into bin/
build:
	go build -o bin/collector ./collector/cmd/collector
	go build -o bin/api ./api/cmd/api

## migrate-up: apply all pending database migrations (run from project root)
migrate-up:
	go run ./collector/cmd/collector migrate up

## migrate-down: roll back the last database migration
migrate-down:
	go run ./collector/cmd/collector migrate down

## seed-brawlers: populate the brawlers reference table from the API
seed-brawlers:
	go run ./collector/cmd/collector seed brawlers

## collect: ingest a player - usage: make collect tag=#YOURTAG
collect:
	go run ./collector/cmd/collector collect player $(tag)

## psql: open a psql shell to the local dev database
psql:
	docker compose -f deploy/docker-compose.yml exec postgres psql -U playbook -d playbook

## lint: run go vet
lint:
	go vet ./...

## help: list targets
help:
	@grep -E '^## ' Makefile | sed 's/## //'
