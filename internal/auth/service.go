package auth

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vBuilt/markham-schedule/internal/store"
)

// Store is the persistence the account service needs; *store.Store satisfies it.
type Store interface {
	CreateUser(ctx context.Context, email, hash string, now time.Time) (store.User, error)
	UserByEmail(ctx context.Context, email string) (store.User, error)
	UserByID(ctx context.Context, id int64) (store.User, error)
	SetPassword(ctx context.Context, userID int64, hash string) error
	SetName(ctx context.Context, userID int64, name string) error
	ConfirmUser(ctx context.Context, userID int64) error
	TouchUser(ctx context.Context, userID int64, now time.Time) error
	DeleteUser(ctx context.Context, userID int64) error
	CreateAuthSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time) error
	AuthSession(ctx context.Context, tokenHash []byte, now time.Time) (int64, time.Time, error)
	ExtendAuthSession(ctx context.Context, tokenHash []byte, expires time.Time) error
	DeleteAuthSession(ctx context.Context, tokenHash []byte) error
	DeleteUserSessions(ctx context.Context, userID int64, except []byte) error
	CreateToken(ctx context.Context, tokenHash []byte, kind string, userID int64, expires time.Time) error
	UseToken(ctx context.Context, tokenHash []byte, kind string, now time.Time) (int64, error)
	DeleteUserTokens(ctx context.Context, userID int64, kind string) error
	PurgeAuth(ctx context.Context, now, unconfirmedBefore time.Time) (int64, error)
}

var _ Store = (*store.Store)(nil)

var (
	ErrInvalidEmail   = errors.New("invalid email address")
	ErrWeakPassword   = errors.New("password must be 10 to 200 characters")
	ErrBadCredentials = errors.New("email or password is incorrect")
	ErrRateLimited    = errors.New("too many attempts")
	ErrBadToken       = errors.New("link is invalid or has expired")
	ErrWrongPassword  = errors.New("current password is incorrect")
	ErrInvalidName    = errors.New("name must be at most 60 characters")
)

const (
	sessionTTL     = 30 * 24 * time.Hour
	renewThreshold = 29 * 24 * time.Hour // renew once expiry is closer than this (at most daily)
	confirmTTL     = 24 * time.Hour
	resetTTL       = time.Hour
	unconfirmedTTL = 7 * 24 * time.Hour
	mailTimeout    = 30 * time.Second

	kindConfirm = "confirm"
	kindReset   = "reset"

	minPassword = 10
	maxPassword = 200
)

// Options configures a Service. Zero values get sensible defaults.
type Options struct {
	// BaseURL prefixes emailed links; APIToken, when set, is appended to them
	// as &token= so they pass the API token gate.
	BaseURL, APIToken string
	Mailer            Mailer
	Bot               BotCheck
	Log               *slog.Logger
	Now               func() time.Time
	Async             func(func()) // runs email sends; default go f()
	// AdminEmails are normalised addresses that make a confirmed user an admin.
	AdminEmails []string
}

// Service implements the account rules on top of a Store.
type Service struct {
	st Store
	o  Options

	emailLimit    *Limiter // emails sent per address
	ipLimit       *Limiter // email-sending requests per IP
	signInLimit   *Limiter // sign-in attempts per email+IP
	signInIPLimit *Limiter // sign-in attempts per IP, whatever the email
	currentLimit  *Limiter // current-password checks per user ID

	dummyHash string
}

func NewService(st Store, o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Mailer == nil {
		o.Mailer = LogMailer{Log: o.Log}
	}
	if o.Bot == nil {
		o.Bot = NoBotCheck{}
	}
	if o.Async == nil {
		o.Async = func(f func()) { go f() }
	}
	// A hash to check against when the email has no account, so sign-in takes
	// the same time either way.
	dummy, err := HashPassword("no account has this password")
	if err != nil {
		panic(fmt.Sprintf("auth: dummy hash: %v", err))
	}
	return &Service{
		st:            st,
		o:             o,
		emailLimit:    NewLimiter(3, time.Hour, o.Now),
		ipLimit:       NewLimiter(10, time.Hour, o.Now),
		signInLimit:   NewLimiter(5, 15*time.Minute, o.Now),
		signInIPLimit: NewLimiter(30, 15*time.Minute, o.Now),
		currentLimit:  NewLimiter(5, 15*time.Minute, o.Now),
		dummyHash:     dummy,
	}
}

