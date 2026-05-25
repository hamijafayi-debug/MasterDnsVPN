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

# ------------------------------------------------------------------------------
# Step 23 — Release Hardening flags
#
# RELEASE_LDFLAGS strips both the symbol table (-s) and DWARF info (-w),
# yielding ~30% smaller binaries. -trimpath rewrites every absolute path in
# the binary to its module-relative form (reproducible, no $HOME leak).
# Override RELEASE_VERSION on the command line to embed a custom version
# string; default is "dev". GOAMD64_LEVEL governs the x86-64 micro-arch:
#   - v1 (default) = baseline (works everywhere)
#   - v2 = Nehalem (2008+) — SSE4.2, POPCNT
#   - v3 = Haswell (2013+) — AVX2, BMI1/2, FMA  ← recommended for "modern" build
#   - v4 = Skylake-X (2017+) — AVX512
# ------------------------------------------------------------------------------
RELEASE_VERSION  ?= dev
RELEASE_LDFLAGS  ?= -s -w -X masterdnsvpn-go/internal/version.BuildVersion=$(RELEASE_VERSION)
RELEASE_FLAGS    ?= -trimpath -ldflags "$(RELEASE_LDFLAGS)"
GOAMD64_LEVEL    ?= v3

.PHONY: help build test test-race vet bench bench-loss pprof-client pprof-server clean release release-modern pgo-collect pgo-clean

help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: ## Compile both client and server binaries.
	$(GO) build -o bin/masterdnsvpn-client ./cmd/client
	$(GO) build -o bin/masterdnsvpn-server ./cmd/server

release: ## Build stripped, trimpath'd release binaries (portable amd64=v1).
	@mkdir -p bin
	$(GO) build $(RELEASE_FLAGS) -o bin/masterdnsvpn-client-release ./cmd/client
	$(GO) build $(RELEASE_FLAGS) -o bin/masterdnsvpn-server-release ./cmd/server
	@echo "==> Built release binaries (GOAMD64=$$(go env GOAMD64), version=$(RELEASE_VERSION))"
	@ls -la bin/masterdnsvpn-*-release

release-modern: ## Build release binaries with GOAMD64=$(GOAMD64_LEVEL) (Haswell+ by default).
	@mkdir -p bin
	GOAMD64=$(GOAMD64_LEVEL) $(GO) build $(RELEASE_FLAGS) -o bin/masterdnsvpn-client-release-$(GOAMD64_LEVEL) ./cmd/client
	GOAMD64=$(GOAMD64_LEVEL) $(GO) build $(RELEASE_FLAGS) -o bin/masterdnsvpn-server-release-$(GOAMD64_LEVEL) ./cmd/server
	@echo "==> Built modern release binaries (GOAMD64=$(GOAMD64_LEVEL), version=$(RELEASE_VERSION))"
	@ls -la bin/masterdnsvpn-*-release-$(GOAMD64_LEVEL)

pgo-collect: ## Collect CPU profiles and merge them into cmd/{client,server}/default.pgo.
	@echo "==> Collecting PGO profiles via end-to-end bench (this takes ~30s)..."
	$(GO) run ./scripts/bench -runs 2 -bytes $(BENCH_BYTES) -pgo -pgo-seconds 8
	@echo "==> default.pgo files written. Next `make release` or `go build` will be PGO-enabled."

pgo-clean: ## Remove PGO profile files (cmd/{client,server}/default.pgo).
	rm -f cmd/client/default.pgo cmd/server/default.pgo
	rm -rf .bench/local_snapshot_go/pgo
	@echo "==> PGO profiles cleared."

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
