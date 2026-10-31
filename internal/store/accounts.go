package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrEmailTaken = errors.New("email already registered")
)

type User struct {
	ID           int64
	Email        string
	PasswordHash string
	Confirmed    bool
	Created      time.Time
	Name         string
}

const userCols = `id, email, password_hash, confirmed, created_unix, name`

func scanUser(row *sql.Row) (User, error) {
	var u User
	var created int64
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Confirmed, &created, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Created = time.Unix(created, 0)
	return u, nil
}

func (s *Store) CreateUser(ctx context.Context, email, hash string, now time.Time) (User, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users(email, password_hash, created_unix) VALUES(?,?,?)`, email, hash, now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return User{}, ErrEmailTaken
		}
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return s.UserByID(ctx, id)
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email=?`, email))
}

func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (s *Store) SetPassword(ctx context.Context, userID int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, userID)
	return err
}

func (s *Store) SetName(ctx context.Context, userID int64, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET name=? WHERE id=?`, name, userID)
	return err
}

func (s *Store) ConfirmUser(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET confirmed=1 WHERE id=?`, userID)
	return err
}

// TouchUser restarts the unconfirmed-account purge clock: PurgeAuth measures
// an unconfirmed user's age from created_unix, which this sets to now.
func (s *Store) TouchUser(ctx context.Context, userID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET created_unix=? WHERE id=?`, now.Unix(), userID)
	return err
}

// DeleteUser removes the user along with their sessions and tokens.
func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM user_sessions WHERE user_id=?`,
			`DELETE FROM auth_tokens WHERE user_id=?`,
			`DELETE FROM watch_stop_tokens WHERE watch_id IN (SELECT id FROM watches WHERE user_id=?)`,
			`DELETE FROM watches WHERE user_id=?`,
			`DELETE FROM reminder_cancel_tokens WHERE reminder_id IN (SELECT id FROM reminders WHERE user_id=?)`,
			`DELETE FROM reminders WHERE user_id=?`,
			`DELETE FROM users WHERE id=?`,
		} {
			if _, err := tx.ExecContext(ctx, q, userID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM watch_state WHERE session_id NOT IN (SELECT session_id FROM watches)`)
		return err
	})
}

func (s *Store) CreateAuthSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_sessions(token_hash, user_id, expires_unix) VALUES(?,?,?)`,
		tokenHash, userID, expires.Unix())
	return err
}

// AuthSession returns ErrNotFound if the session is missing or expired at now.
func (s *Store) AuthSession(ctx context.Context, tokenHash []byte, now time.Time) (int64, time.Time, error) {
	var uid, exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, expires_unix FROM user_sessions WHERE token_hash=? AND expires_unix>?`,
		tokenHash, now.Unix()).Scan(&uid, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, ErrNotFound
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	return uid, time.Unix(exp, 0), nil
}

func (s *Store) ExtendAuthSession(ctx context.Context, tokenHash []byte, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_sessions SET expires_unix=? WHERE token_hash=?`,
		expires.Unix(), tokenHash)
	return err
}

func (s *Store) DeleteAuthSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE token_hash=?`, tokenHash)
	return err
}

// DeleteUserSessions removes all of the user's sessions except the one whose
// hash is except (nil to remove them all).
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, except []byte) error {
	if except == nil {
		_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=?`, userID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id=? AND token_hash<>?`, userID, except)
	return err
}

func (s *Store) CreateToken(ctx context.Context, tokenHash []byte, kind string, userID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_tokens(token_hash, kind, user_id, expires_unix) VALUES(?,?,?,?)`,
		tokenHash, kind, userID, expires.Unix())
	return err
}

// DeleteUserTokens removes all of the user's outstanding tokens of kind.
func (s *Store) DeleteUserTokens(ctx context.Context, userID int64, kind string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_tokens WHERE user_id=? AND kind=?`, userID, kind)
	return err
}

// UseToken consumes a single-use token. ErrNotFound if it is missing, of
// another kind, or expired at now.
func (s *Store) UseToken(ctx context.Context, tokenHash []byte, kind string, now time.Time) (int64, error) {
	var uid int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT user_id FROM auth_tokens WHERE token_hash=? AND kind=? AND expires_unix>?`,
			tokenHash, kind, now.Unix()).Scan(&uid)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE token_hash=?`, tokenHash)
		return err
	})
	if err != nil {
		return 0, err
	}
	return uid, nil
}

// PurgeAuth removes expired sessions and tokens, and unconfirmed users created
// before unconfirmedBefore along with their sessions and tokens. It returns the
// number of rows deleted.
func (s *Store) PurgeAuth(ctx context.Context, now, unconfirmedBefore time.Time) (int64, error) {
	var total int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		total = 0
		stale := `SELECT id FROM users WHERE confirmed=0 AND created_unix<?`
		for _, st := range []struct {
			q    string
			args []any
		}{
			{`DELETE FROM user_sessions WHERE expires_unix<=?`, []any{now.Unix()}},
			{`DELETE FROM auth_tokens WHERE expires_unix<=?`, []any{now.Unix()}},
			{`DELETE FROM user_sessions WHERE user_id IN (` + stale + `)`, []any{unconfirmedBefore.Unix()}},
			{`DELETE FROM auth_tokens WHERE user_id IN (` + stale + `)`, []any{unconfirmedBefore.Unix()}},
			{`DELETE FROM users WHERE confirmed=0 AND created_unix<?`, []any{unconfirmedBefore.Unix()}},
		} {
			res, err := tx.ExecContext(ctx, st.q, st.args...)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			total += n
		}
		return nil
	})
	return total, err
}
