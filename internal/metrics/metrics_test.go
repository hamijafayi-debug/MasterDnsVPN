// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package metrics

import (
	"expvar"
	"sync"
	"testing"
)

func TestCounterAddAndValue(t *testing.T) {
	var c Counter
	if got := c.Value(); got != 0 {
		t.Fatalf("fresh counter value=%d want 0", got)
	}
	c.Add(5)
	c.Add(3)
	if got := c.Value(); got != 8 {
		t.Fatalf("after Add(5)+Add(3) value=%d want 8", got)
	}
	c.Add(-1)
	if got := c.Value(); got != 7 {
		t.Fatalf("after Add(-1) value=%d want 7", got)
	}
}

func TestCounterSet(t *testing.T) {
	var c Counter
	c.Set(42)
	if got := c.Value(); got != 42 {
		t.Fatalf("after Set(42) value=%d want 42", got)
	}
	c.Set(0)
	if got := c.Value(); got != 0 {
		t.Fatalf("after Set(0) value=%d want 0", got)
	}
}

func TestCounterNilSafe(t *testing.T) {
	var c *Counter
	// All operations on a nil counter must be no-ops so that callers can
	// keep optional metrics behind a pointer without nil-checking every site.
	c.Add(1)
	c.Set(1)
	if got := c.Value(); got != 0 {
		t.Fatalf("nil counter value=%d want 0", got)
	}
}

func TestCounterConcurrent(t *testing.T) {
	var c Counter
	const workers = 16
	const perWorker = 1000
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				c.Add(1)
			}
		}()
	}
	wg.Wait()
	if got, want := c.Value(), int64(workers*perWorker); got != want {
		t.Fatalf("concurrent Add value=%d want %d", got, want)
	}
}

func TestExpvarRegistration(t *testing.T) {
	// All well-known counters must be reachable via expvar so /debug/vars
	// works without any extra wiring.
	names := []string{
		"masterdnsvpn_packets_in",
		"masterdnsvpn_packets_out",
		"masterdnsvpn_bytes_in",
		"masterdnsvpn_bytes_out",
		"masterdnsvpn_arq_retx",
		"masterdnsvpn_arq_duplicate_rx",
		"masterdnsvpn_sessions_active",
		"masterdnsvpn_cache_hits",
		"masterdnsvpn_cache_misses",
	}
	for _, n := range names {
		if v := expvar.Get(n); v == nil {
			t.Errorf("expvar metric %q not registered", n)
		}
	}
}

func TestCollectStableOrder(t *testing.T) {
	snap := Collect()
	want := []string{
		"packets_in",
		"packets_out",
		"bytes_in",
		"bytes_out",
		"arq_retx",
		"arq_duplicate_rx",
		"sessions_active",
		"cache_hits",
		"cache_misses",
	}
	if len(snap) != len(want) {
		t.Fatalf("snapshot length=%d want %d", len(snap), len(want))
	}
	for i, s := range snap {
		if s.Name != want[i] {
			t.Errorf("snapshot[%d]=%q want %q", i, s.Name, want[i])
		}
	}
}
