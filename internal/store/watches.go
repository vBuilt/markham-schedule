package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/vBuilt/markham-schedule/internal/schedule"
)

var ErrWatchExists = errors.New("already watching")

// Watch is one user's request to be emailed when a full session opens a spot.
type Watch struct {
	ID           int64
	UserID       int64
	SessionID    string
	SessionStart time.Time
	Created      time.Time
	EmailsSent   int
	LastEmailed  *time.Time
}

// WatchState is the shared per-session polling state.
type WatchState struct {
	SessionID   string
	LastSpots   int
	LastChecked *time.Time
	Misses      int
	LastError   string
}

// SessionByID returns ErrNotFound if the session is not stored.
func (s *Store) SessionByID(ctx context.Context, id string) (schedule.Session, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM sessions WHERE id=?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return schedule.Session{}, ErrNotFound
	}
	if err != nil {
		return schedule.Session{}, err
	}
	ss, err := s.decode(data)
	if err != nil {
		return ss, err
	}
	ss.ID = id
	rows, err := s.db.QueryContext(ctx, `SELECT category FROM session_categories WHERE session_id=? ORDER BY category`, id)
	if err != nil {
		return ss, err
	}
	defer rows.Close()
	ss.Categories = []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return ss, err
		}
		ss.Categories = append(ss.Categories, c)
	}
	return ss, rows.Err()
}

// UpdateSessionSpots rewrites only the spot fields of one stored session.
// left may be nil (count unknown).
func (s *Store) UpdateSessionSpots(ctx context.Context, id, spots string, left *int, at time.Time) error {
	var l any
	if left != nil {
		l = *left
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET data=json_set(data, '$.spots', ?, '$.spots_left', ?, '$.scraped_at', ?) WHERE id=?`,
		spots, l, at.UTC().Format(time.RFC3339Nano), id)
	return err
}

// AddWatch returns ErrWatchExists if the user already watches the session.
func (s *Store) AddWatch(ctx context.Context, userID int64, sessionID string, start, now time.Time) (Watch, error) {
	var w Watch
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO watches(user_id, session_id, session_start_unix, created_unix) VALUES(?,?,?,?)`,
			userID, sessionID, start.Unix(), now.Unix())
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return ErrWatchExists
			}
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// The first watcher starts from a clean state (session treated as
		// full), so a stale leftover row can't hide their first opening.
		var others int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM watches WHERE session_id=? AND id<>?`, sessionID, id).Scan(&others); err != nil {
			return err
		}
		stateSQL := `INSERT OR IGNORE INTO watch_state(session_id, last_spots) VALUES(?,0)`
		if others == 0 {
			stateSQL = `INSERT OR REPLACE INTO watch_state(session_id, last_spots, last_checked_unix, misses, last_error) VALUES(?,0,NULL,0,'')`
		}
		if _, err := tx.ExecContext(ctx, stateSQL, sessionID); err != nil {
			return err
		}
		w = Watch{ID: id, UserID: userID, SessionID: sessionID, SessionStart: time.Unix(start.Unix(), 0), Created: time.Unix(now.Unix(), 0)}
		return nil
	})
	return w, err
}

// removeWatches deletes the watches matching where (with their stop tokens),
// then state rows no watch uses any more. It returns the watches deleted.
func removeWatches(ctx context.Context, tx *sql.Tx, where string, args ...any) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM watch_stop_tokens WHERE watch_id IN (SELECT id FROM watches WHERE `+where+`)`, args...); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM watch_state WHERE session_id NOT IN (SELECT session_id FROM watches)`)
	return n, err
}

// DeleteWatch reports whether a watch was removed.
func (s *Store) DeleteWatch(ctx context.Context, userID int64, sessionID string) (bool, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		n, err = removeWatches(ctx, tx, `user_id=? AND session_id=?`, userID, sessionID)
		return
	})
	return n > 0, err
}

func (s *Store) AddStopToken(ctx context.Context, watchID int64, hash []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO watch_stop_tokens(token_hash, watch_id) VALUES(?,?)`, hash, watchID)
	return err
}

