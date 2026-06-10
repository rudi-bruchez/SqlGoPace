# SqlGoPace developer tasks.
# Integration tests are guarded by the `integration` build tag and excluded from `make test`.

BINARY := sqlgopace
PKG    := ./...

.PHONY: all build test cover lint tidy vet integration clean

all: lint test build

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
	go test -race -tags=integration $(PKG)

clean:
	rm -rf bin coverage.out
