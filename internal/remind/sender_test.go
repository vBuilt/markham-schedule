package remind

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
)

var toronto = func() *time.Location {
	l, err := time.LoadLocation("America/Toronto")
	if err != nil {
		panic(err)
	}
	return l
}()

// Residents opens Fri Oct 30 2026 17:30; the session starts Sat Oct 31 14:30.
var (
	resAt = time.Date(2026, 10, 30, 17, 30, 0, 0, toronto)
	start = time.Date(2026, 10, 31, 14, 30, 0, 0, toronto)
)

type fakeStore struct {
	mu       sync.Mutex
	rems     map[int64]*store.Reminder
	sessions map[string]schedule.Session
	users    map[int64]store.User
	tokens   int
	tokenErr error
	purged   time.Time
}

func (f *fakeStore) PendingReminders(context.Context) ([]store.Reminder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Reminder
	for _, r := range f.rems {
		if r.SentAt == nil {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (f *fakeStore) SetReminderTime(_ context.Context, id int64, t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rems[id].ResidentsAt = t
	return nil
}
func (f *fakeStore) MarkReminderSent(_ context.Context, id int64, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rems[id]
	if r == nil || r.SentAt != nil {
		return false, nil
	}
	r.SentAt = &now
	return true, nil
}
func (f *fakeStore) SetReminderError(_ context.Context, id int64, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rems[id].SendError = msg
	return nil
}
func (f *fakeStore) DropReminder(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rems, id)
	return nil
}
func (f *fakeStore) AddReminderCancelToken(context.Context, int64, []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenErr != nil {
		return f.tokenErr
	}
	f.tokens++
	return nil
}
func (f *fakeStore) PurgeReminders(_ context.Context, t time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged = t
	return 0, nil
}
func (f *fakeStore) SessionByID(_ context.Context, id string) (schedule.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return schedule.Session{}, store.ErrNotFound
	}
	return s, nil
}
func (f *fakeStore) UserByID(_ context.Context, id int64) (store.User, error) {
	u, ok := f.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

type sentMail struct{ to, subject, text, html string }

type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
	err  error
}

func (m *fakeMailer) Send(_ context.Context, to, subject, text, html string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{to, subject, text, html})
	return m.err
}

