package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

const testHost = "vnas:8095"

type httpHarness struct {
	*harness
	h       *HTTP
	handler http.Handler
}

func newHTTPHarness(t *testing.T) *httpHarness {
	t.Helper()
	base := newHarness(t, "")
	h := base.svc.Handler(HTTPOptions{
		BaseURL:          "https://markham.example",
		Trusted:          []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")},
		TurnstileSiteKey: "site-key",
	})
	mux := http.NewServeMux()
	h.Register(mux, func(f http.HandlerFunc) http.HandlerFunc { return f })
	return &httpHarness{harness: base, h: h, handler: h.Middleware(mux)}
}

type reqOpt func(*http.Request)

func withCookie(raw string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "mds_session", Value: raw}) }
}

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func fromAddr(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }

// do sends a request to the harness. POSTs carry a same-origin Origin header
// unless an option overrides it.
func (hh *httpHarness) do(t *testing.T, method, path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(j)
	}
	r := httptest.NewRequest(method, "http://"+testHost+path, rd)
	if method != http.MethodGet {
		r.Header.Set("Origin", "http://"+testHost)
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	hh.handler.ServeHTTP(w, r)
	return w
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) (*http.Cookie, string) {
	t.Helper()
	for _, line := range w.Result().Header.Values("Set-Cookie") {
		if strings.HasPrefix(line, "mds_session=") {
			for _, c := range w.Result().Cookies() {
				if c.Name == "mds_session" {
					return c, line
				}
			}
		}
	}
	t.Fatalf("no mds_session cookie; status %d body %s", w.Code, w.Body)
	return nil, ""
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	return v.Error
}

type meBody struct {
	User *struct {
		Email     string `json:"email"`
		Name      string `json:"name"`
		Confirmed bool   `json:"confirmed"`
	} `json:"user"`
	TurnstileSiteKey string `json:"turnstile_site_key"`
}

