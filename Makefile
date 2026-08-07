.PHONY: run build test tidy fmt vet db-up

# Start (or reuse) the local PostgreSQL cluster used for development.
db-up:
	bash .cursor/scripts/start.sh

# Run the development server (expects Postgres to be up; see `make db-up`).
run:
	go run ./cmd/server

build:
	go build ./...

test:
	go test ./...

tidy:
	go mod tidy

fmt:
	gofmt -w .

vet:
	go vet ./...
