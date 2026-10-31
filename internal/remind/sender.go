// Package remind emails users shortly before a session's Residents
// registration opens.
package remind

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vBuilt/markham-schedule/internal/auth"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
)

const (
	MaxPerUser = 10 // pending reminders per user
	Lead       = 2 * time.Minute
	TickEvery  = 15 * time.Second
	MinAhead   = Lead + TickEvery // a reminder must be created at least this long before opening
	KeepSent   = 7 * 24 * time.Hour
)

const (
	mailTimeout  = 30 * time.Second
	claimTimeout = 10 * time.Second // store calls after the claim, which must outlive shutdown
	fallbackSite = "https://cityofmarkham.perfectmind.com"
)

// Store is the storage the sender needs.
type Store interface {
	PendingReminders(ctx context.Context) ([]store.Reminder, error)
	SetReminderTime(ctx context.Context, id int64, residentsAt time.Time) error
	MarkReminderSent(ctx context.Context, id int64, now time.Time) (bool, error)
	SetReminderError(ctx context.Context, id int64, msg string) error
	DropReminder(ctx context.Context, id int64) error
	AddReminderCancelToken(ctx context.Context, reminderID int64, hash []byte) error
	PurgeReminders(ctx context.Context, sentBefore time.Time) (int64, error)
	SessionByID(ctx context.Context, id string) (schedule.Session, error)
	UserByID(ctx context.Context, id int64) (store.User, error)
}

var _ Store = (*store.Store)(nil)

type Config struct {
	Loc *time.Location
	// BaseURL and APIToken build the cancel links; APIToken is appended as &token=.
	BaseURL, APIToken string
	Now               func() time.Time
	Async             func(func())
	Log               *slog.Logger
}

type Sender struct {
	st  Store
	m   auth.Mailer
	cfg Config
	wg  sync.WaitGroup // default Async sends in flight
}

func New(st Store, m auth.Mailer, cfg Config) *Sender {
	if cfg.Loc == nil {
		loc, err := time.LoadLocation("America/Toronto")
		if err != nil {
			loc = time.UTC
		}
		cfg.Loc = loc
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Sender{st: st, m: m, cfg: cfg}
	if cfg.Async == nil {
		s.cfg.Async = func(f func()) {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				f()
			}()
		}
	}
	return s
}

