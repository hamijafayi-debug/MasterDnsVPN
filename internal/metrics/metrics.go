// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

// Package metrics provides a tiny, zero-dependency counter / gauge surface
// based on the standard-library expvar package. The intent is to give
// operators a low-overhead window into hot-path behaviour (packet rates,
// retransmits, active sessions, ...) without pulling Prometheus or any other
// third-party module.
//
// All counters are safe for concurrent use. Each metric is also published
// under expvar so it shows up at /debug/vars when pprof is enabled.
//
// Usage:
//
//	metrics.PacketsIn.Add(1)
//	metrics.BytesOut.Add(int64(n))
//	metrics.SessionsActive.Set(int64(count))
//
// The package intentionally avoids any heavy initialisation work or
// reflection so it is safe to import from very hot paths.
package metrics

import (
	"expvar"
	"sync/atomic"
)

// Counter is a monotonically-increasing 64-bit integer metric. It is a thin
// wrapper around atomic.Int64 that also keeps an expvar.Func registered so
// the value is reachable from /debug/vars without exposing the underlying
// pointer to callers.
type Counter struct {
	v atomic.Int64
}

// Add atomically increments the counter by delta. delta may be negative to
// accommodate corrections (e.g. session count decrements).
func (c *Counter) Add(delta int64) {
	if c == nil {
		return
	}
	c.v.Add(delta)
}

// Set atomically replaces the stored value. Useful for gauges that are
// recomputed periodically.
func (c *Counter) Set(value int64) {
	if c == nil {
		return
	}
	c.v.Store(value)
}

// Value atomically reads the current value.
func (c *Counter) Value() int64 {
	if c == nil {
		return 0
	}
	return c.v.Load()
}

// register attaches the counter to the global expvar map under name so it
// is reachable from /debug/vars. It is idempotent — duplicate names are
// silently ignored to keep init() simple.
func register(name string, c *Counter) {
	// expvar.Publish panics on duplicate names; we want metric registration
	// to be tolerant of test re-imports, so check first.
	if expvar.Get(name) != nil {
		return
	}
	expvar.Publish(name, expvar.Func(func() any {
		return c.Value()
	}))
}

// Pre-declared counters cover the high-level traffic and ARQ behaviour that
// every step in PLAN.md cares about. Additional metrics should be added as
// new exported Counter values here so that callers never need to construct
// their own.
var (
	// PacketsIn counts the number of inbound UDP/DNS packets the server or
	// client has processed end-to-end.
	PacketsIn = &Counter{}

	// PacketsOut counts the number of outbound UDP/DNS packets emitted by
	// the local node.
	PacketsOut = &Counter{}

	// BytesIn counts the total bytes accepted from the network (after the
	// DNS framing has been stripped). This is the application-visible
	// number, not the on-wire byte count.
	BytesIn = &Counter{}

	// BytesOut counts the total bytes handed to the network for
	// transmission (post-framing, pre-encryption is acceptable as long as
	// the meaning is stable across releases).
	BytesOut = &Counter{}

	// ArqRetx counts the number of ARQ retransmissions issued by the
	// local node. Spikes here indicate adverse network conditions or RTO
	// mistuning.
	ArqRetx = &Counter{}

	// ArqDuplicateRx counts duplicate ARQ data segments that were
	// successfully discarded by the receiver. Useful to evaluate adaptive
	// duplication policy effectiveness (step 14).
	ArqDuplicateRx = &Counter{}

	// SessionsActive is a gauge: number of sessions currently considered
	// alive by the local node.
	SessionsActive = &Counter{}

	// CacheHits / CacheMisses track the DNS cache layer (step 16). They
	// are declared here up-front so wiring in later steps is mechanical.
	CacheHits   = &Counter{}
	CacheMisses = &Counter{}
)

func init() {
	register("masterdnsvpn_packets_in", PacketsIn)
	register("masterdnsvpn_packets_out", PacketsOut)
	register("masterdnsvpn_bytes_in", BytesIn)
	register("masterdnsvpn_bytes_out", BytesOut)
	register("masterdnsvpn_arq_retx", ArqRetx)
	register("masterdnsvpn_arq_duplicate_rx", ArqDuplicateRx)
	register("masterdnsvpn_sessions_active", SessionsActive)
	register("masterdnsvpn_cache_hits", CacheHits)
	register("masterdnsvpn_cache_misses", CacheMisses)
}

// Snapshot captures every well-known counter at a single point in time. It is
// intended for diagnostic dumps (e.g. a /metrics text endpoint or periodic
// log lines). The slice is allocated fresh on each call — this is not a
// hot-path API.
type Snapshot struct {
	Name  string
	Value int64
}

// Collect returns a deterministic ordering of (name, value) pairs covering
// every counter declared above. The order is stable so diff-friendly logs
// stay readable across releases.
func Collect() []Snapshot {
	return []Snapshot{
		{"packets_in", PacketsIn.Value()},
		{"packets_out", PacketsOut.Value()},
		{"bytes_in", BytesIn.Value()},
		{"bytes_out", BytesOut.Value()},
		{"arq_retx", ArqRetx.Value()},
		{"arq_duplicate_rx", ArqDuplicateRx.Value()},
		{"sessions_active", SessionsActive.Value()},
		{"cache_hits", CacheHits.Value()},
		{"cache_misses", CacheMisses.Value()},
	}
}
