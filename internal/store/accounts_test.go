package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func TestCreateUserAndLookup(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "a@x.com", "h", t0)
	if err != nil {
		t.Fatal(err)
	}
	if u.Confirmed || u.Email != "a@x.com" || u.PasswordHash != "h" || !u.Created.Equal(t0) {
		t.Fatalf("bad user %+v", u)
	}
	byEmail, err := s.UserByEmail(ctx, "a@x.com")
	if err != nil || byEmail != u {
		t.Fatalf("UserByEmail = %+v, %v", byEmail, err)
	}
	byID, err := s.UserByID(ctx, u.ID)
	if err != nil || byID != u {
		t.Fatalf("UserByID = %+v, %v", byID, err)
	}
	if _, err := s.CreateUser(ctx, "a@x.com", "h2", t0); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate err = %v", err)
	}
	if _, err := s.UserByEmail(ctx, "nobody@x.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	if _, err := s.UserByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id err = %v", err)
	}
}

func TestSetPasswordAndConfirm(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "a@x.com", "h", t0)
	if err := s.SetPassword(ctx, u.ID, "new"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.PasswordHash != "new" || !got.Confirmed {
		t.Fatalf("got %+v", got)
	}
}

func TestAuthSessionLifecycle(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	h := []byte("tok")
	exp := t0.Add(time.Hour)
	if err := s.CreateAuthSession(ctx, h, 7, exp); err != nil {
		t.Fatal(err)
	}
	uid, gotExp, err := s.AuthSession(ctx, h, t0)
	if err != nil || uid != 7 || !gotExp.Equal(exp) {
		t.Fatalf("got %d %v %v", uid, gotExp, err)
	}
	if _, _, err := s.AuthSession(ctx, h, exp.Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired err = %v", err)
	}
	exp2 := exp.Add(24 * time.Hour)
	if err := s.ExtendAuthSession(ctx, h, exp2); err != nil {
		t.Fatal(err)
	}
	if _, gotExp, err := s.AuthSession(ctx, h, exp.Add(time.Second)); err != nil || !gotExp.Equal(exp2) {
		t.Fatalf("after extend: %v %v", gotExp, err)
	}
	if err := s.DeleteAuthSession(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthSession(ctx, h, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted err = %v", err)
	}
}

func TestDeleteUserSessionsExcept(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	exp := t0.Add(time.Hour)
	for _, k := range []string{"a", "keep", "c"} {
		if err := s.CreateAuthSession(ctx, []byte(k), 1, exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateAuthSession(ctx, []byte("other"), 2, exp); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUserSessions(ctx, 1, []byte("keep")); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]bool{"a": false, "keep": true, "c": false, "other": true} {
		_, _, err := s.AuthSession(ctx, []byte(k), t0)
		if (err == nil) != want {
			t.Errorf("%s: present=%v want %v (%v)", k, err == nil, want, err)
		}
	}
	if err := s.DeleteUserSessions(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthSession(ctx, []byte("keep"), t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("keep err = %v", err)
	}
}

func TestUseTokenOnceAndKind(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	h := []byte("t")
	exp := t0.Add(time.Hour)
	if err := s.CreateToken(ctx, h, "confirm", 5, exp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseToken(ctx, h, "reset", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong kind err = %v", err)
	}
	if uid, err := s.UseToken(ctx, h, "confirm", t0); err != nil || uid != 5 {
		t.Fatalf("got %d %v", uid, err)
	}
	if _, err := s.UseToken(ctx, h, "confirm", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reuse err = %v", err)
	}
	if err := s.CreateToken(ctx, h, "confirm", 5, exp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseToken(ctx, h, "confirm", exp.Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired err = %v", err)
	}
}

func TestDeleteUserCascades(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "a@x.com", "h", t0)
	exp := t0.Add(time.Hour)
	s.CreateAuthSession(ctx, []byte("s"), u.ID, exp)
	s.CreateToken(ctx, []byte("t"), "confirm", u.ID, exp)
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByID(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user err = %v", err)
	}
	if _, _, err := s.AuthSession(ctx, []byte("s"), t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session err = %v", err)
	}
	if _, err := s.UseToken(ctx, []byte("t"), "confirm", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token err = %v", err)
	}
}

func TestPurgeAuth(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	day := 24 * time.Hour
	old, _ := s.CreateUser(ctx, "old@x.com", "h", t0.Add(-8*day))
	recent, _ := s.CreateUser(ctx, "recent@x.com", "h", t0.Add(-1*day))
	conf, _ := s.CreateUser(ctx, "conf@x.com", "h", t0.Add(-30*day))
	if err := s.ConfirmUser(ctx, conf.ID); err != nil {
		t.Fatal(err)
	}
	s.CreateAuthSession(ctx, []byte("dead"), conf.ID, t0.Add(-time.Hour))
	s.CreateAuthSession(ctx, []byte("live"), conf.ID, t0.Add(time.Hour))
	s.CreateAuthSession(ctx, []byte("oldsess"), old.ID, t0.Add(time.Hour))
	s.CreateToken(ctx, []byte("deadt"), "reset", conf.ID, t0.Add(-time.Hour))
	s.CreateToken(ctx, []byte("livet"), "reset", conf.ID, t0.Add(time.Hour))

	n, err := s.PurgeAuth(ctx, t0, t0.Add(-7*day))
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // dead session, dead token, old user, old user's session
		t.Errorf("purged %d, want 4", n)
	}
	if _, err := s.UserByID(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("old user kept: %v", err)
	}
	for _, id := range []int64{recent.ID, conf.ID} {
		if _, err := s.UserByID(ctx, id); err != nil {
			t.Errorf("user %d removed: %v", id, err)
		}
	}
	if _, _, err := s.AuthSession(ctx, []byte("dead"), t0.Add(-2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("dead session kept: %v", err)
	}
	if _, _, err := s.AuthSession(ctx, []byte("live"), t0); err != nil {
		t.Errorf("live session removed: %v", err)
	}
	if _, err := s.UseToken(ctx, []byte("livet"), "reset", t0); err != nil {
		t.Errorf("live token removed: %v", err)
	}
	if _, err := s.UseToken(ctx, []byte("deadt"), "reset", t0.Add(-2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("dead token kept: %v", err)
	}
}
