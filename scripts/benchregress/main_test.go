// Tests for benchregress parsing & regression detection.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLine_StandardFormat(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantOK  bool
		wantNs  float64
		wantB   float64
		wantAll float64
	}{
		{
			name:    "with-allocs",
			line:    "BenchmarkARQ_PushRecv-8    \t1234567\t   123.4 ns/op\t      16 B/op\t       1 allocs/op",
			wantOK:  true,
			wantNs:  123.4,
			wantB:   16,
			wantAll: 1,
		},
		{
			name:    "no-allocs-extra-fields",
			line:    "BenchmarkParseDNSRequestLiteShort-2    1328679    171.5 ns/op    233.21 MB/s    16 B/op    1 allocs/op",
			wantOK:  true,
			wantNs:  171.5,
			wantB:   16,
			wantAll: 1,
		},
		{
			name:    "zero-bytes",
			line:    "BenchmarkDebugfDisabledGuarded-2    356999884    0.8485 ns/op    0 B/op    0 allocs/op",
			wantOK:  true,
			wantNs:  0.8485,
			wantB:   0,
			wantAll: 0,
		},
		{name: "not-a-bench-line", line: "PASS", wantOK: false},
		{name: "header-line", line: "goos: linux", wantOK: false},
		{name: "empty", line: "", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ok := parseLine(c.line)
			if ok != c.wantOK {
				t.Fatalf("parseLine ok=%v want=%v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if r.NsPerOp != c.wantNs {
				t.Errorf("NsPerOp = %v want %v", r.NsPerOp, c.wantNs)
			}
			if r.BytesPerOp != c.wantB {
				t.Errorf("BytesPerOp = %v want %v", r.BytesPerOp, c.wantB)
			}
			if r.AllocsPerOp != c.wantAll {
				t.Errorf("AllocsPerOp = %v want %v", r.AllocsPerOp, c.wantAll)
			}
		})
	}
}

func TestParseFile_MultipleSamplesAveraged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bench.txt")
	content := `goos: linux
goarch: amd64
BenchmarkFoo-2    100    100.0 ns/op    16 B/op    1 allocs/op
BenchmarkFoo-2    100    200.0 ns/op    16 B/op    1 allocs/op
BenchmarkBar-2    100     50.0 ns/op     8 B/op    0 allocs/op
PASS
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := parseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(results); got != 2 {
		t.Fatalf("len(results) = %d, want 2", got)
	}
	if foo := results["BenchmarkFoo-2"]; foo == nil || foo.NsPerOp != 150.0 {
		t.Errorf("Foo mean: got %+v want 150.0", foo)
	}
	if bar := results["BenchmarkBar-2"]; bar == nil || bar.NsPerOp != 50.0 {
		t.Errorf("Bar mean: got %+v want 50.0", bar)
	}
}

func TestPctDiff(t *testing.T) {
	cases := []struct {
		base, cur, want float64
	}{
		{100, 110, 10},
		{100, 90, -10},
		{100, 100, 0},
		{0, 100, 0}, // guard against div-by-zero
	}
	for _, c := range cases {
		got := pctDiff(c.base, c.cur)
		if got != c.want {
			t.Errorf("pctDiff(%v, %v) = %v want %v", c.base, c.cur, got, c.want)
		}
	}
}

func TestMergeNamesSorted(t *testing.T) {
	a := map[string]*BenchResult{"Zeta": {}, "Alpha": {}}
	b := map[string]*BenchResult{"Beta": {}, "Alpha": {}}
	names := mergeNames(a, b)
	want := []string{"Alpha", "Beta", "Zeta"}
	if len(names) != len(want) {
		t.Fatalf("got %v want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("names[%d] = %q want %q", i, n, want[i])
		}
	}
}

func TestEscapeMD(t *testing.T) {
	got := escapeMD("Bench|With|Pipes")
	want := `Bench\|With\|Pipes`
	if got != want {
		t.Errorf("escapeMD: got %q want %q", got, want)
	}
	if got := escapeMD("Plain"); got != "Plain" {
		t.Errorf("escapeMD plain: got %q", got)
	}
}

func TestFormatFloat(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "—"},
		{123.456, "123.46"},
		{5000, "5000"},
	}
	for _, c := range cases {
		if got := formatFloat(c.in); got != c.want {
			t.Errorf("formatFloat(%v) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestParseFile_RealBenchOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "real.txt")
	content := `# bench-output v1
# date: 2026-05-25T00:00:00Z
# go: go version go1.25.0 linux/amd64
goos: linux
goarch: amd64
pkg: masterdnsvpn-go/internal/logger
cpu: Intel(R) Xeon(R) Processor @ 2.50GHz
BenchmarkDebugfDisabled-2          	 7423400	        48.52 ns/op	       7 B/op	       0 allocs/op
BenchmarkDebugfDisabledGuarded-2   	361895522	         0.8572 ns/op	       0 B/op	       0 allocs/op
PASS
ok  	masterdnsvpn-go/internal/logger	0.970s
BenchmarkParseDNSRequestLiteShort-2                 	 1328679	       171.5 ns/op	 233.21 MB/s	      16 B/op	       1 allocs/op
PASS
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := parseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(results); got != 3 {
		t.Fatalf("len(results) = %d, want 3, keys: %v", got, mapKeys(results))
	}
	if r := results["BenchmarkDebugfDisabled-2"]; r == nil || r.NsPerOp != 48.52 {
		t.Errorf("DebugfDisabled: %+v", r)
	}
	if r := results["BenchmarkParseDNSRequestLiteShort-2"]; r == nil || r.NsPerOp != 171.5 || r.BytesPerOp != 16 {
		t.Errorf("ParseDNS: %+v", r)
	}
}

func mapKeys(m map[string]*BenchResult) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestParseLine_IgnoresComment(t *testing.T) {
	_, ok := parseLine("# bench-output v1")
	if ok {
		t.Error("comment line should not parse as bench")
	}
}

func TestParseFile_Missing(t *testing.T) {
	_, err := parseFile("/nonexistent/path/no-way.txt")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	// OS-specific message; just ensure it's an error
	if !strings.Contains(err.Error(), "no such file") &&
		!strings.Contains(err.Error(), "cannot find") {
		t.Logf("got error: %v (OS-specific message, ok)", err)
	}
}
