package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/vBuilt/markham-schedule/internal/store"
)

const (
	cookieName   = "mds_session"
	maxBodyBytes = 16 << 10
)

// HTTPOptions configures the account endpoints.
type HTTPOptions struct {
	// BaseURL's host is accepted as a request origin alongside the Host header.
	BaseURL string
	// Trusted lists proxy addresses whose CF-Connecting-IP and
	// X-Forwarded-Proto headers are believed.
	Trusted []netip.Prefix
	// TurnstileSiteKey is handed to pages by GET /api/auth/me.
	TurnstileSiteKey string
}

// HTTP serves the account JSON endpoints and the session middleware.
type HTTP struct {
	s        *Service
	o        HTTPOptions
	baseHost string
}

func (s *Service) Handler(o HTTPOptions) *HTTP {
	h := &HTTP{s: s, o: o}
	if u, err := url.Parse(o.BaseURL); err == nil {
		h.baseHost = u.Host
	}
	return h
}

type userKey struct{}

// UserFrom returns the signed-in user Middleware found for the request.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey{}).(store.User)
	return u, ok
}

// Middleware looks up the session cookie and puts its user in the request
// context. It never rejects a request.
func (h *HTTP) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := sessionToken(r)
		if raw != "" {
			u, err := h.s.SessionUser(r.Context(), raw)
			switch {
			case err == nil:
				r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
			case !errors.Is(err, store.ErrNotFound):
				h.s.o.Log.Error("session lookup failed", "path", r.URL.Path, "err", err)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RequireConfirmed lets only confirmed signed-in users through, and checks
// the origin of requests that change state.
func (h *HTTP) RequireConfirmed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		u, ok := UserFrom(r.Context())
		if !ok {
			writeErr(w, http.StatusUnauthorized, "sign in required")
			return
		}
		if !u.Confirmed {
			writeErr(w, http.StatusForbidden, "confirm your email first")
			return
		}
		next(w, r)
	}
}

// Register adds the /api/auth/ routes to mux; wrap is applied to each.
func (h *HTTP) Register(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/auth/me", wrap(h.me))
	post := func(path string, f http.HandlerFunc) {
		mux.HandleFunc("POST "+path, wrap(h.CSRF(f)))
	}
	post("/api/auth/signup", h.signUp)
	post("/api/auth/confirm", h.confirm)
	post("/api/auth/resend", h.signedIn(h.resend))
	post("/api/auth/signin", h.signIn)
	post("/api/auth/signout", h.signOut)
	post("/api/auth/forgot", h.forgot)
	post("/api/auth/reset", h.reset)
	post("/api/auth/password", h.signedIn(h.password))
	post("/api/auth/name", h.signedIn(h.setName))
	post("/api/auth/delete", h.signedIn(h.deleteAccount))
}

type userJSON struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	Confirmed bool   `json:"confirmed"`
	IsAdmin   bool   `json:"is_admin"`
}

// IsAdmin reports whether u is an admin; it delegates to the Service.
func (h *HTTP) IsAdmin(u store.User) bool { return h.s.IsAdmin(u) }

func (h *HTTP) userJSON(u store.User) userJSON {
	return userJSON{Email: u.Email, Name: u.Name, Confirmed: u.Confirmed, IsAdmin: h.s.IsAdmin(u)}
}

func (h *HTTP) me(w http.ResponseWriter, r *http.Request) {
	var uj *userJSON
	if u, ok := UserFrom(r.Context()); ok {
		v := h.userJSON(u)
		uj = &v
		// Pages ask for me on load; re-issuing the cookie here keeps its
		// lifetime in step with the server's sliding session expiry.
		h.setSession(w, r, sessionToken(r))
	}
	writeJSON(w, http.StatusOK, struct {
		User             *userJSON `json:"user"`
		TurnstileSiteKey string    `json:"turnstile_site_key"`
	}{uj, h.o.TurnstileSiteKey})
}

var checkEmail = map[string]string{"status": "check your email"}

func (h *HTTP) signUp(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password, Turnstile string }
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.s.SignUp(r.Context(), in.Email, in.Password, in.Turnstile, h.clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, checkEmail)
}

