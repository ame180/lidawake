VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test lint build

test:
	go vet ./...
	go test -race ./...

lint:
	golangci-lint run

build:
	go build -ldflags "$(LDFLAGS)" -o dist/lidawake ./cmd/lidawake
