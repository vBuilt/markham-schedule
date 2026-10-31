package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/auth"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
)

type httpEnv struct {
	st      *store.Store
	svc     *auth.Service
	handler http.Handler
	now     time.Time
	byCat   map[string][]schedule.Session
}

func ip(n int) *int { return &n }

func newHTTPEnv(t *testing.T) *httpEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := auth.NewService(st, auth.Options{
		BaseURL: "http://localhost", Async: func(f func()) { f() },
		AdminEmails: []string{"admin@example.com"},
	})
	ah := svc.Handler(auth.HTTPOptions{BaseURL: "http://localhost"})
	c := New(&fakeSource{}, st, &fakeMailer{}, Config{Now: func() time.Time { return t0 }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	mux := http.NewServeMux()
	NewHTTP(c, st, ah).Register(mux, func(f http.HandlerFunc) http.HandlerFunc { return f })
	return &httpEnv{st: st, svc: svc, handler: ah.Middleware(mux), now: t0, byCat: map[string][]schedule.Session{}}
}

// user creates a user and returns a session cookie for it.
func (e *httpEnv) user(t *testing.T, email string, confirmed bool) *http.Cookie {
	t.Helper()
	hash, _ := auth.HashPassword("correct horse")
	u, err := e.st.CreateUser(context.Background(), email, hash, e.now)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed {
		if err := e.st.ConfirmUser(context.Background(), u.ID); err != nil {
			t.Fatal(err)
		}
	}
	raw, _, err := e.svc.SignIn(context.Background(), email, "correct horse", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "mds_session", Value: raw}
}

// session stores one session in its own category.
func (e *httpEnv) session(t *testing.T, id, cat string, start time.Time, spots string, left *int) schedule.Session {
	t.Helper()
	s := schedule.Session{ID: id, Name: "Swim " + id, Categories: []string{cat}, Centre: "Centennial",
		Start: start, End: start.Add(time.Hour), Spots: spots, SpotsLeft: left, BookingURL: "https://x/" + id}
	e.byCat[cat] = append(e.byCat[cat], s)
	if err := e.st.ReplaceCategory(context.Background(), cat, t0.Add(-24*time.Hour), e.byCat[cat]); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *httpEnv) do(method, path string, body any, c *http.Cookie, origin string) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, "http://localhost"+path, rd)
	if c != nil {
		r.AddCookie(c)
	}
	if method == "POST" {
		if origin == "" {
			origin = "http://localhost"
		}
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return w
}

func errOf(w *httptest.ResponseRecorder) string {
	var v struct{ Error string }
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v.Error
}

func TestWatchEndpointsNeedConfirmed(t *testing.T) {
	e := newHTTPEnv(t)
	e.session(t, "s1", "Swim", t0.Add(24*time.Hour), "Full", ip(0))
	confirmed := e.user(t, "a@example.com", true)
	unconfirmed := e.user(t, "b@example.com", false)
	for _, rq := range [][2]string{{"GET", "/api/watches"}, {"POST", "/api/watches"}, {"POST", "/api/watches/delete"}, {"GET", "/api/watches/all"}} {
		if w := e.do(rq[0], rq[1], map[string]string{"session_id": "s1"}, nil, ""); w.Code != 401 {
			t.Errorf("%v signed out = %d", rq, w.Code)
		}
		if w := e.do(rq[0], rq[1], map[string]string{"session_id": "s1"}, unconfirmed, ""); w.Code != 403 {
			t.Errorf("%v unconfirmed = %d", rq, w.Code)
		}
	}
	for _, p := range []string{"/api/watches", "/api/watches/delete"} {
		if w := e.do("POST", p, map[string]string{"session_id": "s1"}, confirmed, "http://evil.example"); w.Code != 403 {
			t.Errorf("cross-site %s = %d", p, w.Code)
		}
	}
	if ws, _ := e.st.AllWatches(context.Background()); len(ws) != 0 {
		t.Fatalf("watches created: %+v", ws)
	}
}

