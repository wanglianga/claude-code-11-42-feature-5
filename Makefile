SHELL := /bin/bash
DATABASE_URL ?= postgres://ccdev:ccdev@127.0.0.1:5432/ccdev?sslmode=disable
GO ?= go

.PHONY: help build run demo test test-domain fmt vet tidy clean psql docker-up docker-demo

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Build server and demo binaries
	$(GO) build -o bin/server ./cmd/server
	$(GO) build -o bin/demo ./cmd/demo

run: build ## Run the API (needs PostgreSQL at DATABASE_URL)
	DATABASE_URL='$(DATABASE_URL)' ./bin/server

demo: ## Run the end-to-end scenario and print a markdown report
	DATABASE_URL='$(DATABASE_URL)' $(GO) run ./cmd/demo -markdown

test: ## Unit + integration tests (integration is skipped without a DB)
	$(GO) test ./...

test-domain: ## Pure-domain tests only (no database)
	$(GO) test ./internal/domain/ -v

fmt: ## Format Go sources
	gofmt -w ./cmd ./internal

vet: ## Static analysis
	$(GO) vet ./...

tidy: ## Tidy modules
	$(GO) mod tidy

psql: ## Open a psql shell on the local dev database
	PGPASSWORD=ccdev psql -h 127.0.0.1 -U ccdev -d ccdev

docker-up: ## Build and start db + api via compose
	docker compose up --build -d

docker-demo: ## Run the demo scenario in a container
	docker compose --profile demo run --rm demo

review-test: ## Historical review tests only (no DB for the domain half)
	$(GO) test ./internal/domain/review/ -v
	$(GO) test ./internal/api/ -run TestReview -v

clean: ## Remove build artifacts
	rm -rf bin
