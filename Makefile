# ==============================================================================
# MasterDnsVPN — convenience Makefile
# Author: MasterkinG32
# Github: https://github.com/masterking32
# Year: 2026
# ==============================================================================
#
# This Makefile is a thin wrapper around `go` and the bench harness under
# scripts/bench. It intentionally avoids any build-time magic — every recipe
# is something a developer can also run by hand. The targets follow the
# Step 1 contract in PLAN.md ("افزودن یک Makefile target ساده").

GO            ?= go
PKG           ?= ./...
BENCH_RUNS    ?= 3
BENCH_BYTES   ?= 10485760
PPROF_CLIENT  ?= 127.0.0.1:6060
PPROF_SERVER  ?= 127.0.0.1:6061

.PHONY: help build test test-race vet bench bench-loss pprof-client pprof-server clean

help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: ## Compile both client and server binaries.
	$(GO) build -o bin/masterdnsvpn-client ./cmd/client
	$(GO) build -o bin/masterdnsvpn-server ./cmd/server

test: ## Run the full test suite.
	$(GO) test $(PKG)

test-race: ## Run the full test suite with the race detector.
	$(GO) test -race $(PKG)

vet: ## Run go vet across all packages.
	$(GO) vet $(PKG)

bench: ## Run the end-to-end bench harness in lossless mode.
	$(GO) run ./scripts/bench -runs $(BENCH_RUNS) -bytes $(BENCH_BYTES)

bench-loss: ## Run the bench harness against a lossy localhost (5% loss).
	# Lossy bench requires a tc-based netem environment; this recipe just
	# documents the typical invocation. Run as root or inside the linux
	# installer environment, then re-run `make bench` to compare.
	@echo "==> Configure netem: sudo tc qdisc add dev lo root netem loss 5% delay 5ms"
	@echo "==> Then run: make bench"
	@echo "==> Reset with: sudo tc qdisc del dev lo root"

pprof-client: ## Start the client with pprof bound to $(PPROF_CLIENT).
	PPROF_ADDR=$(PPROF_CLIENT) $(GO) run ./cmd/client

pprof-server: ## Start the server with pprof bound to $(PPROF_SERVER).
	PPROF_ADDR=$(PPROF_SERVER) $(GO) run ./cmd/server

clean: ## Remove build artefacts.
	rm -rf bin scripts/bench/.bench
