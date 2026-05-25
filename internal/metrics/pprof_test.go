// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package metrics

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStartPprofEmptyAddrIsNoop(t *testing.T) {
	shutdown, err := StartPprof("", nil)
	if err != nil {
		t.Fatalf("StartPprof empty addr returned err=%v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown func should not be nil even for no-op")
	}
	shutdown() // must not panic
}

func TestStartPprofServesEndpoints(t *testing.T) {
	var banner string
	shutdown, err := StartPprof("127.0.0.1:0", func(s string) { banner = s })
	if err != nil {
		t.Fatalf("StartPprof: %v", err)
	}
	t.Cleanup(shutdown)

	if !strings.Contains(banner, "127.0.0.1:") {
		t.Fatalf("banner missing addr: %q", banner)
	}
	// Extract addr from banner: "pprof/metrics endpoint listening on ADDR"
	idx := strings.LastIndex(banner, " ")
	if idx < 0 {
		t.Fatalf("unexpected banner format: %q", banner)
	}
	addr := banner[idx+1:]

	cli := &http.Client{Timeout: 2 * time.Second}

	// /debug/vars should be valid JSON containing one of our counter names.
	resp, err := cli.Get("http://" + addr + "/debug/vars")
	if err != nil {
		t.Fatalf("GET /debug/vars: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("debug vars status=%d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "masterdnsvpn_packets_in") {
		t.Fatalf("debug vars missing packets_in counter: %s", string(body))
	}

	// /metrics text endpoint should print the prefixed counters.
	resp, err = cli.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status=%d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "masterdnsvpn_packets_in") {
		t.Fatalf("/metrics missing packets_in: %s", string(body))
	}

	// /debug/pprof/ should serve the index page.
	resp, err = cli.Get("http://" + addr + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pprof index status=%d", resp.StatusCode)
	}
}

func TestStartPprofInvalidAddr(t *testing.T) {
	if _, err := StartPprof("not-a-host:abc", nil); err == nil {
		t.Fatal("expected error for invalid addr")
	}
}

func TestIntToString(t *testing.T) {
	cases := map[int64]string{
		0:      "0",
		1:      "1",
		-1:     "-1",
		12345:  "12345",
		-99999: "-99999",
	}
	for in, want := range cases {
		if got := intToString(in); got != want {
			t.Errorf("intToString(%d)=%q want %q", in, got, want)
		}
	}
}
