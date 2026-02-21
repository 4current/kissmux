BINARY=kissmux
PKG=./cmd/kissmux
PREFIX?=/usr/local
MAN1DIR?=$(PREFIX)/share/man/man1

VERSION?=$(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
COMMIT?=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDDATE?=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILDDATE)"

.PHONY: build test clean install uninstall man

build:
	go build -o bin/$(BINARY) $(PKG)

test:
	go test ./...

clean:
	rm -rf bin

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/$(BINARY) $(PREFIX)/bin/$(BINARY)
	install -d $(MAN1DIR)
	install -m 0644 man/$(BINARY).1 $(MAN1DIR)/$(BINARY).1

uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)
	rm -f $(MAN1DIR)/$(BINARY).1

man:
	@echo "manpage is in man/$(BINARY).1 (roff)"

