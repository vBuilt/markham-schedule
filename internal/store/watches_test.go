package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/schedule"
)

func TestAddWatchAndList(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	start := t0.Add(48 * time.Hour)
	w, err := s.AddWatch(ctx, 1, "s1", start, t0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UserWatches(ctx, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("UserWatches = %+v, %v", got, err)
	}
	if got[0].ID != w.ID || got[0].SessionID != "s1" || !got[0].SessionStart.Equal(start) ||
		!got[0].Created.Equal(t0) || got[0].EmailsSent != 0 || got[0].LastEmailed != nil {
		t.Fatalf("bad watch %+v", got[0])
	}
	if _, err := s.AddWatch(ctx, 1, "s1", start, t0); !errors.Is(err, ErrWatchExists) {
		t.Fatalf("dup err = %v", err)
	}
	st, err := s.WatchStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := st["s1"]; !ok || v.LastSpots != 0 {
		t.Fatalf("states = %+v", st)
	}
}

func TestDeleteWatchKeepsOthersState(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	start := t0.Add(48 * time.Hour)
	s.AddWatch(ctx, 1, "s1", start, t0)
	s.AddWatch(ctx, 2, "s1", start, t0)
	if ok, err := s.DeleteWatch(ctx, 1, "s1"); err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if st, _ := s.WatchStates(ctx); len(st) != 1 {
		t.Fatalf("state should remain: %+v", st)
	}
	if ok, err := s.DeleteWatch(ctx, 1, "s1"); err != nil || ok {
		t.Fatalf("second delete = %v, %v", ok, err)
	}
	if ok, _ := s.DeleteWatch(ctx, 2, "s1"); !ok {
		t.Fatal("last delete should succeed")
	}
	if st, _ := s.WatchStates(ctx); len(st) != 0 {
		t.Fatalf("state should be gone: %+v", st)
	}
}

func TestStopTokens(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	w, _ := s.AddWatch(ctx, 1, "s1", t0.Add(time.Hour), t0)
	s.AddStopToken(ctx, w.ID, []byte("a"))
	s.AddStopToken(ctx, w.ID, []byte("b"))
	if ok, err := s.DeleteWatchByStopToken(ctx, []byte("a")); err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	if ok, err := s.DeleteWatchByStopToken(ctx, []byte("b")); err != nil || ok {
		t.Fatalf("second = %v, %v", ok, err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM watch_stop_tokens`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d tokens left", n)
	}
}

func TestPurgeWatches(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	past, _ := s.AddWatch(ctx, 1, "old", t0.Add(-time.Hour), t0.Add(-48*time.Hour))
	s.AddStopToken(ctx, past.ID, []byte("p"))
	fut, _ := s.AddWatch(ctx, 1, "new", t0.Add(time.Hour), t0)
	s.AddStopToken(ctx, fut.ID, []byte("f"))
	if _, err := s.PurgeWatches(ctx, t0); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.AllWatches(ctx)
	if len(ws) != 1 || ws[0].SessionID != "new" {
		t.Fatalf("watches = %+v", ws)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM watch_stop_tokens`).Scan(&n)
	if n != 1 {
		t.Fatalf("tokens = %d", n)
	}
	if st, _ := s.WatchStates(ctx); len(st) != 1 {
		t.Fatalf("states = %+v", st)
	}
	var m int
	s.db.QueryRow(`SELECT COUNT(*) FROM watch_state`).Scan(&m)
	if m != 1 {
		t.Fatalf("watch_state rows = %d", m)
	}
}

func TestMarkEmailed(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	w, _ := s.AddWatch(ctx, 1, "s1", t0.Add(time.Hour), t0)
	now := t0.Add(time.Minute)
	if err := s.MarkEmailed(ctx, w.ID, now); err != nil {
		t.Fatal(err)
	}
	got, _ := s.WatchersOf(ctx, "s1")
	if len(got) != 1 || got[0].EmailsSent != 1 || got[0].LastEmailed == nil || !got[0].LastEmailed.Equal(now) {
		t.Fatalf("got %+v", got)
	}
}

func TestWatchStateSaveAndDeleteSession(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	w, _ := s.AddWatch(ctx, 1, "s1", t0.Add(time.Hour), t0)
	s.AddStopToken(ctx, w.ID, []byte("a"))
	chk := t0.Add(time.Minute)
	if err := s.SaveWatchState(ctx, WatchState{SessionID: "s1", LastSpots: 3, LastChecked: &chk, Misses: 2, LastError: "boom"}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.WatchStates(ctx)
	v := st["s1"]
	if v.LastSpots != 3 || v.Misses != 2 || v.LastError != "boom" || v.LastChecked == nil || !v.LastChecked.Equal(chk) {
		t.Fatalf("state %+v", v)
	}
	if err := s.DeleteSessionWatches(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.AllWatches(ctx)
	st, _ = s.WatchStates(ctx)
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM watch_stop_tokens`).Scan(&n)
	if len(ws) != 0 || len(st) != 0 || n != 0 {
		t.Fatalf("leftovers %v %v %d", ws, st, n)
	}
}

func TestSessionByIDAndUpdateSpots(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	a := sess("a", "Swim", "Centre", "Swim", d(2, 9))
	if err := s.ReplaceCategory(ctx, "Swim", d(1, 0), []schedule.Session{a}); err != nil {
		t.Fatal(err)
	}
	two := 2
	t1 := t0.Add(time.Hour)
	if err := s.UpdateSessionSpots(ctx, "a", "2 spots", &two, t1); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionByID(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spots != "2 spots" || got.SpotsLeft == nil || *got.SpotsLeft != 2 || !got.ScrapedAt.Equal(t1) || got.Name != "Swim" {
		t.Fatalf("got %+v", got)
	}
	if err := s.UpdateSessionSpots(ctx, "a", "Full", nil, t1); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SessionByID(ctx, "a")
	if got.SpotsLeft != nil || got.Spots != "Full" {
		t.Fatalf("nil left: %+v", got)
	}
	if _, err := s.SessionByID(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

func TestDeleteUserRemovesWatches(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "a@x.com", "h", t0)
	w, _ := s.AddWatch(ctx, u.ID, "s1", t0.Add(time.Hour), t0)
	s.AddStopToken(ctx, w.ID, []byte("a"))
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.AllWatches(ctx)
	st, _ := s.WatchStates(ctx)
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM watch_stop_tokens`).Scan(&n)
	var m int
	s.db.QueryRow(`SELECT COUNT(*) FROM watch_state`).Scan(&m)
	if len(ws) != 0 || len(st) != 0 || n != 0 || m != 0 {
		t.Fatalf("leftovers %v %v %d %d", ws, st, n, m)
	}
}

func TestAddWatchResetsStaleState(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	start := t0.Add(48 * time.Hour)
	// Stale row with no watches.
	s.SaveWatchState(ctx, WatchState{SessionID: "s1", LastSpots: 3, Misses: 2, LastError: "x"})
	if _, err := s.AddWatch(ctx, 1, "s1", start, t0); err != nil {
		t.Fatal(err)
	}
	st, _ := s.WatchStates(ctx)
	if got := st["s1"]; got.LastSpots != 0 || got.Misses != 0 || got.LastError != "" {
		t.Fatalf("state not reset: %+v", got)
	}
	// With another watcher present, state is kept.
	s.SaveWatchState(ctx, WatchState{SessionID: "s1", LastSpots: 3})
	if _, err := s.AddWatch(ctx, 2, "s1", start, t0); err != nil {
		t.Fatal(err)
	}
	st, _ = s.WatchStates(ctx)
	if st["s1"].LastSpots != 3 {
		t.Fatalf("state should be kept: %+v", st["s1"])
	}
}
