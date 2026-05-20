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

# Smoke target runs both layers:
#   Layer B — Go unit tests (`go test ./...`) catch package-level
#             regressions before the CLI binary even runs.
#   Layer A — exercises the built binary against a tmp PoC, no cluster
#             required. Each step has a pass/fail check so a regression
#             breaks the build instead of silently scrolling by.
SMOKE_DIR := /tmp/kindbnkctl-smoke-make
smoke: test build
	@echo "=== Layer A smoke (CLI) ==="
	@set -eu; \
	echo "[1] version"; ./bin/kindbnkctl version >/dev/null; \
	echo "[2] init smoke (tmp)"; rm -rf $(SMOKE_DIR); \
	  ./bin/kindbnkctl init smoke --dir $(SMOKE_DIR) --no-git >/dev/null; \
	echo "[3] poc skeleton landed"; \
	  for f in poc.yaml AGENTS.md CLAUDE.md .gitignore keys/.gitkeep \
	           artifacts/.gitkeep journal; do \
	    test -e $(SMOKE_DIR)/$$f || { echo "MISSING $$f"; exit 1; }; \
	  done; \
	echo "[4] validate (expect missing-keys error)"; \
	  if ./bin/kindbnkctl validate --poc $(SMOKE_DIR) >/tmp/smoke-validate.log 2>&1; then \
	    echo "validate should have failed (returned 0)"; exit 1; \
	  fi; \
	  grep -q "far_key_ref file" /tmp/smoke-validate.log || { echo "FAR error missing"; exit 1; }; \
	  grep -q "jwt_ref file"     /tmp/smoke-validate.log || { echo "JWT error missing"; exit 1; }; \
	echo "[5] touch fake keys + validate clean"; \
	  touch $(SMOKE_DIR)/keys/f5-far-auth-key.tgz $(SMOKE_DIR)/keys/.jwt; \
	  ./bin/kindbnkctl validate --poc $(SMOKE_DIR) | grep -q "OK"; \
	echo "[6] e2e --dry-run lists 5 phases with auto-filled gates"; \
	  ./bin/kindbnkctl e2e --poc $(SMOKE_DIR) --dry-run > /tmp/smoke-e2e.log 2>&1; \
	  for ph in validate cluster-up deploy-prereqs deploy-flo deploy-cne; do \
	    grep -q "$$ph" /tmp/smoke-e2e.log || { echo "phase $$ph missing"; exit 1; }; \
	  done; \
	  grep -q -- "--confirm-cluster smoke" /tmp/smoke-e2e.log || { echo "confirm-cluster not auto-filled"; exit 1; }; \
	  grep -q -- "--confirm-deploy smoke"  /tmp/smoke-e2e.log || { echo "confirm-deploy not auto-filled"; exit 1; }; \
	echo "[7] --yolo without --confirm-cluster errors"; \
	  if ./bin/kindbnkctl e2e --poc $(SMOKE_DIR) --yolo >/tmp/smoke-yolo.log 2>&1; then \
	    echo "missing-confirm gate broken (returned 0)"; exit 1; \
	  fi; \
	  grep -q "confirm-cluster is required" /tmp/smoke-yolo.log \
	    || { echo "missing-confirm error message missing"; exit 1; }; \
	echo "[8] doctor reports cores ≥ min baseline"; \
	  ./bin/kindbnkctl doctor 2>&1 | grep -q "min baseline" \
	    || { echo "doctor min-baseline line missing"; exit 1; }; \
	echo "PASS"

clean:
	rm -rf bin/