type env struct {
	t   *testing.T
	st  *fakeStore
	m   *fakeMailer
	s   *Sender
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, m: &fakeMailer{}}
	e.st = &fakeStore{
		rems:     map[int64]*store.Reminder{1: {ID: 1, UserID: 7, SessionID: "s1", ResidentsAt: resAt}},
		sessions: map[string]schedule.Session{"s1": session(resAt)},
		users:    map[int64]store.User{7: {ID: 7, Email: "u@example.com"}},
	}
	e.s = New(e.st, e.m, Config{
		Loc: toronto, BaseURL: "https://x.test", Now: func() time.Time { return e.now },
		Async: func(f func()) { f() }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return e
}

func session(res time.Time) schedule.Session {
	return schedule.Session{
		ID: "s1", Name: "Drop-In Badminton: All Ages", Centre: "Centennial CC",
		Start: start, End: start.Add(90 * time.Minute), BookingURL: "https://book.test/s1",
		Registration: &schedule.Registration{Residents: &res},
	}
}

func (e *env) tick(h, m, s int) {
	e.now = time.Date(2026, 10, 30, h, m, s, 0, toronto)
	e.s.Tick(context.Background())
}

func (e *env) pending() bool { return e.st.rems[1] != nil && e.st.rems[1].SentAt == nil }

func TestTickNotBeforeLead(t *testing.T) {
	e := newEnv(t)
	e.tick(17, 27, 59)
	if len(e.m.sent) != 0 || !e.pending() {
		t.Fatalf("sent=%d pending=%v", len(e.m.sent), e.pending())
	}
}

func TestTickSendsAtLead(t *testing.T) {
	e := newEnv(t)
	e.tick(17, 28, 0)
	if len(e.m.sent) != 1 {
		t.Fatalf("sent=%d", len(e.m.sent))
	}
	m := e.m.sent[0]
	if m.to != "u@example.com" {
		t.Errorf("to=%q", m.to)
	}
	if want := "Registration opens in 2 minutes: Drop-In Badminton: All Ages, Sat Oct 31, 2:30 p.m."; m.subject != want {
		t.Errorf("subject=%q", m.subject)
	}
	for _, w := range []string{"https://book.test/s1", "https://x.test/reminder/cancel?t="} {
		if !strings.Contains(m.text, w) {
			t.Errorf("text missing %q:\n%s", w, m.text)
		}
	}
	if e.st.rems[1].SentAt == nil || e.st.tokens != 1 {
		t.Errorf("sent=%v tokens=%d", e.st.rems[1].SentAt, e.st.tokens)
	}
	if e.st.purged.IsZero() {
		t.Error("no purge")
	}
}

func TestTickSendsOnce(t *testing.T) {
	e := newEnv(t)
	e.tick(17, 28, 10)
	e.tick(17, 28, 10)
	if len(e.m.sent) != 1 {
		t.Fatalf("sent=%d", len(e.m.sent))
	}
}

func TestTickDropsAfterOpen(t *testing.T) {
	e := newEnv(t)
	e.tick(17, 30, 0)
	if len(e.m.sent) != 0 || e.st.rems[1] != nil {
		t.Fatalf("sent=%d rem=%v", len(e.m.sent), e.st.rems[1])
	}
}

func TestTickFollowsMovedTime(t *testing.T) {
	e := newEnv(t)
	later := time.Date(2026, 10, 30, 18, 0, 0, 0, toronto)
	e.st.sessions["s1"] = session(later)
	e.tick(17, 28, 0)
	if len(e.m.sent) != 0 || !e.st.rems[1].ResidentsAt.Equal(later) {
		t.Fatalf("sent=%d at=%v", len(e.m.sent), e.st.rems[1].ResidentsAt)
	}
	e.tick(17, 58, 0)
	if len(e.m.sent) != 1 {
		t.Fatalf("sent=%d", len(e.m.sent))
	}

	e = newEnv(t)
	e.st.sessions["s1"] = session(time.Date(2026, 10, 30, 17, 0, 0, 0, toronto))
	e.tick(17, 28, 0)
	if len(e.m.sent) != 0 || e.st.rems[1] != nil {
		t.Fatalf("earlier: sent=%d rem=%v", len(e.m.sent), e.st.rems[1])
	}

	// Moved earlier into the window: send now.
	e = newEnv(t)
	e.st.sessions["s1"] = session(time.Date(2026, 10, 30, 17, 29, 0, 0, toronto))
	e.tick(17, 28, 0)
	if len(e.m.sent) != 1 {
		t.Fatalf("into window: sent=%d", len(e.m.sent))
	}
}

func TestTickMissingSession(t *testing.T) {
	e := newEnv(t)
	saved := e.st.sessions["s1"]
	delete(e.st.sessions, "s1")
	e.tick(17, 28, 0)
	if len(e.m.sent) != 0 || !e.pending() {
		t.Fatalf("sent=%d pending=%v", len(e.m.sent), e.pending())
	}
	e.st.sessions["s1"] = saved
	e.tick(17, 29, 0)
	if len(e.m.sent) != 1 {
		t.Fatalf("restored: sent=%d", len(e.m.sent))
	}

	e = newEnv(t)
	delete(e.st.sessions, "s1")
	e.tick(17, 30, 0)
	if len(e.m.sent) != 0 || e.st.rems[1] != nil {
		t.Fatalf("dropped: sent=%d rem=%v", len(e.m.sent), e.st.rems[1])
	}
}

func TestTickSendErrorRecorded(t *testing.T) {
	e := newEnv(t)
	e.m.err = errors.New("smtp down")
	e.tick(17, 28, 0)
	r := e.st.rems[1]
	if r.SendError == "" || r.SentAt == nil {
		t.Fatalf("err=%q sent=%v", r.SendError, r.SentAt)
	}
	e.tick(17, 28, 15)
	if len(e.m.sent) != 1 {
		t.Fatalf("retried: sent=%d", len(e.m.sent))
	}
}

func TestEmailToronto(t *testing.T) {
	e := newEnv(t)
	s := session(resAt)
	s.Start, s.End = s.Start.UTC(), s.End.UTC()
	res := resAt.UTC()
	s.Registration = &schedule.Registration{Residents: &res}
	subject, text, h := e.s.email(s, res, "https://x.test/reminder/cancel?t=abc")
	if !strings.Contains(subject, "2:30 p.m.") {
		t.Errorf("subject=%q", subject)
	}
	for _, body := range []string{text, h} {
		if !strings.Contains(body, "5:30 p.m.") || !strings.Contains(body, "2:30 p.m.") {
			t.Errorf("body not Toronto time:\n%s", body)
		}
	}
}

func TestEmailNoBookingURL(t *testing.T) {
	e := newEnv(t)
	s := session(resAt)
	s.BookingURL = ""
	_, text, h := e.s.email(s, resAt, "https://x.test/c")
	for _, body := range []string{text, h} {
		if !strings.Contains(body, "https://cityofmarkham.perfectmind.com") || strings.Contains(body, "Book it") {
			t.Errorf("body:\n%s", body)
		}
	}
}

func TestTickTokenFailureRecorded(t *testing.T) {
	e := newEnv(t)
	e.st.tokenErr = errors.New("disk full")
	e.tick(17, 28, 0)
	r := e.st.rems[1]
	if r.SentAt == nil || r.SendError == "" || len(e.m.sent) != 0 {
		t.Fatalf("sent=%v err=%q mails=%d", r.SentAt, r.SendError, len(e.m.sent))
	}
}

func TestTickMissingUserRecorded(t *testing.T) {
	e := newEnv(t)
	delete(e.st.users, 7)
	e.tick(17, 28, 0)
	r := e.st.rems[1]
	if r.SentAt == nil || r.SendError == "" || len(e.m.sent) != 0 {
		t.Fatalf("sent=%v err=%q mails=%d", r.SentAt, r.SendError, len(e.m.sent))
	}
}

type slowMailer struct {
	release chan struct{}
	done    atomic.Bool
}

func (m *slowMailer) Send(context.Context, string, string, string, string) error {
	<-m.release
	m.done.Store(true)
	return nil
}

func TestRunJoinsInFlightSends(t *testing.T) {
	e := newEnv(t)
	sm := &slowMailer{release: make(chan struct{})}
	e.now = time.Date(2026, 10, 30, 17, 28, 0, 0, toronto)
	s := New(e.st, sm, Config{Loc: toronto, BaseURL: "https://x.test",
		Now: func() time.Time { return e.now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() { s.Run(ctx); close(returned) }()
	for i := 0; ; i++ {
		e.st.mu.Lock()
		sent := e.st.rems[1].SentAt != nil
		e.st.mu.Unlock()
		if sent {
			break
		}
		if i > 500 {
			t.Fatal("never claimed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-returned:
		t.Fatal("Run returned with a send in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(sm.release)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	if !sm.done.Load() {
		t.Fatal("send did not finish")
	}
}
