.PHONY: help run test race cover lint vet fmt build docker clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Run the server locally
	go run ./cmd/server

test: ## Run all tests
	go test ./... -count=1

race: ## Run all tests under the race detector
	go test ./... -race -count=1

cover: ## Report test coverage per package
	go test ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -n 20

fmt: ## Format all Go source
	gofmt -w .

vet: ## Run go vet
	go vet ./...

lint: fmt vet ## Format then vet
	@test -z "$$(gofmt -l .)" || (echo "unformatted files:"; gofmt -l .; exit 1)

build: ## Build the server binary
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/server ./cmd/server

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t game-arena:$(VERSION) .

clean: ## Remove build artefacts
	rm -rf bin coverage.out
