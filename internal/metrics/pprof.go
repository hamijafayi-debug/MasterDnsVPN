// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
	"sync"
	"time"
)

// StartPprof launches an HTTP listener that serves the standard net/http/pprof
// debug endpoints together with the expvar metrics registered in metrics.go.
//
// If addr is empty the function is a no-op and returns nil — this lets
// production builds leave the knob unset without any runtime cost.
// Otherwise addr must be a valid host:port pair accepted by net.Listen.
//
// A separate http.ServeMux is used (rather than the default mux) so importing
// this package never mutates the global handler tree. That keeps the rest of
// the binary safe from surprise endpoints.
//
// The returned shutdown function gracefully stops the server with a short
// timeout. It is safe to call multiple times.
//
// The labelf callback, if non-nil, receives a one-line human readable banner
// once the listener is ready (e.g. "pprof listening on 127.0.0.1:6060"). This
// allows the caller to route the line through its own logger without this
// package having to know about the logger type.
func StartPprof(addr string, labelf func(string)) (shutdown func(), err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return func() {}, nil
	}

	mux := http.NewServeMux()
	// Standard pprof handlers. Index serves /debug/pprof/ and the per-profile
	// subhandlers are registered by name so the URLs match the upstream
	// documentation.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// expvar registers itself on the *default* mux at /debug/vars. We mirror
	// the same path on our private mux so neither namespace leaks into the
	// default handler set.
	mux.Handle("/debug/vars", expvarHandler())

	// A trivial text endpoint that prints the well-known counters in a
	// scrape-friendly key=value form. Useful for grep/jq pipelines without
	// having to parse JSON.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, s := range Collect() {
			// Format: name value\n — Prometheus-compatible for the simple
			// counters we expose today. Histograms / labels are out of scope.
			_, _ = w.Write([]byte("masterdnsvpn_"))
			_, _ = w.Write([]byte(s.Name))
			_, _ = w.Write([]byte(" "))
			_, _ = w.Write([]byte(formatInt(s.Value)))
			_, _ = w.Write([]byte("\n"))
		}
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if labelf != nil {
		labelf("pprof/metrics endpoint listening on " + ln.Addr().String())
	}

	var (
		once sync.Once
		done = make(chan struct{})
	)

	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if labelf != nil {
				labelf("pprof server exited with error: " + err.Error())
			}
		}
	}()

	shutdown = func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
			<-done
		})
	}
	return shutdown, nil
}

// formatInt avoids pulling strconv into the hot path of /metrics callers
// that may be locked behind a context that disallows allocation in the
// future. For now it is a thin wrapper but keeps the call site obvious.
func formatInt(v int64) string {
	return intToString(v)
}

// intToString is a tiny, allocation-conscious integer-to-decimal converter.
// It exists so we do not depend on strconv from this file — keeping the
// import set minimal makes auditing the pprof surface easier.
func intToString(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
