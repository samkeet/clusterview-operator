package api

import (
	"io/fs"
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
		{path: "/static/pools.js", wantStatus: http.StatusOK, wantType: "text/javascript", wantContain: "/api/v1/pools"},
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

// TestUIBuildsDOMWithoutHTMLParsing guards the textContent-only rule: API data
// (pod names, event messages) must never be parsed as HTML. It matches property
// and call syntax, so the header comment that names the rule doesn't trip it.
func TestUIBuildsDOMWithoutHTMLParsing(t *testing.T) {
	for _, name := range []string{"app.js", "pools.js"} {
		js, err := fs.ReadFile(webFS, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{".innerHTML", ".outerHTML", "insertAdjacentHTML(", "document.write(", "eval("} {
			if strings.Contains(string(js), banned) {
				t.Errorf("%s uses %s; build DOM with textContent instead", name, banned)
			}
		}
	}
}

// TestPoolsJSSetsNoInlineStyle guards the CSP (default-src 'self'), which blocks
// inline style attributes. Widths must go through the CSSOM (style.width) and
// colours through classes.
func TestPoolsJSSetsNoInlineStyle(t *testing.T) {
	js, err := fs.ReadFile(webFS, "pools.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{`setAttribute("style"`, "setAttribute('style'", ".cssText", `style="`} {
		if strings.Contains(string(js), banned) {
			t.Errorf("pools.js uses %s, which the CSP blocks or bypasses the class rule", banned)
		}
	}
}

func TestUIHasPoolsRoute(t *testing.T) {
	body := get(t, newHandler(t, nil), "/").Body.String()
	for _, want := range []string{`href="#/"`, `href="#/pools"`, `id="pools-page"`, `src="/static/pools.js"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
}

func TestUIHasPodDrawer(t *testing.T) {
	body := get(t, newHandler(t, nil), "/").Body.String()
	for _, want := range []string{`id="drawer"`, `role="dialog"`, `role="tablist"`, `id="panel-events"`, `id="panel-logs"`, `id="panel-details"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	// The CSP forbids inline script and style, so none may creep in.
	for _, banned := range []string{"<script>", "<style", " style=", " onclick="} {
		if strings.Contains(body, banned) {
			t.Errorf("index.html contains %q, which the CSP blocks", banned)
		}
	}
}
