package scraper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/perfectmind"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/store"
)

var toronto, _ = time.LoadLocation("America/Toronto")

type fakeSource struct {
	mu      sync.Mutex
	cals    []perfectmind.Calendar
	calsErr error
	classes map[string][]perfectmind.RawClass
	errs    map[string]error
	block   chan struct{} // if set, Calendars waits on it
	entered chan struct{}

	regCalls []string         // "eventID|occurrenceDate" per Registration call
	regErr   map[string]error // by eventID
	regNone  map[string]bool  // by eventID: page has no registration dates
}

func (f *fakeSource) Calendars(ctx context.Context) ([]perfectmind.Calendar, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cals, f.calsErr
}

func (f *fakeSource) Classes(ctx context.Context, cal perfectmind.Calendar, from, to time.Time) ([]perfectmind.RawClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[cal.Name]; err != nil {
		return nil, err
	}
	// Like PerfectMind, only return occurrences dated within [from, to]. Rows
	// with unparseable dates pass through so conversion failures can be tested.
	lo, hi := from.Format("20060102"), to.Format("20060102")
	var out []perfectmind.RawClass
	for _, c := range f.classes[cal.Name] {
		if _, err := time.Parse("20060102", c.OccurrenceDate); err == nil && (c.OccurrenceDate < lo || c.OccurrenceDate > hi) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeSource) AbsURL(p string) string { return "https://x" + p }
func (f *fakeSource) ClassURL(eventID, occurrenceDate string) string {
	return "https://x/class?" + eventID
}

// Registration mimics a drop-in course: residents open 21 clock hours before
// an 8:00 AM start (11:00 AM the day before), members and public 3 hours before.
func (f *fakeSource) Registration(ctx context.Context, eventID, occurrenceDate string, loc *time.Location) (*time.Time, *time.Time, *time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regCalls = append(f.regCalls, eventID+"|"+occurrenceDate)
	if err := f.regErr[eventID]; err != nil {
		return nil, nil, nil, err
	}
	if f.regNone[eventID] {
		return nil, nil, nil, nil
	}
	d, err := time.ParseInLocation("20060102", occurrenceDate, loc)
	if err != nil {
		return nil, nil, nil, err
	}
	res := d.Add(-13 * time.Hour)
	mem := d.Add(5 * time.Hour)
	return &res, &mem, &mem, nil
}

func cls(id, date, start string) perfectmind.RawClass {
	return perfectmind.RawClass{EventId: id, EventName: "C" + id, OccurrenceDate: date, FormattedStartTime: start, DurationInMinutes: 60, Facility: "F"}
}

// t0 is the default test clock start; tests advance their own clock, never this.
var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, toronto)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func setup(t *testing.T, src Source) (*Scraper, *store.Store, *clock) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &clock{t: t0}
	s := New(src, st, Config{Horizon: 24 * time.Hour * 90, Retention: 336 * time.Hour, Interval: time.Hour, Loc: toronto, Now: clk.Now},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, st, clk
}

func twoCats() *fakeSource {
	return &fakeSource{
		cals: []perfectmind.Calendar{{ID: "1", Name: "A", BookingLink: "/a"}, {ID: "2", Name: "B", BookingLink: "/b"}},
		classes: map[string][]perfectmind.RawClass{
			"A": {cls("1", "20261102", "08:00 AM"), cls("2", "20261103", "08:00 AM")},
			"B": {cls("3", "20261104", "09:00 AM")},
		},
		errs: map[string]error{},
	}
}

