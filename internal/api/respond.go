package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// writeJSON sends v with a strong ETag over the exact body bytes. The UI polls
// every few seconds, and a 304 saves both the transfer and the re-render.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.serverError(w, r, fmt.Errorf("encoding response: %w", err))
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`

	h := w.Header()
	h.Set("ETag", etag)
	// Let caches store it but force revalidation, so nobody sees a stale snapshot.
	h.Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Values("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	if _, err := w.Write(body); err != nil {
		// Usually the client went away mid-response; nothing to recover.
		s.Log.V(1).Info("Writing response failed", "path", r.URL.Path, "err", err)
	}
}

// etagMatches implements If-None-Match: a list of tags or "*", compared weakly
// (RFC 9110 §13.1.2), so a proxy that weakened our tag to W/"..." still gets a 304.
func etagMatches(headers []string, etag string) bool {
	for _, h := range headers {
		for tag := range strings.SplitSeq(h, ",") {
			tag = strings.TrimSpace(tag)
			if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
				return true
			}
		}
	}
	return false
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	body, _ := json.Marshal(errorResponse{Error: msg}) // a struct of one string can't fail
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// serverError logs the detail and returns a generic message, so cache and
// internal error text never leaks to an unauthenticated client.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error(err, "Serving request failed", "path", r.URL.Path)
	writeError(w, http.StatusInternalServerError, "internal error")
}