func (hh *httpHarness) me(t *testing.T, opts ...reqOpt) meBody {
	t.Helper()
	w := hh.do(t, "GET", "/api/auth/me", nil, opts...)
	if w.Code != http.StatusOK {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}
	var m meBody
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// signIn signs in through HTTP and returns the raw session cookie value.
func (hh *httpHarness) signInHTTP(t *testing.T, email, password string) string {
	t.Helper()
	w := hh.do(t, "POST", "/api/auth/signin", map[string]string{"email": email, "password": password})
	if w.Code != http.StatusOK {
		t.Fatalf("signin: %d %s", w.Code, w.Body)
	}
	c, _ := sessionCookie(t, w)
	return c.Value
}

func TestSignInSetsCookieAndMe(t *testing.T) {
	hh := newHTTPHarness(t)
	if m := hh.me(t); m.User != nil || m.TurnstileSiteKey != "site-key" {
		t.Fatalf("signed-out me = %+v", m)
	}
	w := hh.do(t, "POST", "/api/auth/signup", map[string]string{"email": "a@example.com", "password": pw, "turnstile": "x"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("signup: %d %s", w.Code, w.Body)
	}
	w = hh.do(t, "POST", "/api/auth/confirm", map[string]string{"t": hh.lastToken(t, "/verify")})
	if w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}

	w = hh.do(t, "POST", "/api/auth/signin", map[string]string{"email": "a@example.com", "password": pw})
	if w.Code != http.StatusOK {
		t.Fatalf("signin: %d %s", w.Code, w.Body)
	}
	c, line := sessionCookie(t, w)
	if !strings.Contains(line, "HttpOnly") || !strings.Contains(line, "SameSite=Lax") || !strings.Contains(line, "Path=/") {
		t.Errorf("cookie = %q", line)
	}
	if strings.Contains(line, "Secure") {
		t.Errorf("Secure on plain HTTP: %q", line)
	}
	if c.MaxAge != 30*24*60*60 {
		t.Errorf("MaxAge = %d", c.MaxAge)
	}

	m := hh.me(t, withCookie(c.Value))
	if m.User == nil || m.User.Email != "a@example.com" || !m.User.Confirmed {
		t.Fatalf("me = %+v", m)
	}

	w = hh.do(t, "POST", "/api/auth/signout", nil, withCookie(c.Value))
	if w.Code != http.StatusNoContent {
		t.Fatalf("signout: %d %s", w.Code, w.Body)
	}
	if out, _ := sessionCookie(t, w); out.Value != "" || out.MaxAge >= 0 {
		t.Errorf("signout cookie = %+v", out)
	}
	if m := hh.me(t, withCookie(c.Value)); m.User != nil {
		t.Fatalf("me after signout = %+v", m)
	}
}

func TestSecureCookieOnlyFromTrustedProxy(t *testing.T) {
	hh := newHTTPHarness(t)
	hh.confirmedUser(t, "a@example.com", pw)
	signin := func(addr string) string {
		w := hh.do(t, "POST", "/api/auth/signin", map[string]string{"email": "a@example.com", "password": pw},
			fromAddr(addr), withHeader("X-Forwarded-Proto", "https"))
		if w.Code != http.StatusOK {
			t.Fatalf("signin from %s: %d %s", addr, w.Code, w.Body)
		}
		_, line := sessionCookie(t, w)
		return line
	}
	if line := signin("172.17.0.2:5000"); !strings.Contains(line, "Secure") {
		t.Errorf("trusted proxy: no Secure: %q", line)
	}
	if line := signin("[::ffff:172.17.0.2]:5000"); !strings.Contains(line, "Secure") {
		t.Errorf("trusted proxy (IPv4-mapped): no Secure: %q", line)
	}
	if line := signin("192.168.1.5:5000"); strings.Contains(line, "Secure") {
		t.Errorf("untrusted: Secure set: %q", line)
	}
}

func TestClientIPIgnoresUntrustedHeader(t *testing.T) {
	hh := newHTTPHarness(t)
	hh.confirmedUser(t, "a@example.com", pw)
	for i := 1; i <= 6; i++ {
		w := hh.do(t, "POST", "/api/auth/signin", map[string]string{"email": "a@example.com", "password": "wrong password!"},
			fromAddr("192.168.1.5:5000"), withHeader("CF-Connecting-IP", "203.0.113."+string(rune('0'+i))))
		want := http.StatusUnauthorized
		if i == 6 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d %s", i, w.Code, w.Body)
		}
		if i < 6 && errorOf(t, w) != "Email or password is incorrect." {
			t.Errorf("attempt %d error = %q", i, errorOf(t, w))
		}
	}
}

func TestClientIPFromTrustedProxy(t *testing.T) {
	hh := newHTTPHarness(t)
	hh.confirmedUser(t, "a@example.com", pw)
	// Through the trusted proxy each CF-Connecting-IP is its own client, so
	// none of them reaches the limit.
	for i := 1; i <= 6; i++ {
		w := hh.do(t, "POST", "/api/auth/signin", map[string]string{"email": "a@example.com", "password": "wrong password!"},
			fromAddr("172.17.0.2:5000"), withHeader("CF-Connecting-IP", "203.0.113."+string(rune('0'+i))))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d %s", i, w.Code, w.Body)
		}
	}
}

