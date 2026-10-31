package remind

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/vBuilt/markham-schedule/internal/auth"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
	"github.com/vBuilt/markham-schedule/internal/web"
)

// HTTPStore is the storage the endpoints need.
type HTTPStore interface {
	SessionByID(ctx context.Context, id string) (schedule.Session, error)
	AddReminder(ctx context.Context, userID int64, sessionID string, residentsAt, now time.Time) (store.Reminder, error)
	DeleteReminder(ctx context.Context, userID int64, sessionID string) (bool, error)
	DeleteReminderByCancelToken(ctx context.Context, hash []byte) (bool, error)
	UserReminders(ctx context.Context, userID int64) ([]store.Reminder, error)
	AllReminders(ctx context.Context) ([]store.Reminder, error)
	UserByID(ctx context.Context, id int64) (store.User, error)
}

var _ HTTPStore = (*store.Store)(nil)

// HTTP serves the reminder endpoints and the cancel page.
type HTTP struct {
	addMu sync.Mutex // serialises the limit check and the insert in add
	st    HTTPStore
	a     *auth.HTTP
	now   func() time.Time
}

// NewHTTP builds the handlers; a nil now means time.Now.
func NewHTTP(st HTTPStore, a *auth.HTTP, now func() time.Time) *HTTP {
	if now == nil {
		now = time.Now
	}
	return &HTTP{st: st, a: a, now: now}
}

// Register adds the reminder routes to mux; wrap is applied to each.
func (h *HTTP) Register(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/reminders", wrap(h.a.RequireConfirmed(h.list)))
	mux.HandleFunc("POST /api/reminders", wrap(h.a.CSRF(h.a.RequireConfirmed(h.add))))
	mux.HandleFunc("POST /api/reminders/delete", wrap(h.a.CSRF(h.a.RequireConfirmed(h.remove))))
	mux.HandleFunc("GET /api/reminders/all", wrap(h.a.RequireConfirmed(h.all)))
	mux.HandleFunc("GET /reminder/cancel", wrap(h.cancelPage))
	mux.HandleFunc("POST /reminder/cancel", wrap(h.a.CSRF(h.cancel)))
}

type reminderJSON struct {
	SessionID   string     `json:"session_id"`
	Name        string     `json:"name"`
	Centre      string     `json:"centre"`
	Start       *time.Time `json:"start"`
	ResidentsAt time.Time  `json:"residents_at"`
	RemindAt    time.Time  `json:"remind_at"`
	BookingURL  string     `json:"booking_url"`
	SentAt      *time.Time `json:"sent_at"`
	SendError   string     `json:"send_error"`
}

// toJSON joins a reminder with its session; a missing session leaves the
// session fields empty.
func (h *HTTP) toJSON(ctx context.Context, x store.Reminder) (reminderJSON, error) {
	s, err := h.st.SessionByID(ctx, x.SessionID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return reminderJSON{}, err
	}
	out := reminderJSON{
		SessionID: x.SessionID, ResidentsAt: x.ResidentsAt, RemindAt: x.ResidentsAt.Add(-Lead),
		SentAt: x.SentAt, SendError: x.SendError,
	}
	if err == nil {
		st := s.Start
		out.Name, out.Centre, out.Start, out.BookingURL = s.Name, s.Centre, &st, s.BookingURL
	}
	return out, nil
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	rs, err := h.st.UserReminders(r.Context(), u.ID)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	out := make([]reminderJSON, 0, len(rs))
	for _, x := range rs {
		j, err := h.toJSON(r.Context(), x)
		if err != nil {
			internalErr(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminders": out})
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
	h.addMu.Lock()
	defer h.addMu.Unlock()
	s, err := h.st.SessionByID(ctx, in.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Session not found.")
		return
	}
	if err != nil {
		internalErr(w, r, err)
		return
	}
	mine, err := h.st.UserReminders(ctx, u.ID)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	pending := 0
	for _, x := range mine {
		if x.SessionID == s.ID {
			h.reply(w, r, http.StatusOK, x)
			return
		}
		if x.SentAt == nil {
			pending++
		}
	}
	if s.Registration == nil || s.Registration.Residents == nil {
		writeErr(w, http.StatusConflict, "Online registration time isn't known for this session.")
		return
	}
	res, now := *s.Registration.Residents, h.now()
	switch {
	case !res.After(now):
		writeErr(w, http.StatusConflict, "Registration has already opened.")
		return
	case res.Sub(now) <= MinAhead:
		writeErr(w, http.StatusConflict, "Registration opens too soon for a reminder.")
		return
	case pending >= MaxPerUser:
		writeErr(w, http.StatusTooManyRequests, "You can have at most 10 reminders at a time.")
		return
	}
	code := http.StatusCreated
	nr, err := h.st.AddReminder(ctx, u.ID, s.ID, res, now)
	if errors.Is(err, store.ErrReminderExists) { // lost a race with ourselves
		code = http.StatusOK
		cur, e := h.st.UserReminders(ctx, u.ID)
		if e != nil {
			internalErr(w, r, e)
			return
		}
		nr = store.Reminder{UserID: u.ID, SessionID: s.ID, ResidentsAt: res}
		for _, x := range cur {
			if x.SessionID == s.ID {
				nr = x
			}
		}
		err = nil
	}
	if err != nil {
		internalErr(w, r, err)
		return
	}
	h.reply(w, r, code, nr)
}

func (h *HTTP) reply(w http.ResponseWriter, r *http.Request, code int, x store.Reminder) {
	j, err := h.toJSON(r.Context(), x)
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
	if _, err := h.st.DeleteReminder(r.Context(), u.ID, in.SessionID); err != nil {
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
	rs, err := h.st.AllReminders(ctx)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	type row struct {
		reminderJSON
		Email string `json:"email"`
	}
	out := make([]row, 0, len(rs))
	for _, x := range rs {
		j, err := h.toJSON(ctx, x)
		if err != nil {
			internalErr(w, r, err)
			return
		}
		usr, err := h.st.UserByID(ctx, x.UserID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			internalErr(w, r, err)
			return
		}
		out = append(out, row{j, usr.Email})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminders": out})
}

// cancelPage only shows the confirmation: link scanners fetch GET links, so a
// GET must never delete.
func (h *HTTP) cancelPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if t := q.Get("t"); t != "" {
		web.ReminderCancel(w, "Cancel this registration reminder?", t, q.Get("token"))
		return
	}
	web.ReminderCancel(w, "This reminder was already cancelled.", "", q.Get("token"))
}

func (h *HTTP) cancel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	msg := "This reminder was already cancelled."
	if t := r.PostFormValue("t"); t != "" {
		ok, err := h.st.DeleteReminderByCancelToken(r.Context(), auth.HashToken(t))
		if err != nil {
			internalErr(w, r, err)
			return
		}
		if ok {
			msg = "Your reminder is cancelled."
		}
	}
	web.ReminderCancel(w, msg, "", r.URL.Query().Get("token"))
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
