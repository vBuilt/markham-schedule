package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/store"
)

const testBase = "https://example.test"

type sentMail struct{ to, subject, text, html string }

type fakeMailer struct{ sent []sentMail }

func (m *fakeMailer) Send(_ context.Context, to, subject, text, html string) error {
	m.sent = append(m.sent, sentMail{to, subject, text, html})
	return nil
}

type fakeBot struct{ err error }

func (b *fakeBot) Verify(context.Context, string, string) error { return b.err }

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type harness struct {
	svc   *Service
	st    *store.Store
	path  string
	mail  *fakeMailer
	bot   *fakeBot
	clock *fakeClock
}

func newHarness(t *testing.T, apiToken string) *harness {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{st: st, path: p, mail: &fakeMailer{}, bot: &fakeBot{},
		clock: &fakeClock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}}
	h.svc = NewService(st, Options{
		BaseURL: testBase, APIToken: apiToken, Mailer: h.mail, Bot: h.bot,
		Now: h.clock.Now, Async: func(f func()) { f() },
	})
	return h
}

var ctx = context.Background()

const (
	pw  = "correct horse"
	pw2 = "battery staple"
)

var tokenRE = regexp.MustCompile(`[?&]t=([A-Za-z0-9_-]+)`)

// lastToken returns the one-time token in the most recent email, checking
// that its link has the given path.
func (h *harness) lastToken(t *testing.T, path string) string {
	t.Helper()
	if len(h.mail.sent) == 0 {
		t.Fatal("no email sent")
	}
	text := h.mail.sent[len(h.mail.sent)-1].text
	if !strings.Contains(text, testBase+path+"?t=") {
		t.Fatalf("email lacks %s link:\n%s", path, text)
	}
	m := tokenRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no token in email:\n%s", text)
	}
	return m[1]
}