func TestCSRF(t *testing.T) {
	hh := newHTTPHarness(t)
	hh.confirmedUser(t, "a@example.com", pw)
	raw := hh.signInHTTP(t, "a@example.com", pw)
	body := map[string]string{"current": "wrong password!", "new": pw2}
	cases := []struct {
		name string
		opts []reqOpt
		want int
	}{
		{"evil origin", []reqOpt{withHeader("Origin", "https://evil.example")}, http.StatusForbidden},
		{"no origin or referer", []reqOpt{func(r *http.Request) { r.Header.Del("Origin") }}, http.StatusForbidden},
		{"null origin", []reqOpt{withHeader("Origin", "null")}, http.StatusForbidden},
		{"evil referer", []reqOpt{func(r *http.Request) {
			r.Header.Del("Origin")
			r.Header.Set("Referer", "https://evil.example/account")
		}}, http.StatusForbidden},
		{"same host", []reqOpt{withHeader("Origin", "http://VNAS:8095")}, http.StatusUnauthorized},
		{"same host referer", []reqOpt{func(r *http.Request) {
			r.Header.Del("Origin")
			r.Header.Set("Referer", "http://vnas:8095/account")
		}}, http.StatusUnauthorized},
		{"base url host", []reqOpt{withHeader("Origin", "https://markham.example"), func(r *http.Request) { r.Host = "172.17.0.5:8095" }}, http.StatusUnauthorized},
	}
	for _, c := range cases {
		opts := append([]reqOpt{withCookie(raw)}, c.opts...)
		w := hh.do(t, "POST", "/api/auth/password", body, opts...)
		if w.Code != c.want {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
		// Past CSRF the wrong current password is rejected, which shows the
		// request reached the handler.
	}
	// Sign-in is CSRF-checked too.
	w := hh.do(t, "POST", "/api/auth/signin", map[string]string{"email": "a@example.com", "password": pw},
		withHeader("Origin", "https://evil.example"))
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site signin: %d", w.Code)
	}
}

func TestRequireConfirmed(t *testing.T) {
	hh := newHTTPHarness(t)
	var got string
	gated := hh.h.Middleware(hh.h.RequireConfirmed(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFrom(r.Context())
		if !ok {
			t.Error("UserFrom: no user")
		}
		got = u.Email
		w.WriteHeader(http.StatusTeapot)
	}))
	call := func(method string, opts ...reqOpt) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+testHost+"/api/refresh", nil)
		if method != http.MethodGet {
			r.Header.Set("Origin", "http://"+testHost)
		}
		for _, o := range opts {
			o(r)
		}
		w := httptest.NewRecorder()
		gated.ServeHTTP(w, r)
		return w
	}

	if w := call("POST"); w.Code != http.StatusUnauthorized || errorOf(t, w) != "sign in required" {
		t.Errorf("no cookie: %d %s", w.Code, w.Body)
	}

	if err := hh.svc.SignUp(ctx, "u@example.com", pw, "x", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	unconfirmed := hh.signInHTTP(t, "u@example.com", pw)
	if w := call("POST", withCookie(unconfirmed)); w.Code != http.StatusForbidden || errorOf(t, w) != "confirm your email first" {
		t.Errorf("unconfirmed: %d %s", w.Code, w.Body)
	}

	hh.confirmedUser(t, "c@example.com", pw)
	confirmed := hh.signInHTTP(t, "c@example.com", pw)
	if w := call("POST", withCookie(confirmed)); w.Code != http.StatusTeapot || got != "c@example.com" {
		t.Errorf("confirmed: %d %s user %q", w.Code, w.Body, got)
	}
	if w := call("GET", withCookie(confirmed)); w.Code != http.StatusTeapot {
		t.Errorf("confirmed GET: %d %s", w.Code, w.Body)
	}
	if w := call("POST", withCookie(confirmed), withHeader("Origin", "https://evil.example")); w.Code != http.StatusForbidden {
		t.Errorf("confirmed cross-site POST: %d %s", w.Code, w.Body)
	}
}

