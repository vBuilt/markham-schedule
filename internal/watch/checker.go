// Package watch emails users when a full session they watch opens a spot.
package watch

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
	"github.com/vBuilt/markham-schedule/internal/perfectmind"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
)

const (
	MaxPerUser = 10 // active watches per user
	MaxGroups  = 20 // distinct (first category, day) groups across all watches
	MissLimit  = 3  // successful checks without the session before its watches end
)

const (
	rediscoverEvery = 10 * time.Minute
	mailTimeout     = 30 * time.Second
	notifyTimeout   = 10 * time.Second
)

// Source is the PerfectMind surface the checker needs.
type Source interface {
	Calendars(ctx context.Context) ([]perfectmind.Calendar, error)
	Classes(ctx context.Context, cal perfectmind.Calendar, from, to time.Time) ([]perfectmind.RawClass, error)
}

var _ Source = (*perfectmind.Client)(nil)

// Store is the storage the checker needs.
type Store interface {
	PurgeWatches(ctx context.Context, now time.Time) (int64, error)
	WatchStates(ctx context.Context) (map[string]store.WatchState, error)
	SaveWatchState(ctx context.Context, st store.WatchState) error
	SessionByID(ctx context.Context, id string) (schedule.Session, error)
	UpdateSessionSpots(ctx context.Context, id, spots string, left *int, at time.Time) error
	DeleteSessionWatches(ctx context.Context, sessionID string) error
	WatchersOf(ctx context.Context, sessionID string) ([]store.Watch, error)
	AllWatches(ctx context.Context) ([]store.Watch, error)
	UserByID(ctx context.Context, id int64) (store.User, error)
	AddStopToken(ctx context.Context, watchID int64, hash []byte) error
	MarkEmailed(ctx context.Context, watchID int64, now time.Time) error
}

var _ Store = (*store.Store)(nil)

type Config struct {
	Interval time.Duration
	Loc      *time.Location
	// BaseURL and APIToken build the stop links; APIToken is appended as &token=.
	BaseURL, APIToken string
	Now               func() time.Time
	Async             func(func())
	Log               *slog.Logger
}

// Summary is the checker line on the status page.
type Summary struct {
	LastCycle *time.Time `json:"last_cycle"`
	Sessions  int        `json:"sessions"`
	People    int        `json:"people"`
}

type Checker struct {
	src Source
	st  Store
	m   auth.Mailer
	cfg Config

	cycleMu      sync.Mutex // one Cycle at a time
	cals         map[string]perfectmind.Calendar
	lastDiscover time.Time

	sumMu sync.Mutex
	sum   Summary
}

