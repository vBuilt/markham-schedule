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

func TestWatchStopReferrerPolicy(t *testing.T) {
	body := watchStop
	if !strings.Contains(body, `<meta name="referrer" content="same-origin">`) {
		t.Error("stop page must use referrer policy same-origin")
	}
	if strings.Contains(body, `content="no-referrer"`) {
		t.Error("no-referrer makes the Stop POST send Origin: null and fail CSRF")
	}
}

func TestReminderCancelPage(t *testing.T) {
	w := httptest.NewRecorder()
	ReminderCancel(w, "Cancel this registration reminder?", "tok", "abc")
	b := w.Body.String()
	for _, m := range []string{"<title>Reminder cancelled", `action="/reminder/cancel?token=abc"`, "Cancel reminder</button>", `value="tok"`} {
		if !strings.Contains(b, m) {
			t.Errorf("page lacks %q", m)
		}
	}
	w = httptest.NewRecorder()
	WatchStop(w, "m", "tok", "")
	if b := w.Body.String(); !strings.Contains(b, "Stop watching</button>") || !strings.Contains(b, `action="/watch/stop"`) || !strings.Contains(b, "<title>Watch stopped") {
		t.Errorf("WatchStop changed: %s", b)
	}
}
