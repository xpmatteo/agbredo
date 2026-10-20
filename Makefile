.DEFAULT_GOAL := help
.PHONY: help build vet test run

BIN := bin/agbredo
TEST_DB_NAME := agbredo-test-db

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[1m%-8s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the server into bin/agbredo
	go build -o $(BIN) ./cmd/agbredo

vet: ## Build everything and run go vet
	go build ./... && go vet ./...

test: vet ## Run all tests, with a scratch postgres on a random port
	@set -e; \
	docker rm -f $(TEST_DB_NAME) >/dev/null 2>&1 || true; \
	docker run -d --rm --name $(TEST_DB_NAME) \
		-e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=test \
		-p 127.0.0.1::5432 postgres:17-alpine >/dev/null; \
	trap 'docker rm -f $(TEST_DB_NAME) >/dev/null 2>&1' EXIT; \
	port=$$(docker port $(TEST_DB_NAME) 5432/tcp | head -n1 | sed 's/.*://'); \
	echo "Waiting for postgres on port $$port"; \
	until docker exec $(TEST_DB_NAME) pg_isready -U test -d test -h 127.0.0.1 >/dev/null 2>&1; do sleep 1; done; \
	AGBREDO_TEST_DATABASE_URL="postgres://test:test@127.0.0.1:$$port/test?sslmode=disable" \
		go test -count=1 ./...

run: ## Run the server locally (needs the environment from .env)
	go run ./cmd/agbredo serve
