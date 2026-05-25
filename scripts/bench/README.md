# MasterDnsVPN Benchmark Suite

This directory contains the core benchmarking tools for MasterDnsVPN, now enhanced with high-precision timing and standalone tool capabilities inspired by the `slipstream-rust` methodology.

## Tools

### 1. `bench.go` (Go-based Orchestrator and Benchmarker)

The primary tool for end-to-end performance testing. It builds the server and client, orchestrates a local tunnel, and measures throughput using **First-Byte Timing**.

#### High-Precision Timing
Unlike simple timers, `bench.go` starts its measurement only when the **first byte** of the actual payload is sent or received. This ensures that connection establishment and handshake overheads do not skew the results, providing a true measure of tunnel throughput.

#### Usage (Full Orchestration)

To run a standard end-to-end benchmark through the MasterDnsVPN tunnel:

```bash
go run scripts/bench/bench.go -runs 3 -bytes 10485760
```

#### CLI Options
| Flag | Description | Default |
|------|-------------|---------|
| `-runs` | Number of runs for each direction | 3 |
| `-bytes` | Total payload size in bytes | 10MiB |
| `-force-build` | Rebuild server and client binaries | true |
| `-client-port` | Port for the local client listener | 18080 |
| `-server-port` | Port for the UDP server listener | 5300 |

---

### 2. Standalone Mode (Tool Mode)

`bench.go` can also be used as a standalone source/sink tool, similar to `tcp_bench.py`. This is useful for testing manual configurations or other TCP links.

#### Modes
- `sink`: Listens for a connection and discards received data (sends "OK" at the end).
- `source`: Listens for a connection and sends data.
- `send`: Connects to a target and sends data (waits for "OK" at the end).
- `recv`: Connects to a target and receives data.

#### Examples

**Start a sink server (receiver):**
```bash
go run scripts/bench/bench.go -mode sink -addr :9090
```

**Run a sender (client):**
```bash
go run scripts/bench/bench.go -mode send -addr 127.0.0.1:9090 -bytes 100000000
```

**JSON Output:**
To get raw data for analysis:
```bash
go run scripts/bench/bench.go -mode send -addr 127.0.0.1:9090 -json
```

---

## Directory Structure

- `.bench/local_snapshot_go/bin`: Compiled benchmark binaries.
- `.bench/local_snapshot_go/runtime`: Temporary configuration and log files.

## Methodology

1. **First-Byte Start**: The timer starts on the first successful `Read` or `Write` of the payload.
2. **ACK Synchronization**: For "Exfil" scenarios, the sink sends an "OK" acknowledgment to ensure all data has cleared the tunnel before the timer stops.
3. **Monotonic Timing**: Uses Go's monotonic clock for sub-millisecond precision.

---

## Make targets (Step 1)

The repository root ships a small `Makefile` so the harness can be exercised
without remembering the long `go run` invocations. The relevant recipes are:

| Target            | What it does                                                   |
| ----------------- | -------------------------------------------------------------- |
| `make bench`      | Runs the harness against lossless localhost (10 MiB × 3 runs). |
| `make bench-loss` | Prints the `tc qdisc netem` recipe for lossy-link simulation.  |
| `make pprof-client` | Starts the client with `PPROF_ADDR=127.0.0.1:6060`.          |
| `make pprof-server` | Starts the server with `PPROF_ADDR=127.0.0.1:6061`.          |

Tunables: `BENCH_RUNS`, `BENCH_BYTES`, `PPROF_CLIENT`, `PPROF_SERVER`.

## Observability endpoints

When `PPROF_ADDR` is set (e.g. `PPROF_ADDR=127.0.0.1:6060 ./bin/client`) the
binary exposes:

- `http://<addr>/debug/pprof/` — standard `net/http/pprof` index (cpu/heap/goroutine).
- `http://<addr>/debug/vars`   — full `expvar` JSON registry.
- `http://<addr>/metrics`      — Prometheus-style key/value text dump of the
  counters declared in `internal/metrics`.

The endpoint is bound to a private `http.ServeMux`, so importing the metrics
package never mutates the default mux.

## Lossy-link simulation

The Go harness does not emulate packet loss itself — it relies on Linux's
`netem` qdisc. Typical recipe on the loopback device:

```bash
sudo tc qdisc add dev lo root netem loss 5% delay 5ms
make bench
sudo tc qdisc del dev lo root
```

Substitute `loss 1%` / `delay 1ms` for the milder scenario.
