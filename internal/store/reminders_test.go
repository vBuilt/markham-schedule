package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAddReminder(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	at := t0.Add(48 * time.Hour)
	r, err := s.AddReminder(ctx, 1, "s1", at, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddReminder(ctx, 1, "s1", at, t0); !errors.Is(err, ErrReminderExists) {
		t.Fatalf("dup err = %v", err)
	}
	got, err := s.UserReminders(ctx, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("UserReminders = %+v, %v", got, err)
	}
	g := got[0]
	if g.ID != r.ID || g.UserID != 1 || g.SessionID != "s1" || g.ResidentsAt.Unix() != at.Unix() ||
		g.Created.Unix() != t0.Unix() || g.SentAt != nil || g.SendError != "" {
		t.Fatalf("bad reminder %+v", g)
	}
}

func TestMarkReminderSentOnce(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	r, _ := s.AddReminder(ctx, 1, "s1", t0.Add(time.Hour), t0)
	if ok, err := s.MarkReminderSent(ctx, r.ID, t0); err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	if ok, err := s.MarkReminderSent(ctx, r.ID, t0); err != nil || ok {
		t.Fatalf("second = %v, %v", ok, err)
	}
	if p, err := s.PendingReminders(ctx); err != nil || len(p) != 0 {
		t.Fatalf("pending = %+v, %v", p, err)
	}
	got, _ := s.UserReminders(ctx, 1)
	if len(got) != 1 || got[0].SentAt == nil || got[0].SentAt.Unix() != t0.Unix() {
		t.Fatalf("user reminders = %+v", got)
	}
}

func TestSetReminderTimeAndError(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	r, _ := s.AddReminder(ctx, 1, "s1", t0.Add(time.Hour), t0)
	later := t0.Add(5 * time.Hour)
	if err := s.SetReminderTime(ctx, r.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReminderError(ctx, r.ID, "smtp down"); err != nil {
		t.Fatal(err)
	}
	all, err := s.AllReminders(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	if all[0].ResidentsAt.Unix() != later.Unix() || all[0].SendError != "smtp down" {
		t.Fatalf("got %+v", all[0])
	}
}

func TestDeleteReminderByCancelToken(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	r, _ := s.AddReminder(ctx, 1, "s1", t0.Add(time.Hour), t0)
	s.AddReminderCancelToken(ctx, r.ID, []byte("a"))
	s.AddReminderCancelToken(ctx, r.ID, []byte("b"))
	if ok, err := s.DeleteReminderByCancelToken(ctx, []byte("a")); err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	if ok, err := s.DeleteReminderByCancelToken(ctx, []byte("a")); err != nil || ok {
		t.Fatalf("second = %v, %v", ok, err)
	}
	if ok, _ := s.DeleteReminderByCancelToken(ctx, []byte("b")); ok {
		t.Fatal("other token should be gone with the reminder")
	}
	if all, _ := s.AllReminders(ctx); len(all) != 0 {
		t.Fatalf("reminder remains: %+v", all)
	}
}

func TestDeleteAndDropReminder(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	r, _ := s.AddReminder(ctx, 1, "s1", t0.Add(time.Hour), t0)
	s.AddReminderCancelToken(ctx, r.ID, []byte("a"))
	if ok, err := s.DeleteReminder(ctx, 2, "s1"); err != nil || ok {
		t.Fatalf("other user = %v, %v", ok, err)
	}
	if ok, err := s.DeleteReminder(ctx, 1, "s1"); err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if ok, _ := s.DeleteReminderByCancelToken(ctx, []byte("a")); ok {
		t.Fatal("token should be gone")
	}
	r2, _ := s.AddReminder(ctx, 1, "s2", t0.Add(time.Hour), t0)
	s.AddReminderCancelToken(ctx, r2.ID, []byte("b"))
	if err := s.DropReminder(ctx, r2.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.AllReminders(ctx); len(all) != 0 {
		t.Fatalf("remains: %+v", all)
	}
	if ok, _ := s.DeleteReminderByCancelToken(ctx, []byte("b")); ok {
		t.Fatal("token should be gone")
	}
}

func TestPurgeReminders(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	old, _ := s.AddReminder(ctx, 1, "old", t0.Add(time.Hour), t0)
	recent, _ := s.AddReminder(ctx, 1, "recent", t0.Add(2*time.Hour), t0)
	s.AddReminder(ctx, 1, "pending", t0.Add(3*time.Hour), t0)
	s.AddReminderCancelToken(ctx, old.ID, []byte("old"))
	s.MarkReminderSent(ctx, old.ID, t0.Add(-8*24*time.Hour))
	s.MarkReminderSent(ctx, recent.ID, t0.Add(-24*time.Hour))
	n, err := s.PurgeReminders(ctx, t0.Add(-7*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
	all, _ := s.AllReminders(ctx)
	if len(all) != 2 || all[0].SessionID != "recent" || all[1].SessionID != "pending" {
		t.Fatalf("remaining = %+v", all)
	}
	if ok, _ := s.DeleteReminderByCancelToken(ctx, []byte("old")); ok {
		t.Fatal("purged reminder's token should be gone")
	}
}

func TestDeleteUserRemovesReminders(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "a@x.com", "h", t0)
	r, _ := s.AddReminder(ctx, u.ID, "s1", t0.Add(time.Hour), t0)
	s.AddReminderCancelToken(ctx, r.ID, []byte("a"))
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.AllReminders(ctx); len(all) != 0 {
		t.Fatalf("reminders remain: %+v", all)
	}
	if ok, _ := s.DeleteReminderByCancelToken(ctx, []byte("a")); ok {
		t.Fatal("token remains")
	}
}

func TestReminderSentByCancelToken(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	r, _ := s.AddReminder(ctx, 1, "s1", t0.Add(time.Hour), t0)
	s.AddReminderCancelToken(ctx, r.ID, []byte("a"))
	if sent, found, err := s.ReminderSentByCancelToken(ctx, []byte("a")); err != nil || sent || !found {
		t.Fatalf("pending = %v %v %v", sent, found, err)
	}
	s.MarkReminderSent(ctx, r.ID, t0)
	if sent, found, err := s.ReminderSentByCancelToken(ctx, []byte("a")); err != nil || !sent || !found {
		t.Fatalf("sent = %v %v %v", sent, found, err)
	}
	if _, found, _ := s.ReminderSentByCancelToken(ctx, []byte("zz")); found {
		t.Fatal("unknown token found")
	}
}
