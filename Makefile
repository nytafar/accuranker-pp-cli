.PHONY: build test lint install clean

build:
	go build -o bin/accuranker-pp-cli ./cmd/accuranker-pp-cli

test:
	go test ./...

lint:
	golangci-lint run

install:
	go install ./cmd/accuranker-pp-cli

clean:
	rm -rf bin/

build-mcp:
	go build -o bin/accuranker-pp-mcp ./cmd/accuranker-pp-mcp

install-mcp:
	go install ./cmd/accuranker-pp-mcp

build-all: build build-mcp
