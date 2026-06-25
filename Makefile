# aitori — build & run.
#
# This repo is a multi-module workspace:
#   .                      the aitori agent (cmd/aitori)
#   tools/aitori-gateway  a local AI gateway with OTel tracing + a trace UI
#
# CGO is disabled on every build: a transitive gopsutil dependency (go-m1cpu)
# crashes under cgo on recent Go/macOS, and aitori doesn't need it.

GO          ?= go
GOBUILD     := CGO_ENABLED=0 $(GO) build
GOTEST      := CGO_ENABLED=0 $(GO) test
BIN         := bin
GATEWAY_DIR := tools/aitori-gateway
CONFIG      ?= configs/conversations.yaml

# Version stamped into the binary (aitori status / --version). Defaults to the
# git tag/commit; override: make build VERSION=v0.1.0  /  make dist VERSION=v0.1.0
VERSION         ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_LDFLAGS := -X github.com/truefoundry/aitori/internal/version.Version=$(VERSION)

.DEFAULT_GOAL := build

## ---- build ---------------------------------------------------------------

.PHONY: build
build: aitori gateway ## Build both binaries into ./bin

.PHONY: aitori
aitori: ## Build the aitori agent
	$(GOBUILD) -ldflags "$(VERSION_LDFLAGS)" -o $(BIN)/aitori ./cmd/aitori

.PHONY: gateway
gateway: ## Build the aitori-gateway (trace UI) tool
	cd $(GATEWAY_DIR) && CGO_ENABLED=0 $(GO) build -o $(CURDIR)/$(BIN)/aitori-gateway .

## ---- test / lint ---------------------------------------------------------

.PHONY: test
test: ## Run tests for both modules
	$(GOTEST) ./...
	cd $(GATEWAY_DIR) && CGO_ENABLED=0 $(GO) test ./...

.PHONY: vet
vet: ## go vet both modules
	CGO_ENABLED=0 $(GO) vet ./...
	cd $(GATEWAY_DIR) && CGO_ENABLED=0 $(GO) vet ./...

.PHONY: fmt
fmt: ## gofmt -w all Go sources
	gofmt -w cmd internal $(GATEWAY_DIR)

.PHONY: tidy
tidy: ## go mod tidy both modules
	$(GO) mod tidy
	cd $(GATEWAY_DIR) && $(GO) mod tidy

.PHONY: check
check: fmt vet test ## fmt + vet + test

## ---- run -----------------------------------------------------------------

.PHONY: run-gateway
run-gateway: gateway ## Run the gateway with body logging + trace UI
	$(BIN)/aitori-gateway -debug

.PHONY: run
run: aitori ## Run in explicit-proxy mode, no OS changes; CONFIG=... to override
	$(BIN)/aitori run -c $(CONFIG)

.PHONY: up
up: aitori ## Install CA + system proxy and run (needs sudo); CONFIG=... to override
	sudo $(BIN)/aitori up -c $(CONFIG)

.PHONY: down
down: aitori ## Revert system proxy / Claude Code settings (needs sudo)
	sudo $(BIN)/aitori down

.PHONY: validate
validate: aitori ## Validate CONFIG
	$(BIN)/aitori config validate $(CONFIG)

## ---- release -------------------------------------------------------------

# os/arch combos to package. Override: make dist PLATFORMS="linux/amd64 darwin/arm64"
PLATFORMS ?= darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: dist
dist: ## Build per-os/arch tar.gz bundles (aitori + aitori-gateway + config) into dist/
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; name=aitori_$${os}_$${arch}; stage=dist/$$name; ext=; \
	  [ "$$os" = windows ] && ext=.exe; \
	  echo "==> $$name"; mkdir -p $$stage; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -ldflags "-s -w $(VERSION_LDFLAGS)" -o $$stage/aitori$$ext ./cmd/aitori || exit 1; \
	  ( cd $(GATEWAY_DIR) && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -ldflags "-s -w" -o $(CURDIR)/$$stage/aitori-gateway$$ext . ) || exit 1; \
	  cp configs/demo.yaml configs/conversations.yaml LICENSE $$stage/; \
	  tar -C dist -czf dist/$$name.tar.gz $$name; rm -rf $$stage; \
	done
	@echo "==> bundles:"; ls -1 dist/*.tar.gz

## ---- clean ---------------------------------------------------------------

.PHONY: clean
clean: ## Remove build artifacts and local trace DBs
	rm -rf $(BIN) dist
	rm -f *.db *.db-shm *.db-wal $(GATEWAY_DIR)/*.db $(GATEWAY_DIR)/*.db-shm $(GATEWAY_DIR)/*.db-wal

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
