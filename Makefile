# SqlGoPace developer tasks.
# Integration tests are guarded by the `integration` build tag and excluded from `make test`.

# Append .exe on Windows so `make build` produces bin/sqlgopace.exe (GOEXE is ".exe" on
# windows, empty elsewhere) — otherwise the output has no extension and a Windows shell
# launching bin\sqlgopace.exe silently runs a stale build.
BINARY := sqlgopace$(shell go env GOEXE)
PKG    := ./...

# Connection string for integration / e2e tests against the docker-compose server.
E2E_DSN ?= sqlserver://sa:Str0ng_Passw0rd!@localhost:1433?database=tempdb&encrypt=disable

# Container runtime. Defaults to Docker; override for Podman:
#   make e2e CONTAINER=podman COMPOSE="podman compose"
CONTAINER ?= docker
COMPOSE   ?= docker compose

.PHONY: all setup setup-check build test cover lint tidy vet integration e2e-up e2e-down e2e-test e2e clean

all: lint test build

# Install the linter version CI pins and the pre-push hook that runs it.
# Idempotent; `make setup-check` reports without changing anything.
setup:
	./scripts/setup-dev.sh

setup-check:
	./scripts/setup-dev.sh --check

build:
	go build -o bin/$(BINARY) ./cmd/sqlgopace

test:
	go test -race $(PKG)

cover:
	go test -race -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out

vet:
	go vet $(PKG)

lint:
	golangci-lint run

tidy:
	go mod tidy

integration:
	SQLGOPACE_TEST_DSN="$(E2E_DSN)" go test -race -tags=integration $(PKG)

# Bring up a throwaway SQL Server, wait until healthy.
e2e-up:
	$(COMPOSE) up -d
	@echo "waiting for SQL Server to become healthy..."
	@until [ "$$($(CONTAINER) inspect -f '{{.State.Health.Status}}' sqlgopace-mssql 2>/dev/null)" = "healthy" ]; do sleep 3; done
	@echo "SQL Server is healthy."

e2e-down:
	$(COMPOSE) down -v

# Run only the e2e/integration tests against the running server.
e2e-test:
	SQLGOPACE_TEST_DSN="$(E2E_DSN)" go test -tags=integration -run 'Integration|E2E' $(PKG)

# Full cycle: up, test, down.
e2e: e2e-up
	-$(MAKE) e2e-test
	$(MAKE) e2e-down

clean:
	rm -rf bin coverage.out