func (h *harness) sessionRows(t *testing.T) int {
	t.Helper()
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// confirmedUser signs up and confirms email, returning the user.
func (h *harness) confirmedUser(t *testing.T, email, password string) store.User {
	t.Helper()
	if err := h.svc.SignUp(ctx, email, password, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Confirm(ctx, h.lastToken(t, "/verify")); err != nil {
		t.Fatal(err)
	}
	u, err := h.st.UserByEmail(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (h *harness) signIn(t *testing.T, email, password string) string {
	t.Helper()
	raw, _, err := h.svc.SignIn(ctx, email, password, "2.2.2.2")
	if err != nil {
		t.Fatalf("SignIn(%s): %v", email, err)
	}
	return raw
}

func TestSignUpSendsConfirmAndDoesNotSignIn(t *testing.T) {
	h := newHarness(t, "")
	if err := h.svc.SignUp(ctx, "  New@Example.com ", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if len(h.mail.sent) != 1 || h.mail.sent[0].to != "new@example.com" {
		t.Fatalf("sent = %+v", h.mail.sent)
	}
	if h.mail.sent[0].subject != "Confirm your email" {
		t.Errorf("subject = %q", h.mail.sent[0].subject)
	}
	h.lastToken(t, "/verify")
	u, err := h.st.UserByEmail(ctx, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.Confirmed {
		t.Error("user confirmed before clicking link")
	}
	if n := h.sessionRows(t); n != 0 {
		t.Errorf("user_sessions rows = %d, want 0", n)
	}
}

func TestSignUpExistingConfirmedIsIndistinguishable(t *testing.T) {
	h := newHarness(t, "")
	h.confirmedUser(t, "a@example.com", pw)
	h.mail.sent = nil
	if err := h.svc.SignUp(ctx, "a@example.com", pw2, "bot", "1.1.1.1"); err != nil {
		t.Fatalf("SignUp existing = %v, want nil", err)
	}
	h.signIn(t, "a@example.com", pw)
	if len(h.mail.sent) != 1 {
		t.Fatalf("sent %d emails, want 1", len(h.mail.sent))
	}
	if h.mail.sent[0].subject != "Someone tried to sign up with your email" {
		t.Errorf("subject = %q", h.mail.sent[0].subject)
	}
	h.lastToken(t, "/reset")
}

func TestSignUpExistingUnconfirmedResendsConfirm(t *testing.T) {
	h := newHarness(t, "")
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	oldTok := h.lastToken(t, "/verify")
	early := h.signIn(t, "a@example.com", pw)
	h.mail.sent = nil
	if err := h.svc.SignUp(ctx, "a@example.com", pw2, "bot", "1.1.1.1"); err != nil {
		t.Fatalf("SignUp again = %v, want nil", err)
	}
	if len(h.mail.sent) != 1 || h.mail.sent[0].subject != "Confirm your email" {
		t.Fatalf("sent = %+v, want one confirm email", h.mail.sent)
	}
	newTok := h.lastToken(t, "/verify")
	if _, err := h.svc.SessionUser(ctx, early); err == nil {
		t.Error("earlier session survived repeat sign-up")
	}
	if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw, "2.2.2.2"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("old password SignIn = %v, want ErrBadCredentials", err)
	}
	h.signIn(t, "a@example.com", pw2)
	if err := h.svc.Confirm(ctx, oldTok); !errors.Is(err, ErrBadToken) {
		t.Errorf("old confirm token = %v, want ErrBadToken", err)
	}
	if err := h.svc.Confirm(ctx, newTok); err != nil {
		t.Errorf("new confirm token = %v", err)
	}
}

func TestConfirmSignsOutEarlierSessions(t *testing.T) {
	h := newHarness(t, "")
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	tok := h.lastToken(t, "/verify")
	early := h.signIn(t, "a@example.com", pw)
	if err := h.svc.Confirm(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SessionUser(ctx, early); err == nil {
		t.Error("session from before confirmation still valid")
	}
	h.signIn(t, "a@example.com", pw)
}

func TestSignUpValidation(t *testing.T) {
	h := newHarness(t, "")
	if err := h.svc.SignUp(ctx, "not-an-email", pw, "bot", "1.1.1.1"); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("bad email = %v", err)
	}
	if err := h.svc.SignUp(ctx, "a@example.com", "123456789", "bot", "1.1.1.1"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("9-char password = %v", err)
	}
	if err := h.svc.SignUp(ctx, "a@example.com", strings.Repeat("x", 201), "bot", "1.1.1.1"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("201-char password = %v", err)
	}
	h.bot.err = ErrBot
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bad", "1.1.1.1"); !errors.Is(err, ErrBot) {
		t.Errorf("bot fails = %v", err)
	}
	h.bot.err = ErrBotUnavailable
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); !errors.Is(err, ErrBotUnavailable) {
		t.Errorf("bot unavailable = %v", err)
	}
	h.bot.err = nil
	for i := 0; i < 3; i++ {
		if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
			t.Fatalf("sign-up %d: %v", i+1, err)
		}
	}
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("4th sign-up = %v, want ErrRateLimited", err)
	}
	if len(h.mail.sent) != 3 {
		t.Errorf("sent %d emails, want 3", len(h.mail.sent))
	}
}

func TestSignUpLimitedPerIP(t *testing.T) {
	h := newHarness(t, "")
	for i := 0; i < 10; i++ {
		if err := h.svc.SignUp(ctx, "u"+string(rune('a'+i))+"@example.com", pw, "bot", "9.9.9.9"); err != nil {
			t.Fatalf("sign-up %d: %v", i+1, err)
		}
	}
	if err := h.svc.SignUp(ctx, "z@example.com", pw, "bot", "9.9.9.9"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("11th sign-up from one IP = %v, want ErrRateLimited", err)
	}
	h.clock.Advance(61 * time.Minute)
	if err := h.svc.SignUp(ctx, "z@example.com", pw, "bot", "9.9.9.9"); err != nil {
		t.Errorf("after an hour = %v", err)
	}
}

func TestConfirm(t *testing.T) {
	h := newHarness(t, "")
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	tok := h.lastToken(t, "/verify")
	if err := h.svc.Confirm(ctx, tok); err != nil {
		t.Fatal(err)
	}
	u, _ := h.st.UserByEmail(ctx, "a@example.com")
	if !u.Confirmed {
		t.Error("not confirmed")
	}
	if err := h.svc.Confirm(ctx, tok); !errors.Is(err, ErrBadToken) {
		t.Errorf("reuse = %v, want ErrBadToken", err)
	}
	if err := h.svc.Confirm(ctx, ""); !errors.Is(err, ErrBadToken) {
		t.Errorf("empty = %v, want ErrBadToken", err)
	}

	if err := h.svc.SignUp(ctx, "b@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	tok = h.lastToken(t, "/verify")
	h.clock.Advance(25 * time.Hour)
	if err := h.svc.Confirm(ctx, tok); !errors.Is(err, ErrBadToken) {
		t.Errorf("after 25h = %v, want ErrBadToken", err)
	}
}

func TestResendConfirm(t *testing.T) {
	h := newHarness(t, "")
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	u, _ := h.st.UserByEmail(ctx, "a@example.com")
	for i := 0; i < 2; i++ {
		if err := h.svc.ResendConfirm(ctx, u, "1.1.1.1"); err != nil {
			t.Fatalf("resend %d: %v", i+1, err)
		}
	}
	if err := h.svc.ResendConfirm(ctx, u, "1.1.1.1"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("4th email for address in an hour = %v, want ErrRateLimited", err)
	}
	if len(h.mail.sent) != 3 {
		t.Fatalf("sent %d emails, want 3", len(h.mail.sent))
	}
	if err := h.svc.Confirm(ctx, h.lastToken(t, "/verify")); err != nil {
		t.Fatal(err)
	}
	u, _ = h.st.UserByEmail(ctx, "a@example.com")
	h.clock.Advance(2 * time.Hour)
	if err := h.svc.ResendConfirm(ctx, u, "1.1.1.1"); err != nil {
		t.Errorf("resend for confirmed user = %v", err)
	}
	if len(h.mail.sent) != 3 {
		t.Errorf("confirmed user got another confirm email")
	}
}

func TestSignInRules(t *testing.T) {
	h := newHarness(t, "")
	if _, _, err := h.svc.SignIn(ctx, "nobody@example.com", pw, "2.2.2.2"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("unknown email = %v", err)
	}
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw2, "2.2.2.2"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("wrong password = %v", err)
	}
	raw, u, err := h.svc.SignIn(ctx, " A@Example.com", pw, "2.2.2.2")
	if err != nil || raw == "" || u.Email != "a@example.com" || u.Confirmed {
		t.Fatalf("unconfirmed sign-in = %q, %+v, %v", raw, u, err)
	}
	// The success above reset the counter: five fresh failures, then locked.
	for i := 0; i < 5; i++ {
		if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw2, "2.2.2.2"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("failure %d = %v", i+1, err)
		}
	}
	if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw, "2.2.2.2"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("6th attempt = %v, want ErrRateLimited", err)
	}
	if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw, "3.3.3.3"); err != nil {
		t.Errorf("other IP = %v", err)
	}
	h.clock.Advance(16 * time.Minute)
	if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw, "2.2.2.2"); err != nil {
		t.Errorf("after 16 min = %v", err)
	}
}

