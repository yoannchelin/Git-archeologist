.PHONY: build test fmt vet tidy install index-self mcp

GOFLAGS ?= -tags fts5

build:
	go build $(GOFLAGS) ./...

# Build standalone binaries into ./bin
bin: bin/archaeo bin/archaeo-mcp

bin/archaeo: $(shell find cmd/archaeo internal -name '*.go')
	@mkdir -p bin
	go build $(GOFLAGS) -o bin/archaeo ./cmd/archaeo

bin/archaeo-mcp: $(shell find cmd/archaeo-mcp internal -name '*.go')
	@mkdir -p bin
	go build $(GOFLAGS) -o bin/archaeo-mcp ./cmd/archaeo-mcp

test:
	go test $(GOFLAGS) ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

install:
	go install ./cmd/archaeo
	go install ./cmd/archaeo-mcp

# Dogfood: index this repo itself.
index-self: bin/archaeo
	./bin/archaeo index --repo . --no-embed

# Start the MCP server against the current repo (assumes prior index).
mcp: bin/archaeo-mcp
	./bin/archaeo-mcp --repo .