// Run ticks now and then every TickEvery until ctx ends.
func (s *Sender) Run(ctx context.Context) {
	t := time.NewTicker(TickEvery)
	defer t.Stop()
	defer s.wg.Wait() // let queued emails finish before the caller closes the store
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick sends every reminder whose registration opens within Lead, and drops
// those whose registration has already opened.
func (s *Sender) Tick(ctx context.Context) {
	log := s.cfg.Log
	now := s.cfg.Now()
	rems, err := s.st.PendingReminders(ctx)
	if err != nil {
		log.Error("list reminders", "err", err)
		return
	}
	for _, r := range rems {
		if ctx.Err() != nil {
			return
		}
		s.handle(ctx, r, now)
	}
	if ctx.Err() != nil {
		return
	}
	if _, err := s.st.PurgeReminders(ctx, now.Add(-KeepSent)); err != nil {
		log.Error("purge reminders", "err", err)
	}
}

func (s *Sender) handle(ctx context.Context, r store.Reminder, now time.Time) {
	log := s.cfg.Log
	res, confirmed := r.ResidentsAt, false
	sess, err := s.st.SessionByID(ctx, r.SessionID)
	switch {
	case err == nil:
		if sess.Registration != nil && sess.Registration.Residents != nil {
			res, confirmed = *sess.Registration.Residents, true
			if !res.Equal(r.ResidentsAt) {
				if err := s.st.SetReminderTime(ctx, r.ID, res); err != nil {
					log.Error("update reminder time", "reminder", r.ID, "err", err)
					return
				}
			}
		}
	case errors.Is(err, store.ErrNotFound):
	default:
		log.Error("load reminder session", "reminder", r.ID, "session", r.SessionID, "err", err)
		return
	}
	if !now.Before(res) {
		if err := s.st.DropReminder(ctx, r.ID); err != nil {
			log.Error("drop reminder", "reminder", r.ID, "err", err)
			return
		}
		log.Info("reminder dropped: registration already open", "reminder", r.ID, "session", r.SessionID)
		return
	}
	if !confirmed || now.Before(res.Add(-Lead)) {
		return
	}
	// Claim before sending: at most one email, and never retried.
	ok, err := s.st.MarkReminderSent(ctx, r.ID, now)
	if err != nil {
		log.Error("mark reminder sent", "reminder", r.ID, "err", err)
		return
	}
	if !ok {
		return
	}
	// The claim is recorded, so finish the job even if ctx ends, and record
	// any failure so the status page doesn't show a silent "Sent".
	cctx, stop := context.WithTimeout(context.WithoutCancel(ctx), claimTimeout)
	defer stop()
	fail := func(reason string, err error) {
		log.Error(reason, "reminder", r.ID, "err", err)
		if err := s.st.SetReminderError(cctx, r.ID, reason); err != nil {
			log.Error("record reminder error", "reminder", r.ID, "err", err)
		}
	}
	u, err := s.st.UserByID(cctx, r.UserID)
	if err != nil {
		fail("user not found", err)
		return
	}
	raw, hash, err := auth.NewToken()
	if err != nil {
		fail("could not create cancel token", err)
		return
	}
	if err := s.st.AddReminderCancelToken(cctx, r.ID, hash); err != nil {
		fail("could not save cancel token", err)
		return
	}
	cancel := s.cfg.BaseURL + "/reminder/cancel?t=" + url.QueryEscape(raw)
	if s.cfg.APIToken != "" {
		cancel += "&token=" + url.QueryEscape(s.cfg.APIToken)
	}
	to := u.Email
	subject, text, h := s.email(sess, res, cancel)
	id := r.ID
	s.cfg.Async(func() {
		mctx, stop := context.WithTimeout(context.Background(), mailTimeout)
		defer stop()
		if err := s.m.Send(mctx, to, subject, text, h); err != nil {
			log.Error("send reminder email failed", "reminder", id, "err", err)
			ectx, estop := context.WithTimeout(context.WithoutCancel(ctx), claimTimeout)
			defer estop()
			if err := s.st.SetReminderError(ectx, id, err.Error()); err != nil {
				log.Error("record reminder error", "reminder", id, "err", err)
			}
		}
	})
	log.Info("reminder email queued", "reminder", id, "session", r.SessionID)
}

// clock formats t like the site: "7:00 p.m.".
func (s *Sender) clock(t time.Time) string {
	r := strings.NewReplacer("AM", "a.m.", "PM", "p.m.")
	return r.Replace(t.In(s.cfg.Loc).Format("3:04 PM"))
}

// email builds the subject and plain-text and HTML bodies.
func (s *Sender) email(sess schedule.Session, res time.Time, cancel string) (subject, text, h string) {
	start := sess.Start.In(s.cfg.Loc)
	subject = fmt.Sprintf("Registration opens in 2 minutes: %s, %s, %s", sess.Name, start.Format("Mon Jan 2"), s.clock(start))
	when := start.Format("Monday, January 2") + ", "
	if sess.AllDay {
		when += "all day"
	} else {
		when += s.clock(start) + " – " + s.clock(sess.End)
	}
	intro := fmt.Sprintf("Online registration for residents opens at %s on %s.",
		s.clock(res), res.In(s.cfg.Loc).Format("Monday, January 2"))
	fallback := "Register at " + fallbackSite

	var tb strings.Builder
	tb.WriteString(intro + "\n\n")
	tb.WriteString(sess.Name + "\n")
	if sess.Centre != "" {
		tb.WriteString(sess.Centre + "\n")
	}
	tb.WriteString(when + "\n\n")
	if sess.BookingURL != "" {
		tb.WriteString("Book it: " + sess.BookingURL + "\n\n")
	} else {
		tb.WriteString(fallback + "\n\n")
	}
	tb.WriteString("Cancel this reminder: " + cancel + "\n")

	e := html.EscapeString
	var hb strings.Builder
	hb.WriteString("<p>" + e(intro) + "</p>\n")
	hb.WriteString("<p><strong>" + e(sess.Name) + "</strong><br>\n")
	if sess.Centre != "" {
		hb.WriteString(e(sess.Centre) + "<br>\n")
	}
	hb.WriteString(e(when) + "</p>\n")
	if sess.BookingURL != "" {
		hb.WriteString(`<p><a href="` + e(sess.BookingURL) + `">Book it</a></p>` + "\n")
	} else {
		hb.WriteString(`<p>Register at <a href="` + fallbackSite + `">` + fallbackSite + "</a></p>\n")
	}
	hb.WriteString(`<p><a href="` + e(cancel) + `">Cancel this reminder</a></p>` + "\n")
	return subject, tb.String(), hb.String()
}
