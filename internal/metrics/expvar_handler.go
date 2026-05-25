// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package metrics

import (
	"expvar"
	"fmt"
	"net/http"
)

// expvarHandler returns an http.Handler that renders the entire expvar
// registry as JSON. We duplicate the upstream implementation (which lives in
// expvar.init() and is only reachable via http.DefaultServeMux) so that
// mounting our private pprof mux does not require touching the default mux.
func expvarHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = fmt.Fprint(w, "{\n")
		first := true
		expvar.Do(func(kv expvar.KeyValue) {
			if !first {
				_, _ = fmt.Fprint(w, ",\n")
			}
			first = false
			_, _ = fmt.Fprintf(w, "%q: %s", kv.Key, kv.Value)
		})
		_, _ = fmt.Fprint(w, "\n}\n")
	})
}
