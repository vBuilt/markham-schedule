package watch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/auth"
	"github.com/vBuilt/markham-schedule/internal/perfectmind"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
)

var toronto = mustLoc()

func mustLoc() *time.Location {
	l, err := time.LoadLocation("America/Toronto")
	if err != nil {
		panic(err)
	}
	return l
}

// Friday Oct 30 2026, noon in Toronto.
var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, toronto)

type fakeSource struct {
	mu        sync.Mutex
	cals      []perfectmind.Calendar
	rows      map[string][]perfectmind.RawClass
	errs      map[string]error
	calCalls  int
	clsCalls  int
	fromTo    [][2]time.Time
	calsError error
}

func (f *fakeSource) Calendars(context.Context) ([]perfectmind.Calendar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calCalls++
	return f.cals, f.calsError
}

func (f *fakeSource) Classes(_ context.Context, cal perfectmind.Calendar, from, to time.Time) ([]perfectmind.RawClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clsCalls++
	f.fromTo = append(f.fromTo, [2]time.Time{from, to})
	if err := f.errs[cal.Name]; err != nil {
		return nil, err
	}
	return f.rows[cal.Name], nil
}

type sent struct{ to, subject, text, html string }

type fakeMailer struct {
	mu   sync.Mutex
	sent []sent
}

func (m *fakeMailer) Send(_ context.Context, to, subject, text, html string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sent{to, subject, text, html})
	return nil
}

type env struct {
	t   *testing.T
	st  *store.Store
	src *fakeSource
	m   *fakeMailer
	c   *Checker
	now time.Time
	raw perfectmind.RawClass // the watched session's row
	id  string
	uid int64
}

func rawAt(event, day, start string) perfectmind.RawClass {
	return perfectmind.RawClass{
		EventId: event, EventName: "Badminton - Adult", OccurrenceDate: day,
		FormattedStartTime: start, DurationInMinutes: 60, Facility: "Gym",
		Address: perfectmind.RawAddress{AddressTag: "Angus Glen Community Centre"}, Spots: "Full",
	}
}

// storeSession stores raw as a session under each category.
func (e *env) storeSession(raw perfectmind.RawClass, cats ...string) schedule.Session {
	e.t.Helper()
	var s schedule.Session
	for _, cat := range cats {
		var err error
		s, err = perfectmind.ToSession(raw, cat, "https://book.example/"+raw.EventId+"?a=1&b=2", toronto, e.now)
		if err != nil {
			e.t.Fatal(err)
		}
		if err := e.st.ReplaceCategory(context.Background(), cat, e.now, []schedule.Session{s}); err != nil {
			e.t.Fatal(err)
		}
	}
	return s
}

func (e *env) watch(uid int64, s schedule.Session) {
	e.t.Helper()
	if _, err := e.st.AddWatch(context.Background(), uid, s.ID, s.Start, e.now); err != nil {
		e.t.Fatal(err)
	}
}

