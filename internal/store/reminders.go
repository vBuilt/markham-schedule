package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrReminderExists = errors.New("reminder exists")

// Reminder is one user's request to be emailed shortly before a session's
// Residents registration opens.
type Reminder struct {
	ID, UserID  int64
	SessionID   string
	ResidentsAt time.Time // last Residents registration time seen
	Created     time.Time
	SentAt      *time.Time
	SendError   string
}

// AddReminder returns ErrReminderExists if the user already has a reminder
// for the session.
func (s *Store) AddReminder(ctx context.Context, userID int64, sessionID string, residentsAt, now time.Time) (Reminder, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO reminders(user_id, session_id, residents_unix, created_unix) VALUES(?,?,?,?)`,
		userID, sessionID, residentsAt.Unix(), now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Reminder{}, ErrReminderExists
		}
		return Reminder{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Reminder{}, err
	}
	return Reminder{ID: id, UserID: userID, SessionID: sessionID,
		ResidentsAt: time.Unix(residentsAt.Unix(), 0), Created: time.Unix(now.Unix(), 0)}, nil
}

// removeReminders deletes the reminders matching where, with their cancel
// tokens. It returns the reminders deleted.
func removeReminders(ctx context.Context, tx *sql.Tx, where string, args ...any) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM reminder_cancel_tokens WHERE reminder_id IN (SELECT id FROM reminders WHERE `+where+`)`, args...); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteReminder reports whether a reminder was removed.
func (s *Store) DeleteReminder(ctx context.Context, userID int64, sessionID string) (bool, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		n, err = removeReminders(ctx, tx, `user_id=? AND session_id=?`, userID, sessionID)
		return
	})
	return n > 0, err
}

// DropReminder removes the reminder and its cancel tokens.
func (s *Store) DropReminder(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := removeReminders(ctx, tx, `id=?`, id)
		return err
	})
}

const reminderCols = `id, user_id, session_id, residents_unix, created_unix, sent_unix, send_error`

func (s *Store) queryReminders(ctx context.Context, q string, args ...any) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+reminderCols+` FROM reminders `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reminder{}
	for rows.Next() {
		var r Reminder
		var res, created int64
		var sent sql.NullInt64
		if err := rows.Scan(&r.ID, &r.UserID, &r.SessionID, &res, &created, &sent, &r.SendError); err != nil {
			return nil, err
		}
		r.ResidentsAt, r.Created = time.Unix(res, 0), time.Unix(created, 0)
		if sent.Valid {
			t := time.Unix(sent.Int64, 0)
			r.SentAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserReminders returns the user's pending and sent reminders.
func (s *Store) UserReminders(ctx context.Context, userID int64) ([]Reminder, error) {
	return s.queryReminders(ctx, `WHERE user_id=? ORDER BY residents_unix, id`, userID)
}

func (s *Store) AllReminders(ctx context.Context) ([]Reminder, error) {
	return s.queryReminders(ctx, `ORDER BY residents_unix, id`)
}

// PendingReminders returns reminders not yet sent.
func (s *Store) PendingReminders(ctx context.Context) ([]Reminder, error) {
	return s.queryReminders(ctx, `WHERE sent_unix IS NULL ORDER BY residents_unix, id`)
}

func (s *Store) SetReminderTime(ctx context.Context, id int64, residentsAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET residents_unix=? WHERE id=?`, residentsAt.Unix(), id)
	return err
}

// MarkReminderSent reports true only for the caller that flipped sent_at
// from NULL, so concurrent senders cannot both send.
func (s *Store) MarkReminderSent(ctx context.Context, id int64, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE reminders SET sent_unix=? WHERE id=? AND sent_unix IS NULL`, now.Unix(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) SetReminderError(ctx context.Context, id int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET send_error=? WHERE id=?`, msg, id)
	return err
}

func (s *Store) AddReminderCancelToken(ctx context.Context, reminderID int64, hash []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO reminder_cancel_tokens(token_hash, reminder_id) VALUES(?,?)`, hash, reminderID)
	return err
}

// ReminderSentByCancelToken reports whether the reminder the token belongs to
// has already been sent. found is false if the token is unknown.
func (s *Store) ReminderSentByCancelToken(ctx context.Context, hash []byte) (sent, found bool, err error) {
	var sentUnix sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		`SELECT r.sent_unix FROM reminder_cancel_tokens t JOIN reminders r ON r.id=t.reminder_id WHERE t.token_hash=?`,
		hash).Scan(&sentUnix)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return sentUnix.Valid, true, nil
}

// DeleteReminderByCancelToken removes the reminder the token belongs to. It
// reports false, without error, if the token is unknown (e.g. already used).
func (s *Store) DeleteReminderByCancelToken(ctx context.Context, hash []byte) (bool, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT reminder_id FROM reminder_cancel_tokens WHERE token_hash=?`, hash).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		n, err = removeReminders(ctx, tx, `id=?`, id)
		return err
	})
	return n > 0, err
}

// PurgeReminders removes reminders sent before sentBefore, with their tokens.
// It returns the reminders removed.
func (s *Store) PurgeReminders(ctx context.Context, sentBefore time.Time) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		n, err = removeReminders(ctx, tx, `sent_unix IS NOT NULL AND sent_unix<?`, sentBefore.Unix())
		return
	})
	return n, err
}
