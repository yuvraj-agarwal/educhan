BINARY   := educhan
PKG      := ./cmd/educhan
BIN_DIR  := bin
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w

.PHONY: help build run test cover lint fmt vet tidy clean docker

help: ## Show available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'

build: ## Compile a static binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(PKG)

run: ## Run the server locally
	go run $(PKG)

test: ## Run tests with the race detector
	go test -race ./...

cover: ## Report test coverage
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

fmt: ## Format the codebase
	go fmt ./...

vet: ## Run go vet
	go vet ./...

lint: fmt vet ## Format and vet

tidy: ## Reconcile go.mod and go.sum
	go mod tidy

docker: ## Build the container image
	docker build -t $(BINARY):$(VERSION) .

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.out
