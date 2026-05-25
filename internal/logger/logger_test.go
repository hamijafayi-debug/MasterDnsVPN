// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package logger

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{raw: "debug", want: levelDebug},
		{raw: "INFO", want: levelInfo},
		{raw: "warn", want: levelWarn},
		{raw: "warning", want: levelWarn},
		{raw: "critical", want: levelError},
		{raw: "error", want: levelError},
		{raw: "unknown", want: levelInfo},
	}

	for _, tt := range tests {
		if got := parseLevel(tt.raw); got != tt.want {
			t.Fatalf("parseLevel(%q) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

func TestRenderColorTags(t *testing.T) {
	got := renderColorTags("<green>ok</green> <cyan>test</cyan> <unknown>x</unknown>")
	if !strings.Contains(got, "\x1b[32m") {
		t.Fatal("expected green ANSI code in rendered string")
	}
	if !strings.Contains(got, "\x1b[36m") {
		t.Fatal("expected cyan ANSI code in rendered string")
	}
	if !strings.Contains(got, "<unknown>x</unknown>") {
		t.Fatal("unknown tags should be preserved")
	}
}

func TestRenderColorTagsRestoresParentColor(t *testing.T) {
	got := renderColorTags("<green>Listener <cyan>127.0.0.1:5350</cyan> Ready</green>")
	want := "\x1b[32mListener \x1b[36m127.0.0.1:5350\x1b[0m\x1b[32m Ready\x1b[0m"
	if got != want {
		t.Fatalf("renderColorTags() = %q, want %q", got, want)
	}
}

func TestLoggerSuppressesBelowLevel(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{
		name:          "test",
		level:         levelWarn,
		consoleWriter: &buf,
		color:         false,
		appNameText:   "[test]",
	}

	l.Infof("info message")
	l.Warnf("warn message")

	output := buf.String()
	if strings.Contains(output, "info message") {
		t.Fatal("info message should be suppressed at WARN level")
	}
	if !strings.Contains(output, "warn message") {
		t.Fatal("warn message should be logged at WARN level")
	}
}

func TestLevelGuards(t *testing.T) {
	// Construct loggers at every level and assert that the *Enabled
	// predicates return the expected booleans. This is the contract callers
	// in hot paths rely on for cheap branch-out.
	cases := []struct {
		levelStr string
		debug    bool
		info     bool
		warn     bool
		err      bool
	}{
		{"debug", true, true, true, true},
		{"info", false, true, true, true},
		{"warn", false, false, true, true},
		{"error", false, false, false, true},
	}
	for _, c := range cases {
		l := &Logger{level: parseLevel(c.levelStr)}
		if got := l.DebugEnabled(); got != c.debug {
			t.Errorf("[%s] DebugEnabled=%v want %v", c.levelStr, got, c.debug)
		}
		if got := l.InfoEnabled(); got != c.info {
			t.Errorf("[%s] InfoEnabled=%v want %v", c.levelStr, got, c.info)
		}
		if got := l.WarnEnabled(); got != c.warn {
			t.Errorf("[%s] WarnEnabled=%v want %v", c.levelStr, got, c.warn)
		}
		if got := l.ErrorEnabled(); got != c.err {
			t.Errorf("[%s] ErrorEnabled=%v want %v", c.levelStr, got, c.err)
		}
	}
}

func TestEnabledNilLogger(t *testing.T) {
	// A nil *Logger is a valid sentinel for "no logger configured" so
	// callers can skip nil checks. All Enabled variants must return false.
	var l *Logger
	if l.DebugEnabled() || l.InfoEnabled() || l.WarnEnabled() || l.ErrorEnabled() {
		t.Fatal("nil logger should report all levels as disabled")
	}
	if l.Enabled(LevelError) {
		t.Fatal("nil logger Enabled(LevelError) should be false")
	}
}

// BenchmarkDebugfDisabled measures the cost of a Debugf call when the
// logger is configured above DEBUG. The variadic-arg construction still
// happens in the caller, so the recommended pattern is `if log.DebugEnabled()`
// — see BenchmarkDebugfDisabledGuarded.
func BenchmarkDebugfDisabled(b *testing.B) {
	l := &Logger{level: levelError} // debug suppressed
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Debugf("packet %d size=%d addr=%s", i, 1200, "127.0.0.1:12345")
	}
}

// BenchmarkDebugfDisabledGuarded is the form callers should adopt in hot
// paths. With the guard in place this must report 0 B/op and 0 allocs/op
// because the entire format-argument list is never evaluated.
func BenchmarkDebugfDisabledGuarded(b *testing.B) {
	l := &Logger{level: levelError}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if l.DebugEnabled() {
			l.Debugf("packet %d size=%d addr=%s", i, 1200, "127.0.0.1:12345")
		}
	}
}

func TestShouldUseColorHonorsNoColor(t *testing.T) {
	oldNoColor := os.Getenv("NO_COLOR")
	oldForceColor := os.Getenv("FORCE_COLOR")
	t.Cleanup(func() {
		_ = os.Setenv("NO_COLOR", oldNoColor)
		_ = os.Setenv("FORCE_COLOR", oldForceColor)
	})

	_ = os.Setenv("FORCE_COLOR", "1")
	_ = os.Setenv("NO_COLOR", "1")

	if shouldUseColor() {
		t.Fatal("NO_COLOR should disable colors even when FORCE_COLOR is set")
	}
}