func TestSignUpResponsesIdentical(t *testing.T) {
	hh := newHTTPHarness(t)
	hh.confirmedUser(t, "old@example.com", pw)
	signup := func(email string) *httptest.ResponseRecorder {
		return hh.do(t, "POST", "/api/auth/signup", map[string]string{"email": email, "password": pw2, "turnstile": "x"})
	}
	a, b := signup("new@example.com"), signup("old@example.com")
	if a.Code != b.Code || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatalf("new: %d %q; existing: %d %q", a.Code, a.Body, b.Code, b.Body)
	}
	if a.Code != http.StatusAccepted {
		t.Errorf("status = %d", a.Code)
	}
	fa, fb := hh.do(t, "POST", "/api/auth/forgot", map[string]string{"email": "old@example.com", "turnstile": "x"}),
		hh.do(t, "POST", "/api/auth/forgot", map[string]string{"email": "nobody@example.com", "turnstile": "x"})
	if fa.Code != http.StatusAccepted || fa.Code != fb.Code || !bytes.Equal(fa.Body.Bytes(), fb.Body.Bytes()) {
		t.Fatalf("forgot existing: %d %q; missing: %d %q", fa.Code, fa.Body, fb.Code, fb.Body)
	}
}

func TestErrorMapping(t *testing.T) {
	hh := newHTTPHarness(t)
	w := hh.do(t, "POST", "/api/auth/signup", map[string]string{"email": "a@example.com", "password": "short", "turnstile": "x"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("weak password: %d %s", w.Code, w.Body)
	}
	w = hh.do(t, "POST", "/api/auth/signup", `{"email":`)
	if w.Code != http.StatusBadRequest || errorOf(t, w) != "invalid request" {
		t.Errorf("malformed: %d %s", w.Code, w.Body)
	}
	w = hh.do(t, "POST", "/api/auth/confirm", map[string]string{"t": "nope"})
	if w.Code != http.StatusBadRequest || errorOf(t, w) != "This link has expired or was already used." {
		t.Errorf("bad token: %d %s", w.Code, w.Body)
	}
	big := `{"email":"a@example.com","password":"` + strings.Repeat("x", 17<<10) + `"}`
	w = hh.do(t, "POST", "/api/auth/signin", big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: %d %s", w.Code, w.Body)
	}
	hh.bot.err = ErrBotUnavailable
	w = hh.do(t, "POST", "/api/auth/signup", map[string]string{"email": "a@example.com", "password": pw, "turnstile": "x"})
	if w.Code != http.StatusServiceUnavailable || errorOf(t, w) != "Couldn't check you're not a robot. Try again." {
		t.Errorf("bot unavailable: %d %s", w.Code, w.Body)
	}
	hh.bot.err = ErrBot
	w = hh.do(t, "POST", "/api/auth/forgot", map[string]string{"email": "a@example.com", "turnstile": "x"})
	if w.Code != http.StatusBadRequest || errorOf(t, w) != "Couldn't verify you're not a robot." {
		t.Errorf("bot: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/api/auth/resend", "/api/auth/password", "/api/auth/delete"} {
		w = hh.do(t, "POST", p, map[string]string{})
		if w.Code != http.StatusUnauthorized || errorOf(t, w) != "sign in required" {
			t.Errorf("%s signed out: %d %s", p, w.Code, w.Body)
		}
	}
}

func TestResetPasswordAndDelete(t *testing.T) {
	hh := newHTTPHarness(t)
	hh.confirmedUser(t, "a@example.com", pw)

	w := hh.do(t, "POST", "/api/auth/forgot", map[string]string{"email": "a@example.com", "turnstile": "x"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("forgot: %d %s", w.Code, w.Body)
	}
	w = hh.do(t, "POST", "/api/auth/reset", map[string]string{"t": hh.lastToken(t, "/reset"), "password": pw2})
	if w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	c, _ := sessionCookie(t, w)
	if m := hh.me(t, withCookie(c.Value)); m.User == nil || m.User.Email != "a@example.com" {
		t.Fatalf("me after reset = %+v", m)
	}

	other := hh.signInHTTP(t, "a@example.com", pw2)
	w = hh.do(t, "POST", "/api/auth/password", map[string]string{"current": pw2, "new": pw}, withCookie(c.Value))
	if w.Code != http.StatusNoContent {
		t.Fatalf("password: %d %s", w.Code, w.Body)
	}
	if m := hh.me(t, withCookie(c.Value)); m.User == nil {
		t.Error("password change signed out the current session")
	}
	if m := hh.me(t, withCookie(other)); m.User != nil {
		t.Error("password change kept another session")
	}
	w = hh.do(t, "POST", "/api/auth/password", map[string]string{"current": "wrong password!", "new": pw2}, withCookie(c.Value))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong current: %d %s", w.Code, w.Body)
	}

	w = hh.do(t, "POST", "/api/auth/delete", map[string]string{"password": pw}, withCookie(c.Value))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if out, _ := sessionCookie(t, w); out.Value != "" || out.MaxAge >= 0 {
		t.Errorf("delete cookie = %+v", out)
	}
	if m := hh.me(t, withCookie(c.Value)); m.User != nil {
		t.Error("session survives account deletion")
	}
}

func TestResend(t *testing.T) {
	hh := newHTTPHarness(t)
	if err := hh.svc.SignUp(ctx, "u@example.com", pw, "x", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	raw := hh.signInHTTP(t, "u@example.com", pw)
	n := len(hh.mail.sent)
	w := hh.do(t, "POST", "/api/auth/resend", nil, withCookie(raw))
	if w.Code != http.StatusAccepted || len(hh.mail.sent) != n+1 {
		t.Fatalf("resend: %d %s, mails %d -> %d", w.Code, w.Body, n, len(hh.mail.sent))
	}
}

func TestSetName(t *testing.T) {
	hh := newHTTPHarness(t)
	if w := hh.do(t, "POST", "/api/auth/name", map[string]string{"name": "V"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d %s", w.Code, w.Body)
	}
	hh.confirmedUser(t, "a@example.com", pw)
	raw := hh.signInHTTP(t, "a@example.com", pw)

	w := hh.do(t, "POST", "/api/auth/name", map[string]string{"name": "  Vincent Liu  "}, withCookie(raw))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"Vincent Liu"`) {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	if m := hh.me(t, withCookie(raw)); m.User == nil || m.User.Name != "Vincent Liu" {
		t.Fatalf("me = %+v", m)
	}

	for _, bad := range []string{strings.Repeat("é", MaxNameLen+1), "a\nb", "a\x00b"} {
		w = hh.do(t, "POST", "/api/auth/name", map[string]string{"name": bad}, withCookie(raw))
		if w.Code != http.StatusBadRequest || errorOf(t, w) != "Name must be at most 60 characters." {
			t.Errorf("name %q: %d %s", bad, w.Code, w.Body)
		}
	}
	if w = hh.do(t, "POST", "/api/auth/name", map[string]string{"name": strings.Repeat("é", MaxNameLen)}, withCookie(raw)); w.Code != http.StatusOK {
		t.Errorf("60 chars: %d %s", w.Code, w.Body)
	}
	if w = hh.do(t, "POST", "/api/auth/name", map[string]string{"name": " "}, withCookie(raw)); w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	if m := hh.me(t, withCookie(raw)); m.User.Name != "" {
		t.Fatalf("cleared name = %q", m.User.Name)
	}
	if w = hh.do(t, "POST", "/api/auth/name", map[string]string{"name": "X"}, withCookie(raw), withHeader("Origin", "https://evil.example")); w.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", w.Code)
	}
}

func TestMeShowsIsAdmin(t *testing.T) {
	hh := newHTTPHarness(t)
	hh.svc.o.AdminEmails = []string{"boss@example.com"}
	hh.confirmedUser(t, "boss@example.com", pw)
	hh.confirmedUser(t, "other@example.com", pw)
	for email, want := range map[string]bool{"boss@example.com": true, "other@example.com": false} {
		w := hh.do(t, "GET", "/api/auth/me", nil, withCookie(hh.signIn(t, email, pw)))
		var m struct {
			User struct {
				IsAdmin bool `json:"is_admin"`
			} `json:"user"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m.User.IsAdmin != want {
			t.Fatalf("%s: is_admin = %v (err %v), want %v; body %s", email, m.User.IsAdmin, err, want, w.Body)
		}
	}
}
