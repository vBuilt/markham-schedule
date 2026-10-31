package remind

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/auth"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
)

var ht0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

type httpEnv struct {
	st      *store.Store
	svc     *auth.Service
	handler http.Handler
	sess    []schedule.Session
}

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
	mux := http.NewServeMux()
	NewHTTP(st, ah, func() time.Time { return ht0 }).Register(mux, func(f http.HandlerFunc) http.HandlerFunc { return f })
	return &httpEnv{st: st, svc: svc, handler: ah.Middleware(mux)}
}

func (e *httpEnv) user(t *testing.T, email string, confirmed bool) *http.Cookie {
	t.Helper()
	hash, _ := auth.HashPassword("correct horse")
	u, err := e.st.CreateUser(context.Background(), email, hash, ht0)
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

// session stores a session whose Residents registration opens at res
// (nil: no registration info).
func (e *httpEnv) session(t *testing.T, id string, res *time.Time) {
	t.Helper()
	start := ht0.Add(48 * time.Hour)
	s := schedule.Session{ID: id, Name: "Swim " + id, Categories: []string{"Swim"}, Centre: "Centennial",
		Start: start, End: start.Add(time.Hour), BookingURL: "https://x/" + id}
	if res != nil {
		s.Registration = &schedule.Registration{Residents: res}
	}
	e.sess = append(e.sess, s)
	if err := e.st.ReplaceCategory(context.Background(), "Swim", ht0.Add(-24*time.Hour), e.sess); err != nil {
		t.Fatal(err)
	}
}

func sameTime(v any, want time.Time) bool {
	s, _ := v.(string)
	got, err := time.Parse(time.RFC3339, s)
	return err == nil && got.Equal(want)
}

func tp(d time.Duration) *time.Time { x := ht0.Add(d); return &x }

func (e *httpEnv) do(method, path string, body any, c *http.Cookie, origin string) *httptest.ResponseRecorder {
	j, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(j))
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

func (e *httpEnv) add(c *http.Cookie, id string) *httptest.ResponseRecorder {
	return e.do("POST", "/api/reminders", map[string]string{"session_id": id}, c, "")
}

func TestAddReminder201AndList(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	e.session(t, "s1", tp(time.Hour))
	w := e.add(c, "s1")
	if w.Code != 201 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	var j map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &j)
	if j["name"] != "Swim s1" || j["centre"] != "Centennial" || j["booking_url"] != "https://x/s1" ||
		!sameTime(j["residents_at"], ht0.Add(time.Hour)) || !sameTime(j["remind_at"], ht0.Add(time.Hour-Lead)) || j["sent_at"] != nil {
		t.Fatalf("json: %s", w.Body)
	}
	l := e.do("GET", "/api/reminders", nil, c, "")
	if l.Code != 200 || !strings.Contains(l.Body.String(), `"reminders":[{"session_id":"s1"`) {
		t.Fatalf("list: %d %s", l.Code, l.Body)
	}
	if d := e.do("POST", "/api/reminders/delete", map[string]string{"session_id": "s1"}, c, ""); d.Code != 204 {
		t.Fatalf("delete: %d", d.Code)
	}
	if d := e.do("POST", "/api/reminders/delete", map[string]string{"session_id": "s1"}, c, ""); d.Code != 204 {
		t.Fatalf("delete again: %d", d.Code)
	}
	if rs, _ := e.st.AllReminders(context.Background()); len(rs) != 0 {
		t.Fatalf("left: %+v", rs)
	}
}

