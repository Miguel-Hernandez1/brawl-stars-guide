ROOT := $(shell pwd)

.PHONY: dev test test-verbose build collect migrate-up migrate-down seed-brawlers psql lint help

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
	@set -a; . ./.env 2>/dev/null; set +a; \
	 go run ./collector/cmd/collector migrate up

## migrate-down: roll back the last database migration
migrate-down:
	@set -a; . ./.env 2>/dev/null; set +a; \
	 go run ./collector/cmd/collector migrate down

## seed-brawlers: populate the brawlers reference table from the API
seed-brawlers:
	@set -a; . ./.env 2>/dev/null; set +a; \
	 go run ./collector/cmd/collector seed brawlers

## collect: ingest a player's profile and battle log
##   TAG='#2UCCRJ280P' make collect   (hash preserved via env var)
##   make collect tag=2UCCRJ280P       (hash prefix is optional; binary normalizes the tag)
##   Note: 'make collect tag=#...' does not work - Make treats # as a comment.
##   Use the TAG env-var form when the leading hash matters.
collect:
	@set -a; . ./.env 2>/dev/null; set +a; \
	 go run ./collector/cmd/collector collect player "$${TAG:-$(tag)}"

## psql: open a psql shell to the local dev database
psql:
	docker compose -f deploy/docker-compose.yml exec postgres psql -U playbook -d playbook

## lint: run go vet
lint:
	go vet ./...

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/## //'
