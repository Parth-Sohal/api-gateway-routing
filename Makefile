GO ?= go
BINARY ?= bin/gateway
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build fmt vet lint test test-race cover cover-gate test-integration bench fuzz load chaos docker compose-up compose-down clean token mock

all: fmt vet test

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/gateway

fmt:
	gofmt -w .

vet:
	go vet ./...

lint:
	golangci-lint run

test:
	go test ./...

test-race:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out | tail -n 20

cover-gate:
	bash scripts/coverage-gate.sh

test-integration:
	go test -race -count=1 -run 'Integration|MultiInstance|Graceful' ./internal/...

bench:
	go test -run '^$$' -bench . -benchmem ./internal/...

fuzz:
	go test -run '^$$' -fuzz FuzzConfig -fuzztime 20s ./internal/config
	go test -run '^$$' -fuzz FuzzClientIP -fuzztime 20s ./internal/httpx
	go test -run '^$$' -fuzz FuzzRouter -fuzztime 20s ./internal/router

docker:
	docker build -t api-gateway-routing:$(VERSION) .

compose-up:
	docker compose -f deploy/docker-compose.yml up --build -d

compose-down:
	docker compose -f deploy/docker-compose.yml down -v

chaos:
	docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.chaos.yml up --build -d

load:
	docker compose -f deploy/docker-compose.yml --profile load run --rm k6 run /scripts/baseline.js

token:
	go run ./cmd/token -secret "$$GATEWAY_JWT_SECRET" -sub demo -scopes "users:read,orders:read"

mock:
	go run ./cmd/mockupstream -addr :9001 -name users-1

clean:
	rm -rf bin dist coverage.out coverage.html
	go clean -testcache