func validPassword(pw string) bool {
	n := utf8.RuneCountInString(pw)
	return n >= minPassword && n <= maxPassword
}

// allowEmail applies the per-address and per-IP limits on requests that send
// email. Both counters are charged.
func (s *Service) allowEmail(email, ip string) bool {
	okEmail := s.emailLimit.Allow(email)
	okIP := s.ipLimit.Allow(ip)
	return okEmail && okIP
}

func (s *Service) SignUp(ctx context.Context, email, password, botToken, ip string) error {
	email, ok := NormalizeEmail(email)
	if !ok {
		return ErrInvalidEmail
	}
	if !validPassword(password) {
		return ErrWeakPassword
	}
	if err := s.o.Bot.Verify(ctx, botToken, ip); err != nil {
		return err
	}
	if !s.allowEmail(email, ip) {
		return ErrRateLimited
	}
	// Hash before looking the address up so new and existing addresses take
	// the same time.
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	created := false
	u, err := s.st.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		u, err = s.st.CreateUser(ctx, email, hash, s.o.Now())
		created = err == nil
		if errors.Is(err, store.ErrEmailTaken) { // lost a race with another sign-up
			u, err = s.st.UserByEmail(ctx, email)
		}
	}
	if err != nil {
		return err
	}
	if u.Confirmed {
		return s.sendTakenNotice(ctx, u)
	}
	if !created {
		// Whoever signs up last owns the unconfirmed account: otherwise someone
		// could pre-register a victim's address with a password they know and
		// inherit the account when the victim confirms.
		if err := s.st.SetPassword(ctx, u.ID, hash); err != nil {
			return err
		}
		if err := s.st.DeleteUserSessions(ctx, u.ID, nil); err != nil {
			return err
		}
		if err := s.st.DeleteUserTokens(ctx, u.ID, kindConfirm); err != nil {
			return err
		}
		// A fresh confirmation email gets a fresh 7 days before purge.
		if err := s.st.TouchUser(ctx, u.ID, s.o.Now()); err != nil {
			return err
		}
	}
	return s.sendConfirm(ctx, u)
}

func (s *Service) Confirm(ctx context.Context, rawToken string) error {
	uid, err := s.useToken(ctx, rawToken, kindConfirm)
	if err != nil {
		return err
	}
	if err := s.st.ConfirmUser(ctx, uid); err != nil {
		return err
	}
	// Sessions from before confirmation may belong to someone else who signed
	// up with this address.
	return s.st.DeleteUserSessions(ctx, uid, nil)
}

// ResendConfirm emails a fresh confirmation link; it does nothing for a user
// who is already confirmed.
func (s *Service) ResendConfirm(ctx context.Context, u store.User, ip string) error {
	if u.Confirmed {
		return nil
	}
	if !s.allowEmail(u.Email, ip) {
		return ErrRateLimited
	}
	// A fresh confirmation email gets a fresh 7 days before purge.
	if err := s.st.TouchUser(ctx, u.ID, s.o.Now()); err != nil {
		return err
	}
	return s.sendConfirm(ctx, u)
}

func (s *Service) SignIn(ctx context.Context, email, password, ip string) (string, store.User, error) {
	email, _ = NormalizeEmail(email)
	// Per IP first, so one address can't spray guesses (and argon2 work)
	// across many emails. Success never resets this one.
	if !s.signInIPLimit.Allow(ip) {
		return "", store.User{}, ErrRateLimited
	}
	key := email + "|" + ip
	if !s.signInLimit.Allow(key) {
		return "", store.User{}, ErrRateLimited
	}
	u, err := s.st.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		CheckPassword(s.dummyHash, password)
		return "", store.User{}, ErrBadCredentials
	}
	if err != nil {
		return "", store.User{}, err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return "", store.User{}, ErrBadCredentials
	}
	s.signInLimit.Reset(key)
	raw, err := s.newSession(ctx, u.ID)
	if err != nil {
		return "", store.User{}, err
	}
	return raw, u, nil
}

