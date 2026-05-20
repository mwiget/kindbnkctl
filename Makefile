.PHONY: all build build-linux-arm64 build-darwin-arm64 build-all install test clean tidy fmt vet smoke release release-linux-amd64 release-darwin-arm64

all: build-all

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BNK     := 2.3.0

LDFLAGS := -X 'github.com/mwiget/kindbnkctl/internal/version.Version=$(VERSION)' \
           -X 'github.com/mwiget/kindbnkctl/internal/version.Commit=$(COMMIT)' \
           -X 'github.com/mwiget/kindbnkctl/internal/version.BuildDate=$(DATE)' \
           -X 'github.com/mwiget/kindbnkctl/internal/version.BNKVersion=$(BNK)'

build:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/kindbnkctl ./cmd/kindbnkctl

build-linux-arm64:
	@mkdir -p bin
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
	    go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o bin/kindbnkctl-linux-arm64 ./cmd/kindbnkctl

build-darwin-arm64:
	@mkdir -p bin
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
	    go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o bin/kindbnkctl-darwin-arm64 ./cmd/kindbnkctl

build-all: build build-linux-arm64

release-linux-amd64:
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
	    go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o bin/kindbnkctl-$(VERSION)-linux-amd64 ./cmd/kindbnkctl
	cd bin && sha256sum kindbnkctl-$(VERSION)-linux-amd64 \
	    > kindbnkctl-$(VERSION)-linux-amd64.sha256

release-darwin-arm64:
	@mkdir -p bin
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
	    go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o bin/kindbnkctl-$(VERSION)-darwin-arm64 ./cmd/kindbnkctl
	cd bin && sha256sum kindbnkctl-$(VERSION)-darwin-arm64 \
	    > kindbnkctl-$(VERSION)-darwin-arm64.sha256

release: release-linux-amd64 release-darwin-arm64

install: build
	install -m 0755 bin/kindbnkctl $(HOME)/.local/bin/kindbnkctl

test:
	go test ./...

tidy:
	go mod tidy

fmt:
	gofmt -w .

vet:
	go vet ./...

smoke: build
	@echo "--- version ---"
	./bin/kindbnkctl version
	@echo "--- init smoke ---"
	@rm -rf /tmp/kindbnkctl-smoke
	./bin/kindbnkctl init smoke --dir /tmp/kindbnkctl-smoke --no-git
	@ls /tmp/kindbnkctl-smoke
	@echo "--- validate smoke ---"
	./bin/kindbnkctl validate --poc /tmp/kindbnkctl-smoke || true
	@echo "--- e2e dry-run ---"
	./bin/kindbnkctl e2e --poc /tmp/kindbnkctl-smoke --dry-run || true

clean:
	rm -rf bin/