func TestSessionUserSliding(t *testing.T) {
	h := newHarness(t, "")
	h.confirmedUser(t, "a@example.com", pw)
	start := h.clock.t
	at := func(d time.Duration) { h.clock.t = start.Add(d) }
	day := 24 * time.Hour

	used := h.signIn(t, "a@example.com", pw)
	idle := h.signIn(t, "a@example.com", pw)

	at(2 * day)
	if u, err := h.svc.SessionUser(ctx, used); err != nil || u.Email != "a@example.com" {
		t.Fatalf("day 2 = %+v, %v", u, err)
	}
	at(29 * day)
	if _, err := h.svc.SessionUser(ctx, idle); err != nil {
		t.Errorf("idle at day 29 = %v", err)
	}
	at(31 * day)
	if _, err := h.svc.SessionUser(ctx, used); err != nil {
		t.Errorf("used at day 2, checked at day 31 = %v", err)
	}
	h2 := newHarness(t, "")
	h2.confirmedUser(t, "b@example.com", pw)
	idle2 := h2.signIn(t, "b@example.com", pw)
	h2.clock.Advance(31 * day)
	if _, err := h2.svc.SessionUser(ctx, idle2); err == nil {
		t.Error("never-used session valid at day 31")
	}
	if _, err := h.svc.SessionUser(ctx, "garbage"); err == nil {
		t.Error("unknown session valid")
	}
}