func TestAddReminderAgain200(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	e.session(t, "s1", tp(time.Hour))
	e.add(c, "s1")
	if w := e.add(c, "s1"); w.Code != 200 {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
	if rs, _ := e.st.AllReminders(context.Background()); len(rs) != 1 {
		t.Fatalf("dupes: %+v", rs)
	}
}

func TestAddReminderNoResidents409(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	e.session(t, "s1", nil)
	if w := e.add(c, "s1"); w.Code != 409 || errOf(w) != "Online registration time isn't known for this session." {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestAddReminderTooSoon409(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	e.session(t, "soon", tp(2*time.Minute+15*time.Second))
	e.session(t, "ok", tp(2*time.Minute+16*time.Second))
	if w := e.add(c, "soon"); w.Code != 409 || errOf(w) != "Registration opens too soon for a reminder." {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.add(c, "ok"); w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestAddReminderOpened409(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	e.session(t, "s1", tp(-time.Minute))
	if w := e.add(c, "s1"); w.Code != 409 || errOf(w) != "Registration has already opened." {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestAddReminderLimit429(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	for i := 0; i <= MaxPerUser; i++ {
		e.session(t, fmt.Sprint("s", i), tp(time.Hour))
	}
	for i := 0; i < MaxPerUser; i++ {
		if w := e.add(c, fmt.Sprint("s", i)); w.Code != 201 {
			t.Fatalf("add %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := e.add(c, "s10"); w.Code != 429 || errOf(w) != "You can have at most 10 reminders at a time." {
		t.Fatalf("11th: %d %s", w.Code, w.Body)
	}
	// A sent reminder no longer counts against the limit.
	rs, _ := e.st.UserReminders(context.Background(), rs0(t, e).ID)
	if _, err := e.st.MarkReminderSent(context.Background(), rs[0].ID, ht0); err != nil {
		t.Fatal(err)
	}
	if w := e.add(c, "s10"); w.Code != 201 {
		t.Fatalf("after sent: %d %s", w.Code, w.Body)
	}
}

func rs0(t *testing.T, e *httpEnv) store.User {
	t.Helper()
	u, err := e.st.UserByEmail(context.Background(), "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAddReminderUnknown404(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	if w := e.add(c, "nope"); w.Code != 404 || errOf(w) != "Session not found." {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/reminders", map[string]string{}, c, ""); w.Code != 400 || errOf(w) != "invalid request" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestRemindersAllAdminOnly(t *testing.T) {
	e := newHTTPEnv(t)
	user := e.user(t, "a@example.com", true)
	admin := e.user(t, "admin@example.com", true)
	e.session(t, "s1", tp(time.Hour))
	e.add(user, "s1")
	if w := e.do("GET", "/api/reminders/all", nil, user, ""); w.Code != 403 || errOf(w) != "Admins only." {
		t.Fatalf("non-admin: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/reminders/all", nil, admin, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"email":"a@example.com"`) {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
}

func TestRemindersCSRF(t *testing.T) {
	e := newHTTPEnv(t)
	c := e.user(t, "a@example.com", true)
	e.session(t, "s1", tp(time.Hour))
	for _, p := range []string{"/api/reminders", "/api/reminders/delete"} {
		if w := e.do("POST", p, map[string]string{"session_id": "s1"}, c, "http://evil.example"); w.Code != 403 {
			t.Errorf("cross-site %s = %d", p, w.Code)
		}
	}
	if rs, _ := e.st.AllReminders(context.Background()); len(rs) != 0 {
		t.Fatalf("created: %+v", rs)
	}
}

func TestRemindersRequireConfirmed(t *testing.T) {
	e := newHTTPEnv(t)
	e.session(t, "s1", tp(time.Hour))
	unconfirmed := e.user(t, "b@example.com", false)
	for _, rq := range [][2]string{{"GET", "/api/reminders"}, {"POST", "/api/reminders"}, {"POST", "/api/reminders/delete"}, {"GET", "/api/reminders/all"}} {
		if w := e.do(rq[0], rq[1], map[string]string{"session_id": "s1"}, nil, ""); w.Code != 401 {
			t.Errorf("%v signed out = %d", rq, w.Code)
		}
		if w := e.do(rq[0], rq[1], map[string]string{"session_id": "s1"}, unconfirmed, ""); w.Code != 403 {
			t.Errorf("%v unconfirmed = %d", rq, w.Code)
		}
	}
}

// cancelSetup creates a reminder with one cancel token.
func cancelSetup(t *testing.T) (*httpEnv, string) {
	t.Helper()
	e := newHTTPEnv(t)
	ctx := context.Background()
	e.session(t, "s1", tp(time.Hour))
	u, _ := e.st.CreateUser(ctx, "a@example.com", "x", ht0)
	rem, err := e.st.AddReminder(ctx, u.ID, "s1", *tp(time.Hour), ht0)
	if err != nil {
		t.Fatal(err)
	}
	raw, hash, _ := auth.NewToken()
	if err := e.st.AddReminderCancelToken(ctx, rem.ID, hash); err != nil {
		t.Fatal(err)
	}
	return e, raw
}

func (e *httpEnv) postForm(tok, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://localhost/reminder/cancel", strings.NewReader(url.Values{"t": {tok}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin == "" {
		origin = "http://localhost"
	}
	r.Header.Set("Origin", origin)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	return rec
}

func TestCancelGetDoesNotDelete(t *testing.T) {
	e, tok := cancelSetup(t)
	w := e.do("GET", "/reminder/cancel?t="+tok+"&token=abc", nil, nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Cancel this registration reminder?") ||
		!strings.Contains(w.Body.String(), `action="/reminder/cancel?token=abc"`) ||
		!strings.Contains(w.Body.String(), "Cancel reminder</button>") {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if rs, _ := e.st.AllReminders(context.Background()); len(rs) != 1 {
		t.Fatalf("GET deleted: %+v", rs)
	}
	if w := e.do("GET", "/reminder/cancel", nil, nil, ""); !strings.Contains(w.Body.String(), "This reminder was already cancelled.") {
		t.Fatalf("empty t: %s", w.Body)
	}
}

func TestCancelPostSingleUse(t *testing.T) {
	e, tok := cancelSetup(t)
	if w := e.postForm(tok, "http://evil.example"); w.Code != 403 {
		t.Fatalf("cross-site: %d", w.Code)
	}
	if rs, _ := e.st.AllReminders(context.Background()); len(rs) != 1 {
		t.Fatal("cross-site POST deleted")
	}
	if w := e.postForm(tok, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Your reminder is cancelled.") {
		t.Fatalf("post: %d %s", w.Code, w.Body)
	}
	for _, x := range []string{tok, "bogus", ""} {
		if w := e.postForm(x, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "This reminder was already cancelled.") {
			t.Fatalf("again %q: %d %s", x, w.Code, w.Body)
		}
	}
}

func TestCancelPostAlreadySentKeepsRecord(t *testing.T) {
	e, tok := cancelSetup(t)
	ctx := context.Background()
	rs, _ := e.st.AllReminders(ctx)
	if ok, err := e.st.MarkReminderSent(ctx, rs[0].ID, ht0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	w := e.postForm(tok, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "This reminder was already sent.") {
		t.Fatalf("post: %d %s", w.Code, w.Body)
	}
	if rs, _ := e.st.AllReminders(ctx); len(rs) != 1 || rs[0].SentAt == nil {
		t.Fatalf("sent record removed: %+v", rs)
	}
}
