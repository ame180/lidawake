VERSION ?= 0.1.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test build install uninstall clean

test:
	go vet ./...
	go test ./...

# Native build for the Mac you're on.
build:
	go build -ldflags "$(LDFLAGS)" -o dist/lidawake ./cmd/lidawake

dist:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/lidawake-darwin-arm64 ./cmd/lidawake
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/lidawake-darwin-amd64 ./cmd/lidawake

install: build
	sudo ./dist/lidawake install

uninstall:
	sudo /usr/local/bin/lidawake uninstall

clean:
	rm -rf dist
