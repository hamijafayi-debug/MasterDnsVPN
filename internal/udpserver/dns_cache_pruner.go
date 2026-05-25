// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
//
// Step 18 — DNS cache amortized pruner.
//
// The dnscache.Store already expires entries lazily on read (GetReady drops
// any entry whose LastUsedAt is older than cacheTTL). Lazy expiry is fine
// for hot keys but leaves a long tail of cold-but-expired entries sitting
// in the cold tier forever — they only get evicted when a new insert
// pushes the shard over its capacity limit.
//
// On a busy server with millions of distinct queries that tail can grow to
// the per-shard cap (DNSCacheMaxRecords/32) and stay there, wasting memory
// and CPU on every shard.Iterate (SaveToFile, ClearPending, etc).
//
// The pruner runs in the background, walks a bounded slice of each shard
// per tick (Store.PruneExpired enforces maxScanPerShard), and drops any
// already-expired entries it finds. The Store maintains a per-shard cursor
// so successive ticks cover the whole cache without ever holding a shard
// mutex for longer than ~maxScanPerShard map operations.
//
// Local-only: no DNS/UDP wire format changes.
package udpserver

import (
	"context"
	"time"
)

func (s *Server) runDNSCachePruner(ctx context.Context, interval time.Duration, maxScanPerShard int) {
	if s == nil || s.dnsCache == nil {
		return
	}
	if interval <= 0 {
		return
	}
	if maxScanPerShard <= 0 {
		maxScanPerShard = 32
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			removed := s.dnsCache.PruneExpired(now, maxScanPerShard)
			if removed > 0 && s.log != nil && s.log.DebugEnabled() {
				s.log.Debugf(
					"🧹 <green>DNS Cache Prune</green> <magenta>|</magenta> <blue>Removed</blue>: <cyan>%d</cyan>",
					removed,
				)
			}
		}
	}
}
