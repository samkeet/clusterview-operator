// Package api serves the read-only JSON API and the web UI. Every read comes from
// the manager's shared informer cache, so a request never reaches the API server
// and the kubelets are never contacted.
package api

import (
	"net/http"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Server is the HTTP API and UI.
type Server struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// Reader must be the manager's cache (mgr.GetCache()), not a live client.
	Reader client.Reader
	// ClusterViewName names the ClusterView whose spec.namespaces filters pods.
	ClusterViewName string
	Log             logr.Logger
}

// Handler returns the routes wrapped in the GET-only and security-header middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Patterns carry no method on purpose: getOnly rejects everything but GET up
	// front, whereas a "GET /x" pattern would also answer HEAD.
	mux.HandleFunc("/api/v1/summary", s.summary)
	mux.HandleFunc("/api/v1/nodes", s.nodes)
	mux.HandleFunc("/api/v1/nodes/{name}/pods", s.nodePods)
	mux.HandleFunc("/healthz", healthz)
	return securityHeaders(getOnly(mux))
}

// getOnly enforces the read-only contract: the API and UI answer GET and nothing else.
func getOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders applies to every response. The CSP forbids inline script and
// framing; that's why the UI's JS and CSS live in separate files. No CORS headers
// are sent, so other origins can't read the (unauthenticated) API from a browser.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
