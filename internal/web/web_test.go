package web

import (
	"net/http"
	"net/http/httptest"
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