// DeleteWatchByStopToken removes the watch the token belongs to. It reports
// false, without error, if the token is unknown (e.g. already used).
func (s *Store) DeleteWatchByStopToken(ctx context.Context, hash []byte) (bool, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT watch_id FROM watch_stop_tokens WHERE token_hash=?`, hash).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		n, err = removeWatches(ctx, tx, `id=?`, id)
		return err
	})
	return n > 0, err
}

const watchCols = `id, user_id, session_id, session_start_unix, created_unix, emails_sent, last_emailed_unix`

func (s *Store) queryWatches(ctx context.Context, q string, args ...any) ([]Watch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+watchCols+` FROM watches `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Watch{}
	for rows.Next() {
		var w Watch
		var start, created int64
		var last sql.NullInt64
		if err := rows.Scan(&w.ID, &w.UserID, &w.SessionID, &start, &created, &w.EmailsSent, &last); err != nil {
			return nil, err
		}
		w.SessionStart, w.Created = time.Unix(start, 0), time.Unix(created, 0)
		if last.Valid {
			t := time.Unix(last.Int64, 0)
			w.LastEmailed = &t
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) UserWatches(ctx context.Context, userID int64) ([]Watch, error) {
	return s.queryWatches(ctx, `WHERE user_id=? ORDER BY session_start_unix, id`, userID)
}

func (s *Store) AllWatches(ctx context.Context) ([]Watch, error) {
	return s.queryWatches(ctx, `ORDER BY session_start_unix, user_id, id`)
}

func (s *Store) WatchersOf(ctx context.Context, sessionID string) ([]Watch, error) {
	return s.queryWatches(ctx, `WHERE session_id=? ORDER BY user_id, id`, sessionID)
}

func (s *Store) MarkEmailed(ctx context.Context, watchID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE watches SET emails_sent=emails_sent+1, last_emailed_unix=? WHERE id=?`, now.Unix(), watchID)
	return err
}

// WatchStates returns the polling state of every session with at least one watch.
func (s *Store) WatchStates(ctx context.Context) (map[string]WatchState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, last_spots, last_checked_unix, misses, last_error FROM watch_state
		 WHERE session_id IN (SELECT session_id FROM watches)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WatchState{}
	for rows.Next() {
		var st WatchState
		var chk sql.NullInt64
		if err := rows.Scan(&st.SessionID, &st.LastSpots, &chk, &st.Misses, &st.LastError); err != nil {
			return nil, err
		}
		if chk.Valid {
			t := time.Unix(chk.Int64, 0)
			st.LastChecked = &t
		}
		out[st.SessionID] = st
	}
	return out, rows.Err()
}

func (s *Store) SaveWatchState(ctx context.Context, st WatchState) error {
	var chk any
	if st.LastChecked != nil {
		chk = st.LastChecked.Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO watch_state(session_id, last_spots, last_checked_unix, misses, last_error) VALUES(?,?,?,?,?)
		 ON CONFLICT(session_id) DO UPDATE SET last_spots=excluded.last_spots,
		   last_checked_unix=excluded.last_checked_unix, misses=excluded.misses, last_error=excluded.last_error`,
		st.SessionID, st.LastSpots, chk, st.Misses, st.LastError)
	return err
}

// DeleteSessionWatches removes every watch on the session, with their stop
// tokens and polling state.
func (s *Store) DeleteSessionWatches(ctx context.Context, sessionID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := removeWatches(ctx, tx, `session_id=?`, sessionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM watch_state WHERE session_id=?`, sessionID)
		return err
	})
}

// PurgeWatches removes watches whose session has started by now, and any stop
// tokens or state left without a watch. It returns the watches removed.
func (s *Store) PurgeWatches(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		if n, err = removeWatches(ctx, tx, `session_start_unix<=?`, now.Unix()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM watch_stop_tokens WHERE watch_id NOT IN (SELECT id FROM watches)`)
		return err
	})
	return n, err
}