func count(t *testing.T, st *store.Store) int {
	t.Helper()
	ss, err := st.Sessions(context.Background(), schedule.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return len(ss)
}

func TestRefreshStoresAllCategories(t *testing.T) {
	s, st, _ := setup(t, twoCats())
	status, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Total != 3 || count(t, st) != 3 {
		t.Fatalf("total=%d", status.Total)
	}
	if len(status.Categories) != 2 || !status.Categories[0].OK || status.Categories[0].Count != 2 || status.Categories[1].Count != 1 {
		t.Fatalf("%+v", status.Categories)
	}
	if status.LastSuccess == nil || status.LastAttempt == nil {
		t.Fatal("times unset")
	}
	loaded, _ := st.LoadStatus(context.Background())
	if loaded.Total != 3 {
		t.Fatal("status not saved")
	}
}

func TestRefreshKeepsFailedCategory(t *testing.T) {
	src := twoCats()
	s, st, clk := setup(t, src)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.errs["A"] = errors.New("queue-it html")
	clk.Add(time.Hour)
	status, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count(t, st) != 3 {
		t.Fatalf("old data lost: %d", count(t, st))
	}
	a := status.Categories[0]
	if a.OK || a.Error == "" || a.Count != 0 {
		t.Fatalf("%+v", a)
	}
	if status.LastSuccess == nil || !status.LastSuccess.Equal(clk.Now()) {
		t.Fatalf("LastSuccess %v", status.LastSuccess)
	}
}

func TestRefreshAllFail(t *testing.T) {
	src := twoCats()
	s, _, clk := setup(t, src)
	first, _ := s.Refresh(context.Background())
	src.errs["A"], src.errs["B"] = errors.New("x"), errors.New("y")
	clk.Add(time.Hour)
	status, err := s.Refresh(context.Background())
	if err == nil || status.Error != "all categories failed" {
		t.Fatalf("err=%v status=%+v", err, status)
	}
	if !status.LastSuccess.Equal(*first.LastSuccess) {
		t.Fatal("LastSuccess changed")
	}
}

func TestRefreshDiscoveryFailure(t *testing.T) {
	src := twoCats()
	s, st, clk := setup(t, src)
	first, _ := s.Refresh(context.Background())
	src.calsErr = errors.New("boom")
	clk.Add(time.Hour)
	status, err := s.Refresh(context.Background())
	if err == nil || status.Error == "" {
		t.Fatalf("err=%v", err)
	}
	if count(t, st) != 3 || !status.LastSuccess.Equal(*first.LastSuccess) || len(status.Categories) != 2 {
		t.Fatalf("%+v", status)
	}
	if !status.LastAttempt.Equal(clk.Now()) {
		t.Fatal("LastAttempt not updated")
	}
}

func TestRefreshDropsVanishedCategory(t *testing.T) {
	src := twoCats()
	s, st, _ := setup(t, src)
	s.Refresh(context.Background())
	src.cals = src.cals[:1]
	status, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count(t, st) != 2 || status.Total != 2 || len(status.Categories) != 1 {
		t.Fatalf("%d %+v", count(t, st), status)
	}
}

func TestRefreshPrunesOld(t *testing.T) {
	src := twoCats()
	src.classes["A"] = append(src.classes["A"], cls("today", "20261030", "08:00 AM"))
	s, st, clk := setup(t, src)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count(t, st) != 4 {
		t.Fatalf("count=%d", count(t, st))
	}
	// 2026-10-30 08:00-09:00 is kept for Retention (336h) after it ends, then pruned.
	clk.Add(336*time.Hour - 4*time.Hour) // 2026-11-13 08:00: ended 335h ago
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count(t, st) != 4 {
		t.Fatalf("pruned too early: count=%d", count(t, st))
	}
	clk.Add(2 * time.Hour) // ended 337h ago
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ss, _ := st.Sessions(context.Background(), schedule.Filter{})
	for _, x := range ss {
		if x.Name == "Ctoday" {
			t.Fatalf("not pruned: %+v", ss)
		}
	}
}

// F1: a past session the source no longer returns (it only serves today onward)
// must survive later refreshes until it ages past Retention.
func TestRefreshKeepsPastSessionsUntilRetention(t *testing.T) {
	src := twoCats()
	src.classes["A"] = append(src.classes["A"], cls("y", "20261030", "07:00 PM")) // 19:00-20:00 on day 1
	s, st, clk := setup(t, src)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count(t, st) != 4 {
		t.Fatalf("run 1 count=%d", count(t, st))
	}
	clk.Add(24 * time.Hour) // next day: fetch starts 2026-10-31 00:00, "y" is no longer returned
	status, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := st.Sessions(context.Background(), schedule.Filter{})
	if len(ss) != 4 || ss[0].Name != "Cy" || len(ss[0].Categories) != 1 || ss[0].Categories[0] != "A" {
		t.Fatalf("yesterday's session lost: %+v", ss)
	}
	if status.Total != 4 {
		t.Fatalf("total=%d", status.Total)
	}
	// Ends 2026-10-30 20:00; prune once older than 336h (after 2026-11-13 20:00).
	clk.Add(13*24*time.Hour + 7*time.Hour) // 2026-11-13 19:00
	s.Refresh(context.Background())
	if count(t, st) != 4 {
		t.Fatalf("pruned before retention: %d", count(t, st))
	}
	clk.Add(2 * time.Hour) // 2026-11-13 21:00
	s.Refresh(context.Background())
	ss, _ = st.Sessions(context.Background(), schedule.Filter{})
	if len(ss) != 3 || ss[0].Name == "Cy" {
		t.Fatalf("not pruned after retention: %+v", ss)
	}
}

func TestRefreshBusy(t *testing.T) {
	src := twoCats()
	src.block = make(chan struct{})
	src.entered = make(chan struct{}, 2)
	s, _, _ := setup(t, src)
	done := make(chan error, 1)
	go func() { _, err := s.Refresh(context.Background()); done <- err }()
	<-src.entered
	if _, err := s.Refresh(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	close(src.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRefreshSkipsBadRows(t *testing.T) {
	src := twoCats()
	src.classes["A"] = append(src.classes["A"], cls("bad", "garbage", "nope"))
	s, st, _ := setup(t, src)
	status, err := s.Refresh(context.Background())
	if err != nil || !status.Categories[0].OK || status.Categories[0].Count != 2 || count(t, st) != 3 {
		t.Fatalf("err=%v %+v", err, status.Categories)
	}
}

func TestRunRefreshes(t *testing.T) {
	src := twoCats()
	s, st, _ := setup(t, src)
	s.cfg.Interval = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var n atomic.Int32
	s.onRefresh = func() { n.Add(1) }
	s.Run(ctx)
	if n.Load() < 2 || count(t, st) != 3 {
		t.Fatalf("refreshes=%d", n.Load())
	}
}

// F6: after a failed refresh, Run retries after RetryInterval, not Interval.
func TestRunRetriesSoonAfterFailure(t *testing.T) {
	src := twoCats()
	src.calsErr = errors.New("down")
	s, _, _ := setup(t, src)
	s.cfg.Interval = time.Hour
	s.cfg.RetryInterval = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var n atomic.Int32
	s.onRefresh = func() { n.Add(1) }
	s.Run(ctx)
	if n.Load() < 3 {
		t.Fatalf("refreshes=%d, want retries every RetryInterval", n.Load())
	}
}

func TestRunWaitsIntervalAfterSuccess(t *testing.T) {
	s, _, _ := setup(t, twoCats())
	s.cfg.Interval = time.Hour
	s.cfg.RetryInterval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var n atomic.Int32
	s.onRefresh = func() { n.Add(1) }
	s.Run(ctx)
	if n.Load() != 1 {
		t.Fatalf("refreshes=%d, want 1", n.Load())
	}
}

func TestNewDefaultsRetryInterval(t *testing.T) {
	s := New(twoCats(), nil, Config{Interval: 3 * time.Hour}, nil)
	if s.cfg.RetryInterval != 15*time.Minute {
		t.Fatalf("RetryInterval=%v", s.cfg.RetryInterval)
	}
	if s.retryDelay() != 15*time.Minute {
		t.Fatalf("retryDelay=%v", s.retryDelay())
	}
	s = New(twoCats(), nil, Config{Interval: 5 * time.Minute}, nil)
	if s.retryDelay() != 5*time.Minute {
		t.Fatalf("retryDelay=%v, want min(15m, Interval)", s.retryDelay())
	}
}

func TestRunWaitsWhenFresh(t *testing.T) {
	s, st, clk := setup(t, twoCats())
	s.cfg.Interval = time.Hour
	ts := clk.Now().Add(-time.Minute)
	st.SaveStatus(context.Background(), schedule.Status{LastSuccess: &ts})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var n atomic.Int32
	s.onRefresh = func() { n.Add(1) }
	s.Run(ctx)
	if n.Load() != 0 {
		t.Fatal("refreshed despite fresh data")
	}
}

func TestRefreshZeroCalendarsKeepsData(t *testing.T) {
	src := twoCats()
	s, st, clk := setup(t, src)
	first, _ := s.Refresh(context.Background())
	src.cals = nil
	clk.Add(time.Hour)
	status, err := s.Refresh(context.Background())
	if err == nil || status.Error != "no categories discovered" {
		t.Fatalf("err=%v %+v", err, status)
	}
	if count(t, st) != 3 || !status.LastSuccess.Equal(*first.LastSuccess) || len(status.Categories) != 2 {
		t.Fatalf("%d %+v", count(t, st), status)
	}
}

func TestRefreshAllRowsBadKeepsCategory(t *testing.T) {
	src := twoCats()
	s, st, _ := setup(t, src)
	s.Refresh(context.Background())
	src.classes["A"] = []perfectmind.RawClass{cls("x", "bad", "bad"), cls("y", "bad", "bad")}
	status, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := status.Categories[0]
	if a.OK || a.Error != "all 2 rows failed to convert" || count(t, st) != 3 {
		t.Fatalf("%+v count=%d", a, count(t, st))
	}
	// genuinely empty category is still a success
	src.classes["A"] = nil
	status, _ = s.Refresh(context.Background())
	if !status.Categories[0].OK || count(t, st) != 1 {
		t.Fatalf("%+v count=%d", status.Categories[0], count(t, st))
	}
}

// F2: a run in which every fetched category came back empty, while the store
// held upcoming sessions, is treated as a bad response, not a wipe.
func TestRefreshAllEmptyKeepsData(t *testing.T) {
	src := twoCats()
	s, st, clk := setup(t, src)
	first, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	src.classes = map[string][]perfectmind.RawClass{}
	clk.Add(time.Hour)
	status, err := s.Refresh(context.Background())
	if err == nil || status.Error != "all categories returned no sessions" {
		t.Fatalf("err=%v %+v", err, status)
	}
	if count(t, st) != 3 || !status.LastSuccess.Equal(*first.LastSuccess) || !status.LastAttempt.Equal(clk.Now()) {
		t.Fatalf("count=%d %+v", count(t, st), status)
	}
	loaded, _ := st.LoadStatus(context.Background())
	if loaded.Error != "all categories returned no sessions" {
		t.Fatalf("status not saved: %+v", loaded)
	}
	// One category emptying on its own (season end) is still applied.
	src.classes = map[string][]perfectmind.RawClass{"B": {cls("3", "20261104", "09:00 AM")}}
	status, err = s.Refresh(context.Background())
	if err != nil || count(t, st) != 1 || !status.Categories[0].OK {
		t.Fatalf("err=%v count=%d %+v", err, count(t, st), status)
	}
}

func TestRefreshAllEmptyOnEmptyStoreIsSuccess(t *testing.T) {
	src := twoCats()
	src.classes = map[string][]perfectmind.RawClass{}
	s, _, _ := setup(t, src)
	status, err := s.Refresh(context.Background())
	if err != nil || status.LastSuccess == nil || status.Error != "" {
		t.Fatalf("err=%v %+v", err, status)
	}
}

func regOf(t *testing.T, st *store.Store) map[string]*schedule.Registration {
	t.Helper()
	ss, err := st.Sessions(context.Background(), schedule.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*schedule.Registration{}
	for _, x := range ss {
		out[x.Name+"@"+x.Start.Format("0102")] = x.Registration
	}
	return out
}

func TestRegistrationLookedUpOncePerCourse(t *testing.T) {
	src := &fakeSource{
		cals:    []perfectmind.Calendar{{ID: "1", Name: "A", BookingLink: "/a"}},
		classes: map[string][]perfectmind.RawClass{"A": {cls("1", "20261109", "08:00 AM"), cls("1", "20261102", "08:00 AM"), cls("2", "20261103", "08:00 AM")}},
		regNone: map[string]bool{"2": true},
	}
	s, st, _ := setup(t, src)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(src.regCalls) != 2 || src.regCalls[0] != "1|20261102" {
		t.Fatalf("calls %v (want one per course, earliest occurrence first)", src.regCalls)
	}
	reg := regOf(t, st)
	for _, k := range []string{"C1@1102", "C1@1109"} {
		r := reg[k]
		if r == nil || r.Residents == nil {
			t.Fatalf("%s: %+v", k, r)
		}
		day := r.Residents.In(toronto)
		if day.Hour() != 11 || day.Day() != map[string]int{"C1@1102": 1, "C1@1109": 8}[k] {
			t.Fatalf("%s residents %v", k, day)
		}
		if r.Members.In(toronto).Hour() != 5 {
			t.Fatalf("%s members %v", k, r.Members)
		}
	}
	if r := reg["C2@1103"]; r == nil || r.Online() {
		t.Fatalf("in-person course should have empty, non-nil registration: %+v", r)
	}

	// A second refresh reuses the saved rules.
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(src.regCalls) != 2 {
		t.Fatalf("rules should be cached, calls %v", src.regCalls)
	}
}

func TestRegistrationLookupBudget(t *testing.T) {
	src := twoCats()
	s, st, _ := setup(t, src)
	s.cfg.RegLookups = 1
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg := regOf(t, st)
	if len(src.regCalls) != 1 || reg["C1@1102"] == nil || reg["C2@1103"] != nil || reg["C3@1104"] != nil {
		t.Fatalf("calls %v reg %v", src.regCalls, reg)
	}
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(src.regCalls) != 2 || src.regCalls[1] != "2|20261103" || regOf(t, st)["C2@1103"] == nil {
		t.Fatalf("next refresh should continue with the next course: %v", src.regCalls)
	}
}

func TestRegistrationFailureLeavesUnknown(t *testing.T) {
	src := twoCats()
	src.regErr = map[string]error{"1": errors.New("boom")}
	s, st, _ := setup(t, src)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("lookup failures must not fail the refresh: %v", err)
	}
	reg := regOf(t, st)
	if reg["C1@1102"] != nil || reg["C2@1103"] == nil {
		t.Fatalf("reg %v", reg)
	}
}

func TestRegistrationStaleRuleRechecked(t *testing.T) {
	src := &fakeSource{
		cals:    []perfectmind.Calendar{{ID: "1", Name: "A", BookingLink: "/a"}},
		classes: map[string][]perfectmind.RawClass{"A": {cls("1", "20261125", "08:00 AM")}},
	}
	s, _, clk := setup(t, src)
	s.Refresh(context.Background())
	clk.Add(7 * 24 * time.Hour)
	s.Refresh(context.Background())
	if len(src.regCalls) != 1 {
		t.Fatalf("fresh rule re-fetched: %v", src.regCalls)
	}
	clk.Add(8 * 24 * time.Hour)
	s.Refresh(context.Background())
	if len(src.regCalls) != 2 {
		t.Fatalf("rule older than 14 days should be re-checked: %v", src.regCalls)
	}
}

func TestRefreshRecordsRuns(t *testing.T) {
	src := twoCats()
	s, st, _ := setup(t, src)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.calsErr = errors.New("queue-it")
	s.Refresh(context.Background())

	runs, err := st.Runs(context.Background(), 0)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	bad, good := runs[0], runs[1]
	if !good.OK || good.Total != 3 || len(good.Categories) != 2 || good.Categories[0].Count != 2 || good.Error != "" {
		t.Fatalf("good run %+v", good)
	}
	if good.Registration.Courses != 3 || good.Registration.LookedUp != 3 || good.Registration.Unknown != 0 {
		t.Fatalf("good run registration %+v", good.Registration)
	}
	if bad.OK || !strings.Contains(bad.Error, "queue-it") || bad.Started.IsZero() {
		t.Fatalf("failed run %+v", bad)
	}
}

func TestRequestRefreshRunsNow(t *testing.T) {
	src := twoCats()
	s, st, clk := setup(t, src)
	s.cfg.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Fresh data, so Run would otherwise wait an hour.
	if _, err := s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Add(16 * time.Minute) // past the default 15m cooldown
	done := make(chan struct{}, 4)
	s.onRefresh = func() { done <- struct{}{} }
	go s.Run(ctx)
	if err := s.RequestRefresh(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("requested refresh did not run")
	}
	if runs, _ := st.Runs(ctx, 0); len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(runs))
	}
}

func TestRequestRefreshCooldown(t *testing.T) {
	s, _, clk := setup(t, twoCats())
	ctx := context.Background()
	if _, err := s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Add(5 * time.Minute)
	err := s.RequestRefresh(ctx)
	var soon *TooSoonError
	if !errors.As(err, &soon) || soon.Wait != 10*time.Minute {
		t.Fatalf("want TooSoonError{10m}, got %v", err)
	}
	if at, _ := s.ManualRefreshAt(ctx); !at.Equal(clk.Now().Add(10 * time.Minute)) {
		t.Fatalf("ManualRefreshAt = %v", at)
	}
	clk.Add(10 * time.Minute)
	if err := s.RequestRefresh(ctx); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
	if !s.Refreshing() {
		t.Fatal("a queued refresh should count as refreshing")
	}
	if err := s.RequestRefresh(ctx); !errors.Is(err, ErrBusy) {
		t.Fatalf("second request while queued: want ErrBusy, got %v", err)
	}
}

func TestRequestRefreshBusyWhileRunning(t *testing.T) {
	src := twoCats()
	src.block = make(chan struct{})
	src.entered = make(chan struct{}, 2)
	s, _, _ := setup(t, src)
	done := make(chan error, 1)
	go func() { _, err := s.Refresh(context.Background()); done <- err }()
	<-src.entered
	if !s.Refreshing() {
		t.Fatal("Refreshing should be true during a refresh")
	}
	if err := s.RequestRefresh(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	close(src.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.Refreshing() {
		t.Fatal("Refreshing should be false afterwards")
	}
}

func TestRequestRefreshNoHistory(t *testing.T) {
	s, _, _ := setup(t, twoCats())
	if at, err := s.ManualRefreshAt(context.Background()); err != nil || !at.IsZero() {
		t.Fatalf("no runs yet: want zero time, got %v %v", at, err)
	}
	if err := s.RequestRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBookingURLIsTheSessionsClassPage(t *testing.T) {
	src := twoCats()
	src.classes["A"] = append(src.classes["A"], perfectmind.RawClass{OccurrenceDate: "20261101", FormattedStartTime: "09:00 AM", DurationInMinutes: 60, EventName: "NoID"})
	s, st, _ := setup(t, src)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	all, err := st.Sessions(context.Background(), schedule.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range all {
		want := "https://x/class?" + x.Name[1:]
		if x.Name == "NoID" {
			want = "https://x/a" // no event id: fall back to the program's page
		}
		if x.BookingURL != want {
			t.Errorf("%s: booking url %q, want %q", x.Name, x.BookingURL, want)
		}
	}
}