func (h *HTTP) confirm(w http.ResponseWriter, r *http.Request) {
	var in struct{ T string }
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.s.Confirm(r.Context(), in.T); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

func (h *HTTP) resend(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := h.s.ResendConfirm(r.Context(), u, h.clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, checkEmail)
}

func (h *HTTP) signIn(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	raw, u, err := h.s.SignIn(r.Context(), in.Email, in.Password, h.clientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.setSession(w, r, raw)
	writeJSON(w, http.StatusOK, map[string]userJSON{"user": h.userJSON(u)})
}

func (h *HTTP) signOut(w http.ResponseWriter, r *http.Request) {
	if err := h.s.SignOut(r.Context(), sessionToken(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	h.clearSession(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) forgot(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Turnstile string }
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.s.Forgot(r.Context(), in.Email, in.Turnstile, h.clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, checkEmail)
}

func (h *HTTP) reset(w http.ResponseWriter, r *http.Request) {
	var in struct{ T, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	raw, err := h.s.Reset(r.Context(), in.T, in.Password)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.setSession(w, r, raw)
	writeJSON(w, http.StatusOK, map[string]string{"status": "password changed"})
}

func (h *HTTP) password(w http.ResponseWriter, r *http.Request, u store.User) {
	var in struct{ Current, New string }
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.s.ChangePassword(r.Context(), u, in.Current, in.New, sessionToken(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) setName(w http.ResponseWriter, r *http.Request, u store.User) {
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	name, err := h.s.SetName(r.Context(), u, in.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (h *HTTP) deleteAccount(w http.ResponseWriter, r *http.Request, u store.User) {
	var in struct{ Password string }
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.s.DeleteAccount(r.Context(), u, in.Password); err != nil {
		h.fail(w, r, err)
		return
	}
	h.clearSession(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// signedIn adapts a handler that needs a signed-in (not necessarily
// confirmed) user.
func (h *HTTP) signedIn(f func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFrom(r.Context())
		if !ok {
			writeErr(w, http.StatusUnauthorized, "sign in required")
			return
		}
		f(w, r, u)
	}
}

// CSRF rejects requests whose Origin or Referer is not this site.
func (h *HTTP) CSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		next(w, r)
	}
}

// sameOrigin reports whether a request may change state: safe methods
// always may; others need an Origin (or, failing that, Referer) whose host is
// the request's Host or BaseURL's host.
func (h *HTTP) sameOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) || (h.baseHost != "" && strings.EqualFold(u.Host, h.baseHost))
}

// remoteAddr returns the connection's address and whether it is a trusted
// proxy.
func (h *HTTP) remoteAddr(r *http.Request) (string, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr, false
	}
	a := ap.Addr().Unmap()
	for _, p := range h.o.Trusted {
		if p.Contains(a) {
			return a.String(), true
		}
	}
	return a.String(), false
}

func (h *HTTP) clientIP(r *http.Request) string {
	ip, trusted := h.remoteAddr(r)
	if trusted {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); err == nil {
			return a.Unmap().String()
		}
	}
	return ip
}

func (h *HTTP) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	_, trusted := h.remoteAddr(r)
	return trusted && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func (h *HTTP) setSession(w http.ResponseWriter, r *http.Request, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: raw, Path: "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.isHTTPS(r),
	})
}

func (h *HTTP) clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.isHTTPS(r),
	})
}

// readJSON decodes a request body of at most 16 KiB into v, writing an error
// response and returning false if it can't.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "Request is too large.")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid request")
		}
		return false
	}
	return true
}

// fail writes the response for an error from the Service.
func (h *HTTP) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := http.StatusInternalServerError, "internal error"
	switch {
	case errors.Is(err, ErrInvalidEmail):
		code, msg = http.StatusBadRequest, "Enter a valid email address."
	case errors.Is(err, ErrWeakPassword):
		code, msg = http.StatusBadRequest, "Password must be 10 to 200 characters."
	case errors.Is(err, ErrBadToken):
		code, msg = http.StatusBadRequest, "This link has expired or was already used."
	case errors.Is(err, ErrBadCredentials):
		code, msg = http.StatusUnauthorized, "Email or password is incorrect."
	case errors.Is(err, ErrInvalidName):
		code, msg = http.StatusBadRequest, "Name must be at most 60 characters."
	case errors.Is(err, ErrWrongPassword):
		code, msg = http.StatusUnauthorized, "Current password is incorrect."
	case errors.Is(err, ErrRateLimited):
		code, msg = http.StatusTooManyRequests, "Too many attempts. Try again later."
	case errors.Is(err, ErrBot):
		code, msg = http.StatusBadRequest, "Couldn't verify you're not a robot."
	case errors.Is(err, ErrBotUnavailable):
		code, msg = http.StatusServiceUnavailable, "Couldn't check you're not a robot. Try again."
	default:
		h.s.o.Log.Error("request failed", "path", r.URL.Path, "err", err)
	}
	writeErr(w, code, msg)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