func TestSignOut(t *testing.T) {
	h := newHarness(t, "")
	h.confirmedUser(t, "a@example.com", pw)
	raw := h.signIn(t, "a@example.com", pw)
	if err := h.svc.SignOut(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SessionUser(ctx, raw); err == nil {
		t.Error("session valid after sign-out")
	}
}

func TestForgotAndReset(t *testing.T) {
	h := newHarness(t, "")
	if err := h.svc.Forgot(ctx, "nobody@example.com", "bot", "1.1.1.1"); err != nil {
		t.Errorf("unknown email = %v", err)
	}
	if len(h.mail.sent) != 0 {
		t.Fatalf("sent %d emails for unknown address", len(h.mail.sent))
	}
	h.confirmedUser(t, "a@example.com", pw)
	old := h.signIn(t, "a@example.com", pw)

	if err := h.svc.Forgot(ctx, "A@example.com", "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if s := h.mail.sent[len(h.mail.sent)-1].subject; s != "Reset your password" {
		t.Errorf("subject = %q", s)
	}
	tok := h.lastToken(t, "/reset")
	if _, err := h.svc.Reset(ctx, tok, "123456789"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("weak = %v", err)
	}
	raw, err := h.svc.Reset(ctx, tok, pw2)
	if err != nil || raw == "" {
		t.Fatalf("Reset = %q, %v", raw, err)
	}
	if _, err := h.svc.SessionUser(ctx, raw); err != nil {
		t.Errorf("new session = %v", err)
	}
	if _, err := h.svc.SessionUser(ctx, old); err == nil {
		t.Error("old session survived reset")
	}
	if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw, "4.4.4.4"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("old password = %v", err)
	}
	h.signIn(t, "a@example.com", pw2)
	if _, err := h.svc.Reset(ctx, tok, pw); !errors.Is(err, ErrBadToken) {
		t.Errorf("reuse = %v", err)
	}

	if err := h.svc.Forgot(ctx, "a@example.com", "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	tok = h.lastToken(t, "/reset")
	h.clock.Advance(61 * time.Minute)
	if _, err := h.svc.Reset(ctx, tok, pw); !errors.Is(err, ErrBadToken) {
		t.Errorf("after 61 min = %v", err)
	}

	h.bot.err = ErrBot
	if err := h.svc.Forgot(ctx, "a@example.com", "bad", "1.1.1.1"); !errors.Is(err, ErrBot) {
		t.Errorf("bot fails = %v", err)
	}
}

func TestChangePasswordKeepsCurrentSession(t *testing.T) {
	h := newHarness(t, "")
	u := h.confirmedUser(t, "a@example.com", pw)
	keep := h.signIn(t, "a@example.com", pw)
	other := h.signIn(t, "a@example.com", pw)
	if err := h.svc.ChangePassword(ctx, u, pw2, "another pass", keep); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("wrong current = %v", err)
	}
	if err := h.svc.ChangePassword(ctx, u, pw, "short", keep); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("weak next = %v", err)
	}
	if err := h.svc.ChangePassword(ctx, u, pw, pw2, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SessionUser(ctx, keep); err != nil {
		t.Errorf("kept session = %v", err)
	}
	if _, err := h.svc.SessionUser(ctx, other); err == nil {
		t.Error("other session survived")
	}
	h.signIn(t, "a@example.com", pw2)
}

func TestDeleteAccount(t *testing.T) {
	h := newHarness(t, "")
	u := h.confirmedUser(t, "a@example.com", pw)
	raw := h.signIn(t, "a@example.com", pw)
	if err := h.svc.DeleteAccount(ctx, u, pw2); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("wrong password = %v", err)
	}
	if err := h.svc.DeleteAccount(ctx, u, pw); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.UserByEmail(ctx, "a@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UserByEmail = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.SessionUser(ctx, raw); err == nil {
		t.Error("session valid after delete")
	}
}

