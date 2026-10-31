package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPagesRefuseFraming(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"index": Index, "status": Status, "login": Login, "forgot": Forgot,
		"reset": Reset, "verify": Verify, "account": Account,
	} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", "/", nil))
		if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", name, got)
		}
		if got := w.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
			t.Errorf("%s: Content-Security-Policy = %q", name, got)
		}
	}
}

func TestWatchUIMarkers(t *testing.T) {
	for name, tc := range map[string]struct {
		h    http.HandlerFunc
		want []string
	}{
		"index":  {Index, []string{"Email me when a spot opens", "Stop watching", "/api/watches"}},
		"status": {Status, []string{"Sessions you're watching", "All watches", "/api/watches/all"}},
	} {
		w := httptest.NewRecorder()
		tc.h(w, httptest.NewRequest("GET", "/", nil))
		body := w.Body.String()
		for _, m := range tc.want {
			if !strings.Contains(body, m) {
				t.Errorf("%s page lacks %q", name, m)
			}
		}
	}
}
