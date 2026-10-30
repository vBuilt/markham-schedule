// Package scraper orchestrates refreshes of the PerfectMind schedule into the store.
package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vBuilt/markham-schedule/internal/perfectmind"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
)

// Source is the PerfectMind surface the scraper needs.
type Source interface {
	Calendars(ctx context.Context) ([]perfectmind.Calendar, error)
	Classes(ctx context.Context, cal perfectmind.Calendar, from, to time.Time) ([]perfectmind.RawClass, error)
	AbsURL(path string) string
}

var _ Source = (*perfectmind.Client)(nil)

var ErrBusy = errors.New("refresh already running")

type Config struct {
	Horizon, Retention, Interval time.Duration
	Loc                          *time.Location
	Now                          func() time.Time
}

type Scraper struct {
	src Source
	st  *store.Store
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	onRefresh func() // test hook, called after each Run-initiated refresh
}

func New(src Source, st *store.Store, cfg Config, log *slog.Logger) *Scraper {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Loc == nil {
		loc, err := time.LoadLocation("America/Toronto")
		if err != nil {
			loc = time.UTC
		}
		cfg.Loc = loc
	}
	if log == nil {
		log = slog.Default()
	}
	return &Scraper{src: src, st: st, cfg: cfg, log: log}
}

func (s *Scraper) Refresh(ctx context.Context) (schedule.Status, error) {
	if !s.mu.TryLock() {
		return schedule.Status{}, ErrBusy
	}
	defer s.mu.Unlock()

	now := s.cfg.Now()
	prev, err := s.st.LoadStatus(ctx)
	if err != nil {
		s.log.Warn("load previous status", "err", err)
	}
	status := prev
	status.LastAttempt = &now

	cals, err := s.src.Calendars(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		status.Error = fmt.Sprintf("discover calendars: %v", err)
		if serr := s.st.SaveStatus(ctx, status); serr != nil {
			s.log.Error("save status", "err", serr)
		}
		return status, fmt.Errorf("discover calendars: %w", err)
	}

	ln := now.In(s.cfg.Loc)
	from := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, s.cfg.Loc)
	to := now.Add(s.cfg.Horizon)

	results := make([]schedule.CategoryResult, 0, len(cals))
	names := make([]string, 0, len(cals))
	okCount := 0
	for _, cal := range cals {
		names = append(names, cal.Name)
		res := schedule.CategoryResult{Name: cal.Name}
		raws, err := s.src.Classes(ctx, cal, from, to)
		if err != nil {
			if ctx.Err() != nil {
				return status, ctx.Err()
			}
			res.Error = err.Error()
			s.log.Warn("category fetch failed", "category", cal.Name, "err", err)
			results = append(results, res)
			continue
		}
		bookingURL := s.src.AbsURL(cal.BookingLink)
		sessions := make([]schedule.Session, 0, len(raws))
		var latest time.Time
		for _, r := range raws {
			sess, err := perfectmind.ToSession(r, cal.Name, bookingURL, s.cfg.Loc, now)
			if err != nil {
				s.log.Warn("skipping unconvertible row", "category", cal.Name, "event", r.EventId, "err", err)
				continue
			}
			if sess.Start.After(latest) {
				latest = sess.Start
			}
			sessions = append(sessions, sess)
		}
		if err := s.st.ReplaceCategory(ctx, cal.Name, sessions); err != nil {
			if ctx.Err() != nil {
				return status, ctx.Err()
			}
			res.Error = err.Error()
			s.log.Error("store category", "category", cal.Name, "err", err)
			results = append(results, res)
			continue
		}
		res.OK, res.Count = true, len(sessions)
		okCount++
		latestStr := ""
		if !latest.IsZero() {
			latestStr = latest.In(s.cfg.Loc).Format("2006-01-02")
		}
		s.log.Info("category refreshed", "category", cal.Name, "count", len(sessions), "latest_start", latestStr)
		results = append(results, res)
	}

	if err := s.st.RemoveCategoriesExcept(ctx, names); err != nil {
		s.log.Error("remove vanished categories", "err", err)
	}
	if n, err := s.st.Prune(ctx, now.Add(-s.cfg.Retention)); err != nil {
		s.log.Error("prune", "err", err)
	} else if n > 0 {
		s.log.Info("pruned old sessions", "count", n)
	}

	status.Categories = results
	status.Error = ""
	if all, err := s.st.Sessions(ctx, schedule.Filter{}); err != nil {
		s.log.Error("count sessions", "err", err)
	} else {
		status.Total = len(all)
	}
	var retErr error
	if okCount > 0 {
		status.LastSuccess = &now
	} else if len(cals) > 0 {
		status.Error = "all categories failed"
		retErr = errors.New(status.Error)
	} else {
		// No categories discovered at all: nothing to scrape, treat as success.
		status.LastSuccess = &now
	}
	if err := s.st.SaveStatus(ctx, status); err != nil {
		s.log.Error("save status", "err", err)
		if retErr == nil {
			retErr = fmt.Errorf("save status: %w", err)
		}
	}
	return status, retErr
}

// Run refreshes immediately when data is missing or stale, then every Interval until ctx is done.
func (s *Scraper) Run(ctx context.Context) {
	wait := time.Duration(0)
	if st, err := s.st.LoadStatus(ctx); err == nil && st.LastSuccess != nil {
		if age := s.cfg.Now().Sub(*st.LastSuccess); age < s.cfg.Interval {
			wait = s.cfg.Interval - age
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("refresh failed", "err", err)
		}
		if s.onRefresh != nil {
			s.onRefresh()
		}
		timer.Reset(s.cfg.Interval)
	}
}
