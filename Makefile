BIN := ax
PREFIX ?= $(HOME)/.local

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DIRTY  := $(shell git diff --quiet 2>/dev/null || echo -dirty)
DATE   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG    := github.com/agentswitch-org/ax/internal/build
LDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT)$(DIRTY) -X $(PKG).Date=$(DATE)

.PHONY: build install clean models linux deploy check

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/ax

install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BIN) $(PREFIX)/bin/$(BIN)

# Cross-compile a static Linux binary for a remote host. Set ARCH=arm64 for ARM.
# CGO is off so the binary carries no libc dependency.
ARCH ?= amd64
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -ldflags "$(LDFLAGS)" -o $(BIN)-linux-$(ARCH) ./cmd/ax

# Copy the Linux binary to a host's PATH, e.g.
#   make deploy HOST=remote-code DEST=.local/bin/ax
deploy: linux
	scp $(BIN)-linux-$(ARCH) $(HOST):$(DEST)

# Refresh the checked-in model snapshot from models.dev, then rebuild so it is
# re-embedded into the binary.
models:
	./$(BIN) models update
	cp $${XDG_STATE_HOME:-$$HOME/.local/state}/ax/models.json internal/models/models.json
	$(MAKE) build

clean:
	rm -f $(BIN)

check:
	go vet ./...
	@files=$$(find . -name '*.go' | xargs gofmt -l); \
	if [ -n "$$files" ]; then echo "gofmt: $$files"; exit 1; fi
	go test -race ./...