func TestAddWatchValidation(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	add := func(id string) *httptest.ResponseRecorder {
		return e.do("POST", "/api/watches", map[string]string{"session_id": id}, c, "")
	}
	future := t0.Add(24 * time.Hour)
	e.session(t, "past", "Swim", t0.Add(-time.Hour), "Full", ip(0))
	e.session(t, "open", "Swim", future, "5 spots", ip(5))
	e.session(t, "full", "Swim", future, "Full", ip(0))
	e.session(t, "txt", "Swim", future, " FULL ", nil)

	if w := add("nope"); w.Code != 404 || errOf(w) != "Session not found." {
		t.Fatalf("404: %d %s", w.Code, w.Body)
	}
	if w := add("past"); w.Code != 409 || errOf(w) != "This session has already started." {
		t.Fatalf("started: %d %s", w.Code, w.Body)
	}
	if w := add("open"); w.Code != 409 || errOf(w) != "This session isn't full." {
		t.Fatalf("not full: %d %s", w.Code, w.Body)
	}
	if w := add("full"); w.Code != 201 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if w := add("full"); w.Code != 200 {
		t.Fatalf("dup: %d %s", w.Code, w.Body)
	}
	if w := add("txt"); w.Code != 201 {
		t.Fatalf("full by text: %d %s", w.Code, w.Body)
	}
	for i := 2; i < MaxPerUser; i++ {
		id := fmt.Sprintf("x%d", i)
		e.session(t, id, "Swim", future, "Full", ip(0))
		if w := add(id); w.Code != 201 {
			t.Fatalf("add %s: %d %s", id, w.Code, w.Body)
		}
	}
	e.session(t, "eleven", "Swim", future, "Full", ip(0))
	if w := add("eleven"); w.Code != 429 || errOf(w) != "You can watch at most 10 sessions at a time." {
		t.Fatalf("11th: %d %s", w.Code, w.Body)
	}
	if w := add("full"); w.Code != 200 {
		t.Fatalf("dup at limit: %d", w.Code)
	}
}

