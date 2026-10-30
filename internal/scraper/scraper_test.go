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
	return f.classes[cal.Name], nil
}

func (f *fakeSource) AbsURL(p string) string { return "https://x" + p }

func cls(id, date, start string) perfectmind.RawClass {
	return perfectmind.RawClass{EventId: id, EventName: "C" + id, OccurrenceDate: date, FormattedStartTime: start, DurationInMinutes: 60, Facility: "F"}
}

var now = time.Date(2026, 10, 30, 12, 0, 0, 0, toronto)

func setup(t *testing.T, src Source) (*Scraper, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(src, st, Config{Horizon: 24 * time.Hour * 90, Retention: 336 * time.Hour, Interval: time.Hour, Loc: toronto, Now: func() time.Time { return now }},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, st
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
	s, st := setup(t, twoCats())
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
	s, st := setup(t, src)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.errs["A"] = errors.New("queue-it html")
	now = now.Add(time.Hour)
	defer func() { now = now.Add(-time.Hour) }()
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
	if status.LastSuccess == nil || !status.LastSuccess.Equal(now) {
		t.Fatalf("LastSuccess %v", status.LastSuccess)
	}
}

func TestRefreshAllFail(t *testing.T) {
	src := twoCats()
	s, _ := setup(t, src)
	first, _ := s.Refresh(context.Background())
	src.errs["A"], src.errs["B"] = errors.New("x"), errors.New("y")
	now = now.Add(time.Hour)
	defer func() { now = now.Add(-time.Hour) }()
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
	s, st := setup(t, src)
	first, _ := s.Refresh(context.Background())
	src.calsErr = errors.New("boom")
	now = now.Add(time.Hour)
	defer func() { now = now.Add(-time.Hour) }()
	status, err := s.Refresh(context.Background())
	if err == nil || status.Error == "" {
		t.Fatalf("err=%v", err)
	}
	if count(t, st) != 3 || !status.LastSuccess.Equal(*first.LastSuccess) || len(status.Categories) != 2 {
		t.Fatalf("%+v", status)
	}
	if !status.LastAttempt.Equal(now) {
		t.Fatal("LastAttempt not updated")
	}
}

func TestRefreshDropsVanishedCategory(t *testing.T) {
	src := twoCats()
	s, st := setup(t, src)
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
	// Retention 336h; class ending now-337h vs now-1h. now = 2026-10-30 12:00.
	old := now.Add(-337 * time.Hour).Add(-time.Hour) // start; 60min duration -> ends now-337h
	recent := now.Add(-2 * time.Hour)                // ends now-1h
	fmtT := func(t time.Time) (string, string) { return t.Format("20060102"), t.Format("03:04 PM") }
	od, ot := fmtT(old)
	rd, rt := fmtT(recent)
	src.classes = map[string][]perfectmind.RawClass{"A": {cls("old", od, ot), cls("recent", rd, rt)}, "B": nil}
	s, st := setup(t, src)
	if _, err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ss, _ := st.Sessions(context.Background(), schedule.Filter{})
	if len(ss) != 1 || ss[0].Name != "Crecent" {
		t.Fatalf("%+v", ss)
	}
}

func TestRefreshBusy(t *testing.T) {
	src := twoCats()
	src.block = make(chan struct{})
	src.entered = make(chan struct{}, 2)
	s, _ := setup(t, src)
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
	s, st := setup(t, src)
	status, err := s.Refresh(context.Background())
	if err != nil || !status.Categories[0].OK || status.Categories[0].Count != 2 || count(t, st) != 3 {
		t.Fatalf("err=%v %+v", err, status.Categories)
	}
}

func TestRunRefreshes(t *testing.T) {
	src := twoCats()
	s, st := setup(t, src)
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

func TestRunWaitsWhenFresh(t *testing.T) {
	s, st := setup(t, twoCats())
	s.cfg.Interval = time.Hour
	ts := now.Add(-time.Minute)
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
	s, st := setup(t, src)
	first, _ := s.Refresh(context.Background())
	src.cals = nil
	now = now.Add(time.Hour)
	defer func() { now = now.Add(-time.Hour) }()
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
	s, st := setup(t, src)
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
