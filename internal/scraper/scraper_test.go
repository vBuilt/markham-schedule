package scraper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
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
