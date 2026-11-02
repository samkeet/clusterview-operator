// Package api serves the read-only JSON API and the web UI. Every read comes from
// the manager's shared informer cache, so a request never reaches the API server
// and the kubelets are never contacted.
package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// writeTimeout leaves room for a large /nodes body on a slow link.
	writeTimeout = 30 * time.Second
	// shutdownTimeout fits inside the pod's 10s terminationGracePeriodSeconds.
	shutdownTimeout = 5 * time.Second
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

// NeedLeaderElection lets every replica serve: the server is read-only and the
// informer cache runs on non-leaders too. As a non-leader runnable it's started
// only after the cache has synced, so it never serves a cold cache.
func (s *Server) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable. It blocks until ctx is cancelled, then shuts
// down gracefully so in-flight requests can finish.
func (s *Server) Start(ctx context.Context) error {
	// Listen synchronously so a bad or busy address fails the manager at startup.
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.Addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// No BaseContext from ctx: that would cancel in-flight requests the moment
		// shutdown starts, defeating the graceful drain below.
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	s.Log.Info("Serving API and UI", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		return fmt.Errorf("serving HTTP: %w", err)
	case <-ctx.Done():
	}

	s.Log.Info("Shutting down API server")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down HTTP server: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving HTTP: %w", err)
	}
	return nil
}

// Handler returns the routes wrapped in the GET-only and security-header middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Patterns carry no method on purpose: getOnly rejects everything but GET up
	// front, whereas a "GET /x" pattern would also answer HEAD.
	mux.HandleFunc("/api/v1/summary", s.summary)
	mux.HandleFunc("/api/v1/pools", s.pools)
	mux.HandleFunc("/api/v1/nodes", s.nodes)
	mux.HandleFunc("/api/v1/nodes/{name}/pods", s.nodePods)
	mux.HandleFunc("/api/v1/namespaces/{ns}/pods/{name}/events", s.podEvents)
	mux.HandleFunc("/healthz", healthz)
	// "/{$}" matches only the root, so unknown paths 404 instead of serving the UI.
	mux.HandleFunc("/{$}", index)
	mux.Handle("/static/", static())
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
