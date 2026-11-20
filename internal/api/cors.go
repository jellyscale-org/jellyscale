package api

import (
	"net/http"
	"strings"
)

// cors opens the API and media routes to a page on any origin (D-016), and never with credentials: a viewer's token
// travels in a header or the query, and no route reads or sets a cookie, so another site's page can only use a token
// it already holds. Like Jellyfin, an answer to a request with an Origin allows every origin with `*`, which needs no
// Vary and which a cache can share.
//
// A preflight is answered here, before the mux and before any token or database is touched: it is a function of the
// request line, the Origin, the two Access-Control-Request-* headers and the route table. The method it allows is the one
// asked, and only when the mux serves it at that path.
func cors(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == "" || !crossOrigin(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
			// What a page needs to read ranges, and how long a refused sign-in is refused: a header is hidden from it
			// unless it is listed.
			h.Set("Access-Control-Expose-Headers", "Content-Range, Accept-Ranges, Content-Length, Retry-After")
			next.ServeHTTP(w, r)
			return
		}
		probe := r.Clone(r.Context())
		probe.Method = r.Header.Get("Access-Control-Request-Method")
		if _, pattern := mux.Handler(probe); !strings.Contains(pattern, " ") { // The catch-all's "/" is no route.
			next.ServeHTTP(w, r) // Its 404, which a page may read.
			return
		}
		h.Set("Access-Control-Allow-Methods", probe.Method)
		// Whatever header the page asks for is allowed, by name: a browser does not take `*` for Authorization.
		if asked := r.Header.Get("Access-Control-Request-Headers"); asked != "" {
			h.Set("Access-Control-Allow-Headers", asked)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// crossOrigin says whether a path is open to other origins: what an operator owns (/healthz, /readyz) and the web
// client's own files are not API or media routes.
func crossOrigin(path string) bool {
	return path != "/healthz" && path != "/readyz" && path != "/" && !strings.HasPrefix(path, "/web/")
}
