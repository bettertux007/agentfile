BINARY := agentfile
PKG := ./cmd/agentfile
DIST_DIR := dist
INSTALL_DIR ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -s -w \
	-X github.com/bettertux007/agentfile/internal/cli.Version=$(VERSION) \
	-X github.com/bettertux007/agentfile/internal/cli.Commit=$(COMMIT) \
	-X github.com/bettertux007/agentfile/internal/cli.Date=$(DATE)

.PHONY: all build build-all install test test-race coverage vet fmt lint check clean docs-dev docs-build

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

build-all:
	@mkdir -p $(DIST_DIR)
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-darwin-arm64 $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-darwin-amd64 $(PKG)
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-amd64 $(PKG)
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-arm64 $(PKG)
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-windows-amd64.exe $(PKG)

install:
	@mkdir -p "$(INSTALL_DIR)"
	go build -ldflags "$(LDFLAGS)" -o "$(INSTALL_DIR)/$(BINARY)" $(PKG)
	@echo "Installed $(BINARY) to $(INSTALL_DIR)/$(BINARY)"

test:
	go test ./...

test-race:
	go test -race ./...

coverage:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

vet:
	go vet ./...

fmt:
	gofmt -w .

lint: vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		test -z "$$(gofmt -l .)" || (echo "Run 'make fmt' to format files" && exit 1); \
	fi

check: lint test-race
	GOOS=windows go build ./...
	GOOS=linux go build ./...
	go run $(PKG) audit --strict --no-store

clean:
	rm -rf $(BINARY) $(DIST_DIR) coverage.out internal_cov.out docs/.vitepress/dist docs/.vitepress/cache

docs-dev:
	npm run docs:dev --prefix docs

docs-build:
	npm run docs:build --prefix docs