func TestCleanupRemovesStaleUnconfirmed(t *testing.T) {
	h := newHarness(t, "")
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	h.confirmedUser(t, "b@example.com", pw)
	h.clock.Advance(8 * 24 * time.Hour)
	if err := h.svc.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.UserByEmail(ctx, "a@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("stale unconfirmed = %v", err)
	}
	if _, err := h.st.UserByEmail(ctx, "b@example.com"); err != nil {
		t.Errorf("confirmed user removed: %v", err)
	}
}

func TestEmailLinksCarryAPIToken(t *testing.T) {
	h := newHarness(t, "k")
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	m := h.mail.sent[0]
	if !strings.Contains(m.text, "&token=k") || !strings.Contains(m.html, "&amp;token=k") {
		t.Errorf("links lack API token:\n%s\n%s", m.text, m.html)
	}
	h2 := newHarness(t, "")
	if err := h2.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h2.mail.sent[0].text, "token=") {
		t.Errorf("token in link without API token:\n%s", h2.mail.sent[0].text)
	}
}

func TestPasswordChangesCancelResetLinks(t *testing.T) {
	h := newHarness(t, "")
	u := h.confirmedUser(t, "a@example.com", pw)
	forgot := func() string {
		t.Helper()
		if err := h.svc.Forgot(ctx, "a@example.com", "bot", "1.1.1.1"); err != nil {
			t.Fatal(err)
		}
		return h.lastToken(t, "/reset")
	}
	first, second := forgot(), forgot()
	if _, err := h.svc.Reset(ctx, first, pw2); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Reset(ctx, second, pw); !errors.Is(err, ErrBadToken) {
		t.Errorf("second reset link after Reset = %v, want ErrBadToken", err)
	}

	h.clock.Advance(2 * time.Hour) // clear the per-address email limit
	link := forgot()
	keep := h.signIn(t, "a@example.com", pw2)
	if err := h.svc.ChangePassword(ctx, u, pw2, pw, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Reset(ctx, link, pw2); !errors.Is(err, ErrBadToken) {
		t.Errorf("reset link after ChangePassword = %v, want ErrBadToken", err)
	}
}

func TestChangePasswordRateLimited(t *testing.T) {
	h := newHarness(t, "")
	u := h.confirmedUser(t, "a@example.com", pw)
	keep := h.signIn(t, "a@example.com", pw)
	for i := 0; i < 5; i++ {
		if err := h.svc.ChangePassword(ctx, u, "wrong password", pw2, keep); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("failure %d = %v", i+1, err)
		}
	}
	if err := h.svc.ChangePassword(ctx, u, pw, pw2, keep); !errors.Is(err, ErrRateLimited) {
		t.Errorf("6th attempt = %v, want ErrRateLimited", err)
	}
	if err := h.svc.DeleteAccount(ctx, u, pw); !errors.Is(err, ErrRateLimited) {
		t.Errorf("DeleteAccount while locked = %v, want ErrRateLimited", err)
	}
	h.clock.Advance(16 * time.Minute)
	if err := h.svc.ChangePassword(ctx, u, pw, pw2, keep); err != nil {
		t.Fatalf("after 16 min = %v", err)
	}
	// Success reset the counter: four more failures are not locked out.
	for i := 0; i < 4; i++ {
		if err := h.svc.ChangePassword(ctx, u, "wrong password", pw, keep); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("post-success failure %d = %v", i+1, err)
		}
	}
	if err := h.svc.ChangePassword(ctx, u, pw2, pw, keep); err != nil {
		t.Errorf("5th attempt after reset = %v", err)
	}
}

// countArgon counts argon2 key derivations until the test ends.
func countArgon(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := argonKey
	argonKey = func(pw, salt []byte, tm, m uint32, p uint8, l uint32) []byte {
		n.Add(1)
		return orig(pw, salt, tm, m, p, l)
	}
	t.Cleanup(func() { argonKey = orig })
	return &n
}