// newEnv seeds one full session under the given categories (default "A") for
// tomorrow 19:00 and one confirmed user watching it.
func newEnv(t *testing.T, cats ...string) *env {
	t.Helper()
	if len(cats) == 0 {
		cats = []string{"A"}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, m: &fakeMailer{}, now: t0,
		src: &fakeSource{rows: map[string][]perfectmind.RawClass{}, errs: map[string]error{}}}
	for _, n := range []string{"A", "B"} {
		e.src.cals = append(e.src.cals, perfectmind.Calendar{ID: "id-" + n, Name: n})
	}
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "watcher@example.com", "x", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	e.uid = u.ID
	e.raw = rawAt("e1", "20261031", "07:00 PM")
	s := e.storeSession(e.raw, cats...)
	e.id = s.ID
	e.watch(u.ID, s)
	e.c = New(e.src, st, e.m, Config{
		Interval: time.Minute, Loc: toronto, BaseURL: "https://site.example", APIToken: "api<tok>",
		Now:   func() time.Time { return e.now },
		Async: func(f func()) { f() },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return e
}

// serve makes category cat return the watched row with the given spots, plus
// another session's row.
func (e *env) serve(cat, spots string) {
	r := e.raw
	r.Spots = spots
	e.src.rows[cat] = []perfectmind.RawClass{rawAt("other", "20261031", "09:00 AM"), r}
}

func (e *env) cycle() {
	e.c.Cycle(context.Background())
	e.now = e.now.Add(time.Minute)
}

func (e *env) state() store.WatchState {
	e.t.Helper()
	sts, err := e.st.WatchStates(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return sts[e.id]
}

func (e *env) watches() []store.Watch {
	e.t.Helper()
	ws, err := e.st.AllWatches(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return ws
}

func (e *env) emails() int {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	return len(e.m.sent)
}

func TestCycleEmailsOnOpen(t *testing.T) {
	e := newEnv(t)
	e.serve("A", "1 spot")
	e.cycle()
	if e.emails() != 1 {
		t.Fatalf("emails = %d, want 1", e.emails())
	}
	m := e.m.sent[0]
	if m.to != "watcher@example.com" {
		t.Errorf("to = %q", m.to)
	}
	if want := "A spot opened: Badminton - Adult, Sat Oct 31, 7:00 p.m."; m.subject != want {
		t.Errorf("subject = %q, want %q", m.subject, want)
	}
	for _, s := range []string{"Angus Glen Community Centre", "Saturday, October 31, 7:00 p.m. – 8:00 p.m.",
		"1 spot left", "https://book.example/e1?a=1&b=2", "https://site.example/watch/stop?t=",
		"&token=api%3Ctok%3E", "until the session starts"} {
		if !strings.Contains(m.text, s) {
			t.Errorf("text missing %q:\n%s", s, m.text)
		}
	}
	if !strings.Contains(m.html, "https://book.example/e1?a=1&amp;b=2") || strings.Contains(m.html, "a=1&b=2") {
		t.Errorf("html not escaped:\n%s", m.html)
	}
	ws := e.watches()
	if len(ws) != 1 || ws[0].EmailsSent != 1 || ws[0].LastEmailed == nil {
		t.Fatalf("watches = %+v", ws)
	}
	s, err := e.st.SessionByID(context.Background(), e.id)
	if err != nil {
		t.Fatal(err)
	}
	if s.SpotsLeft == nil || *s.SpotsLeft != 1 || s.Spots != "1 spot" {
		t.Fatalf("stored spots = %q %v", s.Spots, s.SpotsLeft)
	}
	if st := e.state(); st.LastSpots != 1 || st.Misses != 0 || st.LastError != "" || st.LastChecked == nil {
		t.Fatalf("state = %+v", st)
	}
	// The stop link's token works.
	var raw string
	for _, line := range strings.Split(m.text, "\n") {
		if i := strings.Index(line, "/watch/stop?t="); i >= 0 {
			raw = strings.SplitN(line[i+len("/watch/stop?t="):], "&", 2)[0]
		}
	}
	if raw == "" {
		t.Fatal("no stop token")
	}
	sum := auth.HashToken(raw)
	if ok, err := e.st.DeleteWatchByStopToken(context.Background(), sum); err != nil || !ok {
		t.Fatalf("stop token = %v, %v", ok, err)
	}
	sum2 := e.c.Summary()
	if sum2.LastCycle == nil || sum2.Sessions != 1 || sum2.People != 1 {
		t.Fatalf("summary = %+v", sum2)
	}
}

func TestCycleNoEmailWhileOpen(t *testing.T) {
	e := newEnv(t)
	e.serve("A", "2 spots")
	e.cycle()
	e.serve("A", "1 spot")
	e.cycle()
	if e.emails() != 1 {
		t.Fatalf("emails = %d, want 1", e.emails())
	}
}

func TestCycleEmailsOnEachReopen(t *testing.T) {
	e := newEnv(t)
	for _, s := range []string{"Full", "1 spot", "Full", "3 spots"} {
		e.serve("A", s)
		e.cycle()
	}
	if e.emails() != 2 {
		t.Fatalf("emails = %d, want 2", e.emails())
	}
	if ws := e.watches(); ws[0].EmailsSent != 2 {
		t.Fatalf("EmailsSent = %d", ws[0].EmailsSent)
	}
}

func TestCycleNilSpotsKeepsState(t *testing.T) {
	e := newEnv(t)
	e.serve("A", "Full")
	e.cycle()
	e.serve("A", "Waitlist")
	e.cycle()
	if e.emails() != 0 {
		t.Fatalf("emails = %d", e.emails())
	}
	if st := e.state(); st.LastSpots != 0 || st.Misses != 0 {
		t.Fatalf("state = %+v", st)
	}
	s, _ := e.st.SessionByID(context.Background(), e.id)
	if s.Spots != "Waitlist" || s.SpotsLeft != nil {
		t.Fatalf("stored spots = %q %v", s.Spots, s.SpotsLeft)
	}
}

func TestCycleErrorNoChange(t *testing.T) {
	e := newEnv(t)
	e.src.errs["A"] = errors.New("boom")
	e.cycle()
	st := e.state()
	if st.Misses != 0 || st.LastError == "" || st.LastSpots != 0 || st.LastChecked == nil {
		t.Fatalf("after error state = %+v", st)
	}
	delete(e.src.errs, "A")
	e.src.rows["A"] = nil // empty day
	for i := 0; i < MissLimit; i++ {
		e.cycle()
	}
	st = e.state()
	if st.Misses != 0 || st.LastError == "" {
		t.Fatalf("after empty state = %+v", st)
	}
	if len(e.watches()) != 1 || e.emails() != 0 {
		t.Fatalf("watches = %d, emails = %d", len(e.watches()), e.emails())
	}
}

func TestCycleMissesCancel(t *testing.T) {
	e := newEnv(t)
	e.src.rows["A"] = []perfectmind.RawClass{rawAt("other", "20261031", "09:00 AM")}
	for i := 1; i < MissLimit; i++ {
		e.cycle()
		if st := e.state(); st.Misses != i || st.LastError == "" {
			t.Fatalf("cycle %d state = %+v", i, st)
		}
	}
	e.cycle()
	if n := len(e.watches()); n != 0 {
		t.Fatalf("%d watches left", n)
	}
	if sts, _ := e.st.WatchStates(context.Background()); len(sts) != 0 {
		t.Fatalf("states left: %+v", sts)
	}
	if e.emails() != 0 {
		t.Fatalf("emails = %d", e.emails())
	}
}

func TestCycleMissResetsOnFound(t *testing.T) {
	e := newEnv(t)
	e.src.rows["A"] = []perfectmind.RawClass{rawAt("other", "20261031", "09:00 AM")}
	e.cycle()
	e.serve("A", "Full")
	e.cycle()
	if st := e.state(); st.Misses != 0 || st.LastError != "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestCycleTriesNextCategory(t *testing.T) {
	e := newEnv(t, "A", "B")
	e.src.rows["A"] = []perfectmind.RawClass{rawAt("other", "20261031", "09:00 AM")}
	e.serve("B", "1 spot")
	e.cycle()
	if e.emails() != 1 {
		t.Fatalf("emails = %d", e.emails())
	}
	if st := e.state(); st.Misses != 0 || st.LastSpots != 1 {
		t.Fatalf("state = %+v", st)
	}
}

func TestCycleErrorInOneCategoryNoMiss(t *testing.T) {
	e := newEnv(t, "A", "B")
	e.src.rows["A"] = []perfectmind.RawClass{rawAt("other", "20261031", "09:00 AM")}
	e.src.errs["B"] = errors.New("boom")
	e.cycle()
	if st := e.state(); st.Misses != 0 || st.LastError == "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestCycleOneFetchPerGroup(t *testing.T) {
	e := newEnv(t)
	other := rawAt("e2", "20261031", "08:00 PM")
	ctx := context.Background()
	s1, _ := e.st.SessionByID(ctx, e.id)
	s2, err := perfectmind.ToSession(other, "A", "", toronto, e.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.ReplaceCategory(ctx, "A", e.now, []schedule.Session{s1, s2}); err != nil {
		t.Fatal(err)
	}
	e.watch(e.uid, s2)
	e.serve("A", "Full")
	e.cycle()
	if e.src.clsCalls != 1 || e.src.calCalls != 1 {
		t.Fatalf("Classes calls = %d, Calendars calls = %d", e.src.clsCalls, e.src.calCalls)
	}
	ft := e.src.fromTo[0]
	if !ft[0].Equal(time.Date(2026, 10, 31, 0, 0, 0, 0, toronto)) || !ft[1].Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, toronto)) {
		t.Fatalf("from/to = %v", ft)
	}
	e.cycle()
	if e.src.clsCalls != 2 || e.src.calCalls != 1 {
		t.Fatalf("second cycle: Classes calls = %d, Calendars calls = %d", e.src.clsCalls, e.src.calCalls)
	}
	if sum := e.c.Summary(); sum.Sessions != 2 || sum.People != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestCycleUnknownCalendarRediscoverLimited(t *testing.T) {
	e := newEnv(t, "C")
	e.serve("C", "1 spot")
	e.cycle()
	if e.src.calCalls != 1 || e.src.clsCalls != 0 {
		t.Fatalf("calls = %d/%d", e.src.calCalls, e.src.clsCalls)
	}
	if st := e.state(); st.Misses != 0 || st.LastError == "" {
		t.Fatalf("state = %+v", st)
	}
	e.cycle() // within 10 minutes: no re-discovery
	if e.src.calCalls != 1 {
		t.Fatalf("Calendars calls = %d, want 1", e.src.calCalls)
	}
	e.src.cals = append(e.src.cals, perfectmind.Calendar{ID: "id-C", Name: "C"})
	e.now = e.now.Add(10 * time.Minute)
	e.cycle()
	if e.src.calCalls != 2 || e.emails() != 1 {
		t.Fatalf("Calendars calls = %d, emails = %d", e.src.calCalls, e.emails())
	}
}

func TestCyclePurgesStarted(t *testing.T) {
	e := newEnv(t)
	e.serve("A", "1 spot")
	e.now = time.Date(2026, 10, 31, 19, 0, 0, 0, toronto)
	e.cycle()
	if n := len(e.watches()); n != 0 {
		t.Fatalf("%d watches left", n)
	}
	if e.src.clsCalls != 0 || e.src.calCalls != 0 || e.emails() != 0 {
		t.Fatalf("calls = %d/%d, emails = %d", e.src.calCalls, e.src.clsCalls, e.emails())
	}
}

func TestCycleNoWatchesNoRequests(t *testing.T) {
	e := newEnv(t)
	if _, err := e.st.DeleteWatch(context.Background(), e.uid, e.id); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	if e.src.calCalls != 0 || e.src.clsCalls != 0 {
		t.Fatalf("calls = %d/%d", e.src.calCalls, e.src.clsCalls)
	}
	if sum := e.c.Summary(); sum.LastCycle == nil || sum.Sessions != 0 || sum.People != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestCycleSessionMissingFromStore(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// A watch on a session id that isn't stored.
	if _, err := e.st.AddWatch(ctx, e.uid, "gone", e.now.Add(time.Hour), e.now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MissLimit; i++ {
		e.cycle()
	}
	for _, w := range e.watches() {
		if w.SessionID == "gone" {
			t.Fatal("watch on missing session not removed")
		}
	}
}

func TestGroupKey(t *testing.T) {
	s := schedule.Session{Categories: []string{"A", "B"}, Start: time.Date(2026, 10, 31, 23, 30, 0, 0, toronto)}
	if got := GroupKey(s); got != "A|2026-10-31" {
		t.Fatalf("GroupKey = %q", got)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	e := newEnv(t)
	e.c.cfg.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.c.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.c.Summary().LastCycle == nil {
		if time.Now().After(deadline) {
			t.Fatal("no cycle ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// flakyStore wraps the real store with failing methods.
type flakyStore struct {
	*store.Store
	failSave, failPurge bool
}

func (f *flakyStore) SaveWatchState(ctx context.Context, st store.WatchState) error {
	if f.failSave {
		return errors.New("database is locked")
	}
	return f.Store.SaveWatchState(ctx, st)
}

func (f *flakyStore) PurgeWatches(ctx context.Context, now time.Time) (int64, error) {
	if f.failPurge {
		return 0, errors.New("database is locked")
	}
	return f.Store.PurgeWatches(ctx, now)
}

func TestCycleSaveFailureNoDuplicateEmail(t *testing.T) {
	e := newEnv(t)
	fs := &flakyStore{Store: e.st, failSave: true}
	e.c = New(e.src, fs, e.m, e.c.cfg)
	e.serve("A", "1 spot")
	e.cycle()
	if e.emails() != 0 {
		t.Fatalf("emails after failed save = %d, want 0", e.emails())
	}
	fs.failSave = false
	e.cycle()
	e.cycle()
	if e.emails() != 1 {
		t.Fatalf("emails = %d, want 1", e.emails())
	}
}

func TestCycleNoEmailAfterStartWhenPurgeFails(t *testing.T) {
	e := newEnv(t)
	e.c = New(e.src, &flakyStore{Store: e.st, failPurge: true}, e.m, e.c.cfg)
	e.serve("A", "1 spot")
	e.now = time.Date(2026, 10, 31, 19, 0, 0, 0, toronto)
	e.cycle()
	if e.emails() != 0 || e.src.clsCalls != 0 {
		t.Fatalf("emails = %d, Classes calls = %d", e.emails(), e.src.clsCalls)
	}
}