func New(src Source, st Store, m auth.Mailer, cfg Config) *Checker {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
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
	if cfg.Async == nil {
		cfg.Async = func(f func()) { go f() }
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Checker{src: src, st: st, m: m, cfg: cfg}
}

// GroupKey is the session's first category and its start day (in the
// session's own location, America/Toronto for stored sessions). Sessions with
// the same key are checked with one request.
func GroupKey(s schedule.Session) string {
	cat := ""
	if len(s.Categories) > 0 {
		cat = s.Categories[0]
	}
	return cat + "|" + s.Start.Format("2006-01-02")
}

// Summary returns the result of the last cycle.
func (c *Checker) Summary() Summary {
	c.sumMu.Lock()
	defer c.sumMu.Unlock()
	return c.sum
}

// Run calls Cycle every Interval until ctx ends. A cycle that overruns makes
// the next one start straight away; cycles never overlap.
func (c *Checker) Run(ctx context.Context) {
	for {
		started := time.Now()
		c.Cycle(ctx)
		wait := c.cfg.Interval - time.Since(started)
		if wait < 0 {
			wait = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

type fetchKey struct{ cat, day string }

type fetchResult struct {
	rows []perfectmind.RawClass
	err  error
}

// cycle holds the per-cycle fetch cache.
type cycle struct {
	c          *Checker
	ctx        context.Context
	now        time.Time
	discovered bool // Calendars was called during this cycle
	fetched    map[fetchKey]fetchResult
}

// Cycle checks every watched session once.
func (c *Checker) Cycle(ctx context.Context) {
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	now := c.cfg.Now()
	log := c.cfg.Log

	if n, err := c.st.PurgeWatches(ctx, now); err != nil {
		log.Error("purge watches", "err", err)
	} else if n > 0 {
		log.Info("purged started watches", "count", n)
	}
	states, err := c.st.WatchStates(ctx)
	if err != nil {
		log.Error("load watch states", "err", err)
		return
	}
	cy := &cycle{c: c, ctx: ctx, now: now, fetched: map[fetchKey]fetchResult{}}
	for id, st := range states {
		if ctx.Err() != nil {
			return
		}
		cy.check(id, st)
	}
	if ctx.Err() != nil {
		return
	}
	c.updateSummary(ctx, now)
}

func (c *Checker) updateSummary(ctx context.Context, now time.Time) {
	ws, err := c.st.AllWatches(ctx)
	if err != nil {
		c.cfg.Log.Error("list watches", "err", err)
		return
	}
	sessions, people := map[string]bool{}, map[int64]bool{}
	for _, w := range ws {
		sessions[w.SessionID] = true
		people[w.UserID] = true
	}
	c.sumMu.Lock()
	c.sum = Summary{LastCycle: &now, Sessions: len(sessions), People: len(people)}
	c.sumMu.Unlock()
}

// check runs one session's check and saves its state.
func (cy *cycle) check(id string, st store.WatchState) {
	c, ctx, log := cy.c, cy.ctx, cy.c.cfg.Log
	st.SessionID = id

	s, err := c.st.SessionByID(ctx, id)
	var opened *schedule.Session // set when this check moves the session from full to open
	switch {
	case errors.Is(err, store.ErrNotFound):
		if cy.miss(&st, "session is no longer in the schedule") {
			return
		}
	case err != nil:
		st.LastError = "load session: " + err.Error()
		log.Error("load watched session", "session", id, "err", err)
	case !s.Start.After(cy.now):
		// Started (PurgeWatches failed or raced): never check or email it.
		return
	default:
		raw, found, failure := cy.find(s)
		switch {
		case found:
			left := perfectmind.ParseSpots(raw.Spots)
			if err := c.st.UpdateSessionSpots(ctx, id, raw.Spots, left, cy.now); err != nil {
				log.Error("update session spots", "session", id, "err", err)
			}
			if left != nil {
				if st.LastSpots == 0 && *left > 0 {
					s.Spots, s.SpotsLeft = raw.Spots, left
					opened = &s
				}
				st.LastSpots = *left
			}
			st.Misses, st.LastError = 0, ""
		case failure != "":
			if ctx.Err() != nil {
				return
			}
			st.LastError = failure
		default:
			if cy.miss(&st, "session missing from PerfectMind response") {
				return
			}
		}
	}
	now := cy.now
	st.LastChecked = &now
	// Save before emailing: if the new open state can't be stored, a later
	// cycle would see last_spots == 0 again and email everyone twice.
	if err := c.st.SaveWatchState(ctx, st); err != nil {
		log.Error("save watch state", "session", id, "err", err)
		return
	}
	if opened != nil {
		c.notify(ctx, *opened, st.LastSpots, now)
	}
}

// miss counts a miss. At MissLimit it ends the session's watches and reports
// true (there is no state left to save).
func (cy *cycle) miss(st *store.WatchState, reason string) bool {
	st.Misses++
	if st.Misses >= MissLimit {
		if err := cy.c.st.DeleteSessionWatches(cy.ctx, st.SessionID); err != nil {
			cy.c.cfg.Log.Error("delete missing session's watches", "session", st.SessionID, "err", err)
		} else {
			cy.c.cfg.Log.Info("session missing; watches ended", "session", st.SessionID)
		}
		return true
	}
	st.LastError = reason
	return false
}

// find looks for the session in each of its categories' day listings in turn.
// failure is non-empty when it wasn't found and at least one category failed
// or returned no rows (so not finding it is no evidence it's gone).
func (cy *cycle) find(s schedule.Session) (raw perfectmind.RawClass, found bool, failure string) {
	if len(s.Categories) == 0 {
		return raw, false, "session has no category"
	}
	day := s.Start.In(cy.c.cfg.Loc)
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, cy.c.cfg.Loc)
	to := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, cy.c.cfg.Loc)
	for _, cat := range s.Categories {
		res := cy.fetch(cat, from, to)
		if res.err != nil {
			failure = fmt.Sprintf("%s: %v", cat, res.err)
			continue
		}
		if len(res.rows) == 0 {
			failure = cat + ": no sessions returned"
			continue
		}
		for _, r := range res.rows {
			if perfectmind.SessionID(r) == s.ID {
				return r, true, ""
			}
		}
	}
	return raw, false, failure
}

// fetch returns the category's rows for the day starting at from, fetching
// each (category, day) at most once per cycle.
func (cy *cycle) fetch(cat string, from, to time.Time) fetchResult {
	key := fetchKey{cat, from.Format("2006-01-02")}
	if res, ok := cy.fetched[key]; ok {
		return res
	}
	var res fetchResult
	cal, err := cy.calendar(cat)
	if err != nil {
		res.err = err
	} else {
		res.rows, res.err = cy.c.src.Classes(cy.ctx, cal, from, to)
		if res.err != nil && cy.ctx.Err() == nil {
			cy.c.cfg.Log.Warn("watch fetch failed", "category", cat, "day", key.day, "err", res.err)
		}
	}
	cy.fetched[key] = res
	return res
}

// calendar resolves a category name to its PerfectMind calendar. Calendars
// are discovered on first need and again for an unknown name at most every
// rediscoverEvery (and at most once per cycle).
func (cy *cycle) calendar(name string) (perfectmind.Calendar, error) {
	c := cy.c
	if cal, ok := c.cals[name]; ok {
		return cal, nil
	}
	if !cy.discovered && (c.cals == nil || cy.now.Sub(c.lastDiscover) >= rediscoverEvery) {
		cy.discovered = true
		c.lastDiscover = cy.now
		cals, err := c.src.Calendars(cy.ctx)
		if err != nil {
			c.cfg.Log.Warn("watch calendar discovery failed", "err", err)
			return perfectmind.Calendar{}, fmt.Errorf("discover calendars: %w", err)
		}
		m := make(map[string]perfectmind.Calendar, len(cals))
		for _, cal := range cals {
			m[cal.Name] = cal
		}
		c.cals = m
		if cal, ok := m[name]; ok {
			return cal, nil
		}
	}
	return perfectmind.Calendar{}, fmt.Errorf("unknown calendar %q", name)
}

// notify emails every watcher of s that a spot opened.
func (c *Checker) notify(ctx context.Context, s schedule.Session, left int, now time.Time) {
	log := c.cfg.Log
	// The opening is already saved; finish queueing its emails even if the
	// cycle is being cancelled (shutdown).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()
	ws, err := c.st.WatchersOf(ctx, s.ID)
	if err != nil {
		log.Error("list watchers", "session", s.ID, "err", err)
		return
	}
	for _, w := range ws {
		u, err := c.st.UserByID(ctx, w.UserID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				log.Error("load watcher", "watch", w.ID, "err", err)
			}
			continue
		}
		raw, hash, err := auth.NewToken()
		if err != nil {
			log.Error("new stop token", "watch", w.ID, "err", err)
			continue
		}
		if err := c.st.AddStopToken(ctx, w.ID, hash); err != nil {
			log.Error("save stop token", "watch", w.ID, "err", err)
			continue
		}
		stop := c.cfg.BaseURL + "/watch/stop?t=" + url.QueryEscape(raw)
		if c.cfg.APIToken != "" {
			stop += "&token=" + url.QueryEscape(c.cfg.APIToken)
		}
		to := u.Email
		subject, text, h := c.email(s, left, now, stop)
		watchID := w.ID
		c.cfg.Async(func() {
			mctx, cancel := context.WithTimeout(context.Background(), mailTimeout)
			defer cancel()
			if err := c.m.Send(mctx, to, subject, text, h); err != nil {
				log.Error("send spot email failed", "watch", watchID, "err", err)
			}
		})
		if err := c.st.MarkEmailed(ctx, w.ID, now); err != nil {
			log.Error("mark emailed", "watch", w.ID, "err", err)
		}
		log.Info("spot email queued", "watch", w.ID, "session", s.ID)
	}
}

// clock formats t like the site: "7:00 p.m.".
func (c *Checker) clock(t time.Time) string {
	r := strings.NewReplacer("AM", "a.m.", "PM", "p.m.")
	return r.Replace(t.In(c.cfg.Loc).Format("3:04 PM"))
}

func spotsLeft(n int) string {
	if n == 1 {
		return "1 spot left"
	}
	return fmt.Sprintf("%d spots left", n)
}

// email builds the subject and plain-text and HTML bodies.
func (c *Checker) email(s schedule.Session, left int, now time.Time, stop string) (subject, text, h string) {
	start := s.Start.In(c.cfg.Loc)
	subject = fmt.Sprintf("A spot opened: %s, %s, %s", s.Name, start.Format("Mon Jan 2"), c.clock(start))
	when := start.Format("Monday, January 2") + ", "
	if s.AllDay {
		when += "all day"
	} else {
		when += c.clock(start) + " – " + c.clock(s.End)
	}
	nowLine := spotsLeft(left) + " (checked " + c.clock(now) + ")"
	const intro = "A spot just opened in a session you're watching."
	const outro = "We'll keep watching and email you again if it fills up and opens again, until the session starts."

	var tb strings.Builder
	tb.WriteString(intro + "\n\n")
	tb.WriteString(s.Name + "\n")
	if s.Centre != "" {
		tb.WriteString(s.Centre + "\n")
	}
	tb.WriteString(when + "\n")
	tb.WriteString("Now: " + nowLine + "\n\n")
	if s.BookingURL != "" {
		tb.WriteString("Book it now: " + s.BookingURL + "\n\n")
	}
	tb.WriteString(outro + "\n\n")
	tb.WriteString("Stop watching: " + stop + "\n")

	e := html.EscapeString
	var hb strings.Builder
	hb.WriteString("<p>" + e(intro) + "</p>\n")
	hb.WriteString("<p><strong>" + e(s.Name) + "</strong><br>\n")
	if s.Centre != "" {
		hb.WriteString(e(s.Centre) + "<br>\n")
	}
	hb.WriteString(e(when) + "<br>\n")
	hb.WriteString("Now: " + e(nowLine) + "</p>\n")
	if s.BookingURL != "" {
		hb.WriteString(`<p><a href="` + e(s.BookingURL) + `">Book it now</a></p>` + "\n")
	}
	hb.WriteString("<p>" + e(outro) + "</p>\n")
	hb.WriteString(`<p><a href="` + e(stop) + `">Stop watching</a></p>` + "\n")
	return subject, tb.String(), hb.String()
}