func TestResetChecksTokenBeforeHashing(t *testing.T) {
	h := newHarness(t, "")
	n := countArgon(t)
	if _, err := h.svc.Reset(ctx, "not-a-real-token", pw); !errors.Is(err, ErrBadToken) {
		t.Fatalf("bad token = %v, want ErrBadToken", err)
	}
	if got := n.Load(); got != 0 {
		t.Errorf("bad-token reset ran argon2 %d times, want 0", got)
	}
	// A weak password still doesn't burn a good token.
	h.confirmedUser(t, "a@example.com", pw)
	if err := h.svc.Forgot(ctx, "a@example.com", "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	tok := h.lastToken(t, "/reset")
	if _, err := h.svc.Reset(ctx, tok, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak = %v", err)
	}
	if _, err := h.svc.Reset(ctx, tok, pw2); err != nil {
		t.Fatalf("token burnt by weak password: %v", err)
	}
}

func TestResetConfirmsUser(t *testing.T) {
	h := newHarness(t, "")
	if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Forgot(ctx, "a@example.com", "bot", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Reset(ctx, h.lastToken(t, "/reset"), pw2); err != nil {
		t.Fatal(err)
	}
	u, err := h.st.UserByEmail(ctx, "a@example.com")
	if err != nil || !u.Confirmed {
		t.Fatalf("after reset = %+v, %v; want confirmed", u, err)
	}
}

func TestSignInLimitedPerIP(t *testing.T) {
	h := newHarness(t, "")
	h.confirmedUser(t, "a@example.com", pw)
	const ip = "2.2.2.2"             // h.signIn's address
	h.signIn(t, "a@example.com", pw) // success must not reset the per-IP count
	for i := 0; i < 29; i++ {
		email := fmt.Sprintf("x%d@example.com", i)
		if _, _, err := h.svc.SignIn(ctx, email, pw, ip); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d = %v", i+2, err)
		}
	}
	n := countArgon(t)
	if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw, ip); !errors.Is(err, ErrRateLimited) {
		t.Errorf("31st attempt from IP = %v, want ErrRateLimited", err)
	}
	if got := n.Load(); got != 0 {
		t.Errorf("refused sign-in ran argon2 %d times", got)
	}
	if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw, "8.8.8.8"); err != nil {
		t.Errorf("other IP = %v", err)
	}
	h.clock.Advance(16 * time.Minute)
	if _, _, err := h.svc.SignIn(ctx, "a@example.com", pw, ip); err != nil {
		t.Errorf("after 16 min = %v", err)
	}
}

func TestRepeatSignUpAndResendRestartPurgeClock(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		name  string
		again func(h *harness)
	}{
		{"resend", func(h *harness) {
			u, _ := h.st.UserByEmail(ctx, "a@example.com")
			if err := h.svc.ResendConfirm(ctx, u, "1.1.1.1"); err != nil {
				t.Fatal(err)
			}
		}},
		{"repeat sign-up", func(h *harness) {
			if err := h.svc.SignUp(ctx, "a@example.com", pw2, "bot", "1.1.1.1"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		h := newHarness(t, "")
		if err := h.svc.SignUp(ctx, "a@example.com", pw, "bot", "1.1.1.1"); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(6 * day)
		tc.again(h)
		h.clock.Advance(2 * day)
		if err := h.svc.Cleanup(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := h.st.UserByEmail(ctx, "a@example.com"); err != nil {
			t.Errorf("%s at day 6: purged at day 8: %v", tc.name, err)
		}
		h.clock.Advance(6 * day)
		if err := h.svc.Cleanup(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := h.st.UserByEmail(ctx, "a@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: kept 8 days after last email: %v", tc.name, err)
		}
	}
}

func TestServicePrune(t *testing.T) {
	h := newHarness(t, "")
	h.confirmedUser(t, "a@example.com", pw)
	h.svc.SignIn(ctx, "a@example.com", pw2, "2.2.2.2")
	h.svc.ChangePassword(ctx, store.User{ID: 1}, pw2, pw, "")
	h.clock.Advance(2 * time.Hour)
	h.svc.Prune()
	for name, l := range map[string]*Limiter{
		"email": h.svc.emailLimit, "ip": h.svc.ipLimit, "signIn": h.svc.signInLimit,
		"signInIP": h.svc.signInIPLimit, "current": h.svc.currentLimit,
	} {
		l.mu.Lock()
		if len(l.hits) != 0 {
			t.Errorf("%s limiter kept %d keys after prune", name, len(l.hits))
		}
		l.mu.Unlock()
	}
}
