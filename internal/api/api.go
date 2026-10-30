// Package api implements the HTTP interface: the .ics feed and the JSON API.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/vBuilt/markham-schedule/internal/ics"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
	"github.com/vBuilt/markham-schedule/internal/web"
)

// Store is the read-side of the data store used by the handlers.
type Store interface {
	Sessions(ctx context.Context, f schedule.Filter) ([]schedule.Session, error)
	Categories(ctx context.Context) ([]store.CategoryCount, error)
	Centres(ctx context.Context) ([]store.CentreCount, error)
	Activities(ctx context.Context) ([]store.ActivityCount, error)
	LoadStatus(ctx context.Context) (schedule.Status, error)
}

var _ Store = (*store.Store)(nil)

type Options struct {
	Token, CalName, Version string
	Refresh, Retention      time.Duration
	FeedDays                int // default 60
	Loc                     *time.Location
	Now                     func() time.Time
}

type server struct {
	st Store
	o  Options
}

// New returns the HTTP handler serving all routes.
func New(st Store, o Options) http.Handler {
	if o.FeedDays == 0 {
		o.FeedDays = 60
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Loc == nil {
		loc, err := time.LoadLocation("America/Toronto")
		if err != nil {
			loc = time.UTC
		}
		o.Loc = loc
	}
	s := &server{st: st, o: o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /{$}", s.auth(web.Index))
	mux.HandleFunc("GET /calendar.ics", s.auth(s.calendar))
	mux.HandleFunc("GET /api/sessions", s.auth(s.sessions))
	mux.HandleFunc("GET /api/categories", s.auth(s.categories))
	mux.HandleFunc("GET /api/centres", s.auth(s.centres))
	mux.HandleFunc("GET /api/activities", s.auth(s.activities))
	mux.HandleFunc("GET /api/status", s.auth(s.status))
	return mux
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.o.Token != "" {
			got := r.URL.Query().Get("token")
			if got == "" {
				if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
					got = strings.TrimPrefix(h, "Bearer ")
				}
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.o.Token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json", "err", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func internalErr(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "path", r.URL.Path, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}

func nonEmpty(vs []string) []string {
	var out []string
	for _, v := range vs {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func baseFilter(r *http.Request) schedule.Filter {
	q := r.URL.Query()
	return schedule.Filter{
		Categories: nonEmpty(q["category"]),
		Centres:    nonEmpty(q["centre"]),
		Activities: nonEmpty(q["activity"]),
		Query:      q.Get("q"),
	}
}

func (s *server) sessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := baseFilter(r)
	now := s.o.Now().In(s.o.Loc)
	f.From = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.o.Loc)
	if v := q.Get("from"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, s.o.Loc)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid from date, want YYYY-MM-DD")
			return
		}
		f.From = d
	}
	if v := q.Get("to"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, s.o.Loc)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid to date, want YYYY-MM-DD")
			return
		}
		f.To = time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, s.o.Loc)
	}
	f.Limit = 500
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "invalid limit")
			return
		}
		f.Limit = min(n, 5000)
	}
	sessions, err := s.st.Sessions(r.Context(), f)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	st, err := s.st.LoadStatus(r.Context())
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if sessions == nil {
		sessions = []schedule.Session{}
	}
	writeJSON(w, http.StatusOK, struct {
		UpdatedAt *time.Time         `json:"updated_at"`
		Count     int                `json:"count"`
		Sessions  []schedule.Session `json:"sessions"`
	}{st.LastSuccess, len(sessions), sessions})
}

func (s *server) categories(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.Categories(r.Context())
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if v == nil {
		v = []store.CategoryCount{}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) activities(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.Activities(r.Context())
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if v == nil {
		v = []store.ActivityCount{}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) centres(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.Centres(r.Context())
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if v == nil {
		v = []store.CentreCount{}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.LoadStatus(r.Context())
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if st.Categories == nil {
		st.Categories = []schedule.CategoryResult{}
	}
	writeJSON(w, http.StatusOK, struct {
		schedule.Status
		Version   string     `json:"version"`
		UpdatedAt *time.Time `json:"updated_at"`
	}{st, s.o.Version, st.LastSuccess})
}

func (s *server) calendar(w http.ResponseWriter, r *http.Request) {
	days := s.o.FeedDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			writeErr(w, http.StatusBadRequest, "days must be an integer between 1 and 365")
			return
		}
		days = n
	}
	now := s.o.Now()
	f := baseFilter(r)
	f.From = now.Add(-s.o.Retention)
	f.To = now.Add(time.Duration(days) * 24 * time.Hour)
	sessions, err := s.st.Sessions(r.Context(), f)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	if err := ics.Write(w, sessions, ics.Options{Name: s.o.CalName, Refresh: s.o.Refresh, Now: now}); err != nil {
		slog.Error("write ics", "err", err)
	}
}
