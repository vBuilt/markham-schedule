package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTurnstile(t *testing.T) {
	reply, code, calls := `{"success":true}`, 200, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = r.ParseForm()
		if r.PostForm.Get("secret") != "s" || r.PostForm.Get("response") != "tok" || r.PostForm.Get("remoteip") != "1.2.3.4" {
			t.Errorf("form = %v", r.PostForm)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(reply))
	}))
	ts := &Turnstile{Secret: "s", Endpoint: srv.URL}
	ctx := context.Background()
	if err := ts.Verify(ctx, "tok", "1.2.3.4"); err != nil {
		t.Errorf("success: %v", err)
	}
	reply = `{"success":false}`
	if err := ts.Verify(ctx, "tok", "1.2.3.4"); err != ErrBot {
		t.Errorf("fail: %v", err)
	}
	reply, code = `oops`, 500
	if err := ts.Verify(ctx, "tok", "1.2.3.4"); err != ErrBotUnavailable {
		t.Errorf("500: %v", err)
	}
	before := calls
	if err := ts.Verify(ctx, "", "1.2.3.4"); err != ErrBot || calls != before {
		t.Errorf("empty token: %v calls %d->%d", err, before, calls)
	}
	srv.Close()
	if err := ts.Verify(ctx, "tok", "1.2.3.4"); err != ErrBotUnavailable {
		t.Errorf("unreachable: %v", err)
	}
	if err := (NoBotCheck{}).Verify(ctx, "", ""); err != nil {
		t.Error(err)
	}
}
