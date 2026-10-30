package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/auth"
	"github.com/vBuilt/markham-schedule/internal/store"
	"github.com/vBuilt/markham-schedule/internal/watch"
)

// authHTTP returns account handlers over a real store plus a cookie for a
// signed-in user (confirmed or not).
func authHTTP(t *testing.T, apiToken string, confirmed bool) (*auth.HTTP, *http.Cookie) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := auth.NewService(st, auth.Options{
		BaseURL: "http://localhost", APIToken: apiToken,
		Async: func(f func()) { f() },
	})
	ctx := context.Background()
	hash, err := auth.HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(ctx, "a@example.com", hash, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if confirmed {
		if err := st.ConfirmUser(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	raw, _, err := svc.SignIn(ctx, "a@example.com", "correct horse", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	return svc.Handler(auth.HTTPOptions{BaseURL: "http://localhost"}), &http.Cookie{Name: "mds_session", Value: raw}
}

func postWith(h http.Handler, target string, c *http.Cookie, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", target, nil)
	if c != nil {
		r.AddCookie(c)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRefreshNeedsSignIn(t *testing.T) {
	ah, cookie := authHTTP(t, "", true)
	fr := &fakeRefresher{}
	h := newH(&fakeStore{}, Options{Refresher: fr, Auth: ah})
	if w := postWith(h, "/api/refresh", nil, "http://example.com"); w.Code != 401 || fr.calls != 0 {
		t.Fatal("no cookie:", w.Code, fr.calls, w.Body.String())
	}
	if w := postWith(h, "/api/refresh", cookie, "http://evil.example"); w.Code != 403 || fr.calls != 0 {
		t.Fatal("cross-site:", w.Code, fr.calls)
	}
	if w := postWith(h, "/api/refresh", cookie, "http://example.com"); w.Code != 202 || fr.calls != 1 {
		t.Fatal("signed in:", w.Code, fr.calls, w.Body.String())
	}

	ah, cookie = authHTTP(t, "", false)
	fr = &fakeRefresher{}
	h = newH(&fakeStore{}, Options{Refresher: fr, Auth: ah})
	if w := postWith(h, "/api/refresh", cookie, "http://example.com"); w.Code != 403 || fr.calls != 0 {
		t.Fatal("unconfirmed:", w.Code, fr.calls)
	}

	ah, cookie = authHTTP(t, "k", true)
	h = newH(&fakeStore{}, Options{Token: "k", Refresher: fr, Auth: ah})
	if w := postWith(h, "/api/refresh", cookie, "http://example.com"); w.Code != 401 || fr.calls != 0 {
		t.Fatal("API token still required:", w.Code, fr.calls)
	}
}

func TestPublicRoutesStayOpen(t *testing.T) {
	ah, _ := authHTTP(t, "", true)
	h := newH(&fakeStore{}, Options{Refresher: &fakeRefresher{}, Auth: ah})
	for _, p := range []string{"/", "/status", "/calendar.ics", "/api/sessions", "/api/status", "/login", "/forgot", "/reset", "/verify", "/account", "/api/auth/me"} {
		if w := get(h, p); w.Code != 200 {
			t.Errorf("GET %s = %d", p, w.Code)
		}
	}
	ah, _ = authHTTP(t, "k", true)
	h = newH(&fakeStore{}, Options{Token: "k", Auth: ah})
	if w := get(h, "/api/sessions?token=k"); w.Code != 200 {
		t.Error("with token:", w.Code)
	}
	if w := get(h, "/api/auth/me"); w.Code != 401 {
		t.Error("auth/me without token:", w.Code)
	}
	if w := get(h, "/login"); w.Code != 401 {
		t.Error("login page without token:", w.Code)
	}
}

func TestWatchRoutesWired(t *testing.T) {
	ah, _ := authHTTP(t, "", true)
	h := newH(&fakeStore{}, Options{Auth: ah, Watch: watch.NewHTTP(nil, nil, ah)})
	for _, p := range []string{"/api/watches", "/api/watches/all"} {
		r := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s signed out = %d", p, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/status", nil))
	if w.Code != 200 {
		t.Fatalf("/api/status = %d", w.Code)
	}
}
