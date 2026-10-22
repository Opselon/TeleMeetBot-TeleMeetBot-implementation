# TeleMeetBot — Makefile
#
# Common targets:
#   make build            local binary (version stamped from git)
#   make test             unit tests (no browser needed)
#   make test-integration full tests, uses $TELEMEETBOT_BROWSER or auto-detected Chromium
#   make docker           build the container image
#   make release          cross-compile release archives + checksums in dist/
#   make publish          tag vX.Y.Z, push, create the GitHub release (VERSION required)

SHELL := /bin/bash
BIN   := telemeetbot
PKG   := github.com/Opselon/TeleMeetBot
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/config.Version=$(VERSION)
GOFLAGS := -trimpath

.PHONY: help build run wizard check test test-integration fmt vet lint docker release publish clean

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Build the local binary
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/telemeetbot

run: build ## Run with ./telemeetbot.yml
	./$(BIN) --config ./telemeetbot.yml

wizard: build ## Interactive setup wizard
	./$(BIN) --wizard

check: build ## Doctor + real screen-capture self-test
	./$(BIN) --config ./telemeetbot.yml --check --capture-test

test: ## Unit tests
	go test ./...

test-integration: ## Full suite including real Chromium tests
	TELEMEETBOT_BROWSER=$${TELEMEETBOT_BROWSER:-$$(command -v chromium || command -v google-chrome || true)} go test ./... -count=1

fmt: ## gofmt everything
	gofmt -w $$(find . -name '*.go' -not -path './dist/*')

vet: ## go vet
	go vet ./...

lint: fmt vet ## Format + vet

docker: ## Build the Docker image
	docker build --build-arg VERSION=$(VERSION) -t telemeetbot:$(VERSION) .

release: ## Build release archives into dist/
	VERSION=$(VERSION) ./scripts/release.sh build

publish: ## Tag, push and publish the GitHub release
	VERSION=$(VERSION) ./scripts/release.sh publish

clean: ## Remove build artifacts
	rm -rf dist $(BIN)