func TestAddWatchGroupCapacity(t *testing.T) {
	e := newHTTPEnv(t)
	future := t0.Add(24 * time.Hour)
	// 20 groups, spread over users (10 per user).
	for g := 0; g < MaxGroups; g++ {
		if g%MaxPerUser == 0 {
			e.user(t, fmt.Sprintf("u%d@example.com", g/MaxPerUser), true)
		}
	}
	for g := 0; g < MaxGroups; g++ {
		email := fmt.Sprintf("u%d@example.com", g/MaxPerUser)
		raw, _, err := e.svc.SignIn(context.Background(), email, "correct horse", "192.0.2.1")
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("g%d", g)
		e.session(t, id, fmt.Sprintf("Cat%d", g), future, "Full", ip(0))
		if w := e.do("POST", "/api/watches", map[string]string{"session_id": id}, &http.Cookie{Name: "mds_session", Value: raw}, ""); w.Code != 201 {
			t.Fatalf("group %d: %d %s", g, w.Code, w.Body)
		}
	}
	c := e.user(t, "late@example.com", true)
	e.session(t, "new", "CatNew", future, "Full", ip(0))
	e.session(t, "same", "Cat0", future, "Full", ip(0))
	if w := e.do("POST", "/api/watches", map[string]string{"session_id": "new"}, c, ""); w.Code != 503 ||
		errOf(w) != "Spot watching is at capacity right now. Try again later." {
		t.Fatalf("21st group: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/watches", map[string]string{"session_id": "same"}, c, ""); w.Code != 201 {
		t.Fatalf("existing group: %d %s", w.Code, w.Body)
	}
}

func TestListAndDelete(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	e.session(t, "s1", "Swim", t0.Add(24*time.Hour), "Full", ip(0))
	if w := e.do("POST", "/api/watches", map[string]string{"session_id": "s1"}, c, ""); w.Code != 201 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	w := e.do("GET", "/api/watches", nil, c, "")
	var got struct {
		Watches []struct {
			SessionID  string `json:"session_id"`
			Name       string `json:"name"`
			Centre     string `json:"centre"`
			BookingURL string `json:"booking_url"`
			EmailsSent int    `json:"emails_sent"`
		}
		Summary map[string]any
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if len(got.Watches) != 1 || got.Watches[0].SessionID != "s1" || got.Watches[0].Name != "Swim s1" ||
		got.Watches[0].Centre != "Centennial" || got.Watches[0].BookingURL != "https://x/s1" {
		t.Fatalf("watches = %+v", got.Watches)
	}
	for _, k := range []string{"last_cycle", "sessions", "people"} {
		if _, ok := got.Summary[k]; !ok {
			t.Fatalf("summary missing %s: %s", k, w.Body)
		}
	}
	for i := 0; i < 2; i++ { // delete is idempotent
		if w := e.do("POST", "/api/watches/delete", map[string]string{"session_id": "s1"}, c, ""); w.Code != 204 {
			t.Fatalf("delete: %d", w.Code)
		}
	}
	w = e.do("GET", "/api/watches", nil, c, "")
	if !strings.Contains(w.Body.String(), `"watches":[]`) {
		t.Fatalf("not empty: %s", w.Body)
	}
}

func TestAllWatchesAdminOnly(t *testing.T) {
	e := newHTTPEnv(t)
	user := e.user(t, "a@example.com", true)
	admin := e.user(t, "admin@example.com", true)
	e.session(t, "s1", "Swim", t0.Add(24*time.Hour), "Full", ip(0))
	e.do("POST", "/api/watches", map[string]string{"session_id": "s1"}, user, "")
	if w := e.do("GET", "/api/watches/all", nil, user, ""); w.Code != 403 || errOf(w) != "Admins only." {
		t.Fatalf("non-admin: %d %s", w.Code, w.Body)
	}
	w := e.do("GET", "/api/watches/all", nil, admin, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"email":"a@example.com"`) ||
		!strings.Contains(w.Body.String(), `"session_name":"Swim s1"`) {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
}

func TestStopLink(t *testing.T) {
	e := newHTTPEnv(t)
	ctx := context.Background()
	e.session(t, "s1", "Swim", t0.Add(24*time.Hour), "Full", ip(0))
	u, _ := e.st.CreateUser(ctx, "a@example.com", "x", t0)
	wt, err := e.st.AddWatch(ctx, u.ID, "s1", t0.Add(24*time.Hour), t0)
	if err != nil {
		t.Fatal(err)
	}
	var toks []string
	for i := 0; i < 2; i++ {
		raw, hash, _ := auth.NewToken()
		toks = append(toks, raw)
		if err := e.st.AddStopToken(ctx, wt.ID, hash); err != nil {
			t.Fatal(err)
		}
	}
	// GET only shows the confirmation and never deletes.
	w := e.do("GET", "/watch/stop?t="+toks[0]+"&token=abc", nil, nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Stop emails for this session?") ||
		!strings.Contains(w.Body.String(), `action="/watch/stop?token=abc"`) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if ws, _ := e.st.AllWatches(ctx); len(ws) != 1 {
		t.Fatalf("GET deleted: %+v", ws)
	}
	post := func(tok, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost/watch/stop", strings.NewReader(url.Values{"t": {tok}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin == "" {
			origin = "http://localhost"
		}
		r.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, r)
		return rec
	}
	if w := post(toks[0], "http://evil.example"); w.Code != 403 {
		t.Fatalf("cross-site: %d", w.Code)
	}
	if ws, _ := e.st.AllWatches(ctx); len(ws) != 1 {
		t.Fatal("cross-site POST deleted")
	}
	// The earlier email's token still works.
	if w := post(toks[0], ""); w.Code != 200 || !strings.Contains(w.Body.String(), "You&#39;ve stopped watching this session.") {
		t.Fatalf("post: %d %s", w.Code, w.Body)
	}
	if ws, _ := e.st.AllWatches(ctx); len(ws) != 0 {
		t.Fatalf("watch not deleted: %+v", ws)
	}
	for _, tok := range []string{toks[0], toks[1], "bogus", ""} {
		if w := post(tok, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "This watch was already stopped.") {
			t.Fatalf("again %q: %d %s", tok, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "/watch/stop", nil, nil, ""); !strings.Contains(w.Body.String(), "This watch was already stopped.") {
		t.Fatalf("empty t: %s", w.Body)
	}
}

func TestConcurrentAddsRespectLimit(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	future := t0.Add(24 * time.Hour)
	const n = 15
	for i := 0; i < n; i++ {
		e.session(t, fmt.Sprintf("c%d", i), "Swim", future, "Full", ip(0))
	}
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			codes <- e.do("POST", "/api/watches", map[string]string{"session_id": id}, c, "").Code
		}(fmt.Sprintf("c%d", i))
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[201] != MaxPerUser || got[429] != n-MaxPerUser {
		t.Fatalf("codes = %v", got)
	}
}
