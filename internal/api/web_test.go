package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUI(t *testing.T) {
	h := newHandler(t, nil)
	tests := []struct {
		path        string
		wantStatus  int
		wantType    string
		wantContain string
	}{
		{path: "/", wantStatus: http.StatusOK, wantType: "text/html", wantContain: `src="/static/app.js"`},
		{path: "/static/app.js", wantStatus: http.StatusOK, wantType: "text/javascript", wantContain: "/api/v1/nodes"},
		{path: "/static/style.css", wantStatus: http.StatusOK, wantType: "text/css"},
		// No directory listings, and unknown paths don't fall back to the UI.
		{path: "/static/", wantStatus: http.StatusNotFound},
		{path: "/nope", wantStatus: http.StatusNotFound},
		{path: "/api/v1/nope", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := get(t, h, tt.path)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tt.wantType) {
				t.Errorf("Content-Type = %q, want prefix %q", ct, tt.wantType)
			}
			if !strings.Contains(rec.Body.String(), tt.wantContain) {
				t.Errorf("body does not contain %q", tt.wantContain)
			}
		})
	}
}

func TestUIRejectsNonGET(t *testing.T) {
	h := newHandler(t, nil)
	for _, m := range []string{http.MethodHead, http.MethodPost} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /: status = %d, want 405", m, rec.Code)
		}
	}
}