// SessionUser returns the user a session belongs to, or store.ErrNotFound if
// the session is missing or expired. It renews the session at most once a day.
func (s *Service) SessionUser(ctx context.Context, rawSession string) (store.User, error) {
	if rawSession == "" {
		return store.User{}, store.ErrNotFound
	}
	hash := HashToken(rawSession)
	now := s.o.Now()
	uid, expires, err := s.st.AuthSession(ctx, hash, now)
	if err != nil {
		return store.User{}, err
	}
	u, err := s.st.UserByID(ctx, uid)
	if err != nil {
		return store.User{}, err
	}
	if expires.Sub(now) < renewThreshold {
		if err := s.st.ExtendAuthSession(ctx, hash, now.Add(sessionTTL)); err != nil {
			return store.User{}, err
		}
	}
	return u, nil
}

func (s *Service) SignOut(ctx context.Context, rawSession string) error {
	if rawSession == "" {
		return nil
	}
	return s.st.DeleteAuthSession(ctx, HashToken(rawSession))
}

// Forgot emails a reset link if the address has an account. It returns nil
// either way.
func (s *Service) Forgot(ctx context.Context, email, botToken, ip string) error {
	email, ok := NormalizeEmail(email)
	if !ok {
		return ErrInvalidEmail
	}
	if err := s.o.Bot.Verify(ctx, botToken, ip); err != nil {
		return err
	}
	if !s.allowEmail(email, ip) {
		return ErrRateLimited
	}
	u, err := s.st.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.sendReset(ctx, u)
}

// Reset sets a new password from a reset link, signs the user out everywhere
// and returns a fresh session for the current device.
func (s *Service) Reset(ctx context.Context, rawToken, newPassword string) (string, error) {
	// Length first so a weak password doesn't burn the link; then the token,
	// so a bad link costs no argon2 work.
	if !validPassword(newPassword) {
		return "", ErrWeakPassword
	}
	uid, err := s.useToken(ctx, rawToken, kindReset)
	if err != nil {
		return "", err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return "", err
	}
	if err := s.st.SetPassword(ctx, uid, hash); err != nil {
		return "", err
	}
	// The link reached the inbox, which proves ownership as a confirm link would.
	if err := s.st.ConfirmUser(ctx, uid); err != nil {
		return "", err
	}
	if err := s.st.DeleteUserSessions(ctx, uid, nil); err != nil {
		return "", err
	}
	if err := s.st.DeleteUserTokens(ctx, uid, kindReset); err != nil {
		return "", err
	}
	return s.newSession(ctx, uid)
}

// ChangePassword sets a new password and signs out every session but keepSession.
func (s *Service) ChangePassword(ctx context.Context, u store.User, current, next, keepSession string) error {
	if err := s.checkCurrent(ctx, u.ID, current); err != nil {
		return err
	}
	if !validPassword(next) {
		return ErrWeakPassword
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.st.SetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	if err := s.st.DeleteUserTokens(ctx, u.ID, kindReset); err != nil {
		return err
	}
	var keep []byte
	if keepSession != "" {
		keep = HashToken(keepSession)
	}
	return s.st.DeleteUserSessions(ctx, u.ID, keep)
}

// MaxNameLen is the longest display name accepted, in characters.
const MaxNameLen = 60

// SetName saves the user's display name, trimmed; empty clears it.
func (s *Service) SetName(ctx context.Context, u store.User, name string) (string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxNameLen || !utf8.ValidString(name) {
		return "", ErrInvalidName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", ErrInvalidName
		}
	}
	return name, s.st.SetName(ctx, u.ID, name)
}

// DeleteAccount removes the user with their sessions and tokens.
func (s *Service) DeleteAccount(ctx context.Context, u store.User, password string) error {
	if err := s.checkCurrent(ctx, u.ID, password); err != nil {
		return err
	}
	return s.st.DeleteUser(ctx, u.ID)
}

// Cleanup removes expired sessions and tokens and unconfirmed accounts older
// than 7 days.
func (s *Service) Cleanup(ctx context.Context) error {
	now := s.o.Now()
	n, err := s.st.PurgeAuth(ctx, now, now.Add(-unconfirmedTTL))
	if err != nil {
		return err
	}
	if n > 0 {
		s.o.Log.Info("auth cleanup", "deleted", n)
	}
	return nil
}

// Prune drops rate-limit entries that have aged out of their windows, so the
// limiters' memory stays bounded. Call it periodically.
func (s *Service) Prune() {
	for _, l := range []*Limiter{s.emailLimit, s.ipLimit, s.signInLimit, s.signInIPLimit, s.currentLimit} {
		l.Prune()
	}
}

