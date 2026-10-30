package watch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vBuilt/markham-schedule/internal/auth"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
	"github.com/vBuilt/markham-schedule/internal/web"
)

// HTTPStore is the storage the endpoints need beyond the checker's Store.
type HTTPStore interface {
	SessionByID(ctx context.Context, id string) (schedule.Session, error)
	AddWatch(ctx context.Context, userID int64, sessionID string, start, now time.Time) (store.Watch, error)
	DeleteWatch(ctx context.Context, userID int64, sessionID string) (bool, error)
	DeleteWatchByStopToken(ctx context.Context, hash []byte) (bool, error)
	UserWatches(ctx context.Context, userID int64) ([]store.Watch, error)
	AllWatches(ctx context.Context) ([]store.Watch, error)
	UserByID(ctx context.Context, id int64) (store.User, error)
}

var _ HTTPStore = (*store.Store)(nil)

// HTTP serves the watch endpoints and the stop page.
type HTTP struct {
	c  *Checker
	st HTTPStore
	a  *auth.HTTP
}

func NewHTTP(c *Checker, st HTTPStore, a *auth.HTTP) *HTTP {
	return &HTTP{c: c, st: st, a: a}
}

// Register adds the watch routes to mux; wrap is applied to each.
func (h *HTTP) Register(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/watches", wrap(h.a.RequireConfirmed(h.list)))
	mux.HandleFunc("POST /api/watches", wrap(h.a.CSRF(h.a.RequireConfirmed(h.add))))
	mux.HandleFunc("POST /api/watches/delete", wrap(h.a.CSRF(h.a.RequireConfirmed(h.remove))))
	mux.HandleFunc("GET /api/watches/all", wrap(h.a.RequireConfirmed(h.all)))
	mux.HandleFunc("GET /watch/stop", wrap(h.stop))
}

type watchJSON struct {
	SessionID   string     `json:"session_id"`
	Name        string     `json:"name"`
	Centre      string     `json:"centre"`
	Start       time.Time  `json:"start"`
	Spots       string     `json:"spots"`
	SpotsLeft   *int       `json:"spots_left"`
	BookingURL  string     `json:"booking_url"`
	EmailsSent  int        `json:"emails_sent"`
	LastEmailed *time.Time `json:"last_emailed"`
}

// toJSON joins a watch with its session; a missing session leaves the
// session fields empty and keeps the watch's own start time.
func (h *HTTP) toJSON(ctx context.Context, w store.Watch) (watchJSON, schedule.Session, error) {
	s, err := h.st.SessionByID(ctx, w.SessionID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return watchJSON{}, s, err
	}
	out := watchJSON{
		SessionID: w.SessionID, Name: s.Name, Centre: s.Centre, Start: w.SessionStart,
		Spots: s.Spots, SpotsLeft: s.SpotsLeft, BookingURL: s.BookingURL,
		EmailsSent: w.EmailsSent, LastEmailed: w.LastEmailed,
	}
	if err == nil {
		out.Start = s.Start
	}
	return out, s, nil
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	ws, err := h.st.UserWatches(r.Context(), u.ID)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	out := make([]watchJSON, 0, len(ws))
	for _, x := range ws {
		j, _, err := h.toJSON(r.Context(), x)
		if err != nil {
			internalErr(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"watches": out, "summary": h.c.Summary()})
}

func isFull(s schedule.Session) bool {
	return (s.SpotsLeft != nil && *s.SpotsLeft == 0) || strings.EqualFold(strings.TrimSpace(s.Spots), "full")
}

func (h *HTTP) add(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := auth.UserFrom(ctx)
	var in struct {
		SessionID string `json:"session_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.SessionID == "" {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	s, err := h.st.SessionByID(ctx, in.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Session not found.")
		return
	}
	if err != nil {
		internalErr(w, r, err)
		return
	}
	mine, err := h.st.UserWatches(ctx, u.ID)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	for _, x := range mine {
		if x.SessionID == s.ID {
			j, _, err := h.toJSON(ctx, x)
			if err != nil {
				internalErr(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, j)
			return
		}
	}
	now := h.c.cfg.Now()
	switch {
	case !s.Start.After(now):
		writeErr(w, http.StatusConflict, "This session has already started.")
		return
	case !isFull(s):
		writeErr(w, http.StatusConflict, "This session isn't full.")
		return
	case len(mine) >= MaxPerUser:
		writeErr(w, http.StatusTooManyRequests, "You can watch at most 10 sessions at a time.")
		return
	}
	all, err := h.st.AllWatches(ctx)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	groups := map[string]bool{}
	for _, x := range all {
		if ws, err := h.st.SessionByID(ctx, x.SessionID); err == nil {
			groups[GroupKey(ws)] = true
		} else if !errors.Is(err, store.ErrNotFound) {
			internalErr(w, r, err)
			return
		}
	}
	if !groups[GroupKey(s)] && len(groups) >= MaxGroups {
		writeErr(w, http.StatusServiceUnavailable, "Spot watching is at capacity right now. Try again later.")
		return
	}
	code := http.StatusCreated
	nw, err := h.st.AddWatch(ctx, u.ID, s.ID, s.Start, now)
	if errors.Is(err, store.ErrWatchExists) { // lost a race with ourselves
		code = http.StatusOK
		err = nil
		if cur, e := h.st.UserWatches(ctx, u.ID); e == nil {
			for _, x := range cur {
				if x.SessionID == s.ID {
					nw = x
				}
			}
		}
	}
	if err != nil {
		internalErr(w, r, err)
		return
	}
	j, _, err := h.toJSON(ctx, nw)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	writeJSON(w, code, j)
}

func (h *HTTP) remove(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	var in struct {
		SessionID string `json:"session_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.SessionID == "" {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if _, err := h.st.DeleteWatch(r.Context(), u.ID, in.SessionID); err != nil {
		internalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) all(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := auth.UserFrom(ctx)
	if !h.a.IsAdmin(u) {
		writeErr(w, http.StatusForbidden, "Admins only.")
		return
	}
	ws, err := h.st.AllWatches(ctx)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	type row struct {
		Name        string    `json:"name"`
		Email       string    `json:"email"`
		SessionID   string    `json:"session_id"`
		SessionName string    `json:"session_name"`
		Start       time.Time `json:"start"`
		SpotsLeft   *int      `json:"spots_left"`
		EmailsSent  int       `json:"emails_sent"`
	}
	out := make([]row, 0, len(ws))
	for _, x := range ws {
		j, _, err := h.toJSON(ctx, x)
		if err != nil {
			internalErr(w, r, err)
			return
		}
		usr, err := h.st.UserByID(ctx, x.UserID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			internalErr(w, r, err)
			return
		}
		out = append(out, row{usr.Name, usr.Email, x.SessionID, j.Name, j.Start, j.SpotsLeft, x.EmailsSent})
	}
	writeJSON(w, http.StatusOK, map[string]any{"watches": out})
}

func (h *HTTP) stop(w http.ResponseWriter, r *http.Request) {
	msg := "This watch was already stopped."
	if t := r.URL.Query().Get("t"); t != "" {
		ok, err := h.st.DeleteWatchByStopToken(r.Context(), auth.HashToken(t))
		if err != nil {
			internalErr(w, r, err)
			return
		}
		if ok {
			msg = "You've stopped watching this session."
		}
	}
	web.WatchStop(w, msg, r.URL.Query().Get("token"))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
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