// checkCurrent verifies password against the user's stored hash, read fresh
// so a password changed in another session is respected. Attempts are limited
// per user like sign-in: 5 per 15 minutes, reset on success.
func (s *Service) checkCurrent(ctx context.Context, userID int64, password string) error {
	key := strconv.FormatInt(userID, 10)
	if !s.currentLimit.Allow(key) {
		return ErrRateLimited
	}
	u, err := s.st.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return ErrWrongPassword
	}
	s.currentLimit.Reset(key)
	return nil
}

func (s *Service) newSession(ctx context.Context, userID int64) (string, error) {
	raw, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	if err := s.st.CreateAuthSession(ctx, hash, userID, s.o.Now().Add(sessionTTL)); err != nil {
		return "", err
	}
	return raw, nil
}

func (s *Service) useToken(ctx context.Context, raw, kind string) (int64, error) {
	if raw == "" {
		return 0, ErrBadToken
	}
	uid, err := s.st.UseToken(ctx, HashToken(raw), kind, s.o.Now())
	if errors.Is(err, store.ErrNotFound) {
		return 0, ErrBadToken
	}
	return uid, err
}

// newLink creates a one-time token of kind for the user and returns the link
// to path carrying it.
func (s *Service) newLink(ctx context.Context, u store.User, kind, path string, ttl time.Duration) (string, error) {
	raw, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	if err := s.st.CreateToken(ctx, hash, kind, u.ID, s.o.Now().Add(ttl)); err != nil {
		return "", err
	}
	link := s.o.BaseURL + path + "?t=" + url.QueryEscape(raw)
	if s.o.APIToken != "" {
		link += "&token=" + url.QueryEscape(s.o.APIToken)
	}
	return link, nil
}

func (s *Service) sendConfirm(ctx context.Context, u store.User) error {
	link, err := s.newLink(ctx, u, kindConfirm, "/verify", confirmTTL)
	if err != nil {
		return err
	}
	s.send(u.Email, "Confirm your email", link,
		"Confirm your email for Markham Drop-In by opening this link:",
		"The link works once and lasts 24 hours. If you didn't create an account, you can ignore this email.")
	return nil
}

func (s *Service) sendReset(ctx context.Context, u store.User) error {
	link, err := s.newLink(ctx, u, kindReset, "/reset", resetTTL)
	if err != nil {
		return err
	}
	s.send(u.Email, "Reset your password", link,
		"To choose a new password for Markham Drop-In, open this link:",
		"The link works once and lasts 1 hour. If you didn't ask to reset your password, you can ignore this email.")
	return nil
}

func (s *Service) sendTakenNotice(ctx context.Context, u store.User) error {
	link, err := s.newLink(ctx, u, kindReset, "/reset", resetTTL)
	if err != nil {
		return err
	}
	s.send(u.Email, "Someone tried to sign up with your email", link,
		"Someone tried to create a Markham Drop-In account with this email, but you already have one. If you've forgotten your password, you can reset it with this link:",
		"The link works once and lasts 1 hour. If it wasn't you, you can ignore this email; your account hasn't changed.")
	return nil
}

// send emails intro, link and outro in the background. Failures are logged.
func (s *Service) send(to, subject, link, intro, outro string) {
	text := intro + "\n\n" + link + "\n\n" + outro + "\n"
	h := "<p>" + html.EscapeString(intro) + "</p>\n" +
		`<p><a href="` + html.EscapeString(link) + `">` + html.EscapeString(link) + "</a></p>\n" +
		"<p>" + html.EscapeString(outro) + "</p>\n"
	s.o.Async(func() {
		ctx, cancel := context.WithTimeout(context.Background(), mailTimeout)
		defer cancel()
		if err := s.o.Mailer.Send(ctx, to, subject, text, h); err != nil {
			s.o.Log.Error("send email failed", "subject", subject, "err", err)
		}
	})
}

// IsAdmin reports whether u is a confirmed user whose email is in AdminEmails.
func (s *Service) IsAdmin(u store.User) bool {
	if !u.Confirmed {
		return false
	}
	for _, e := range s.o.AdminEmails {
		if e == u.Email {
			return true
		}
	}
	return false
}
