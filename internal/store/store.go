// Package store persists sessions in SQLite with a many-to-many
// session<->category link.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"

	_ "modernc.org/sqlite"

	"github.com/vBuilt/markham-schedule/internal/schedule"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions(
  id TEXT PRIMARY KEY, start_unix INTEGER NOT NULL, end_unix INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_start ON sessions(start_unix);
CREATE INDEX IF NOT EXISTS sessions_end ON sessions(end_unix);
CREATE TABLE IF NOT EXISTS session_categories(
  session_id TEXT NOT NULL, category TEXT NOT NULL, PRIMARY KEY(session_id, category));
CREATE INDEX IF NOT EXISTS session_categories_cat ON session_categories(category);
CREATE TABLE IF NOT EXISTS status(id INTEGER PRIMARY KEY CHECK(id=1), data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS reg_rules(event_id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS runs(id INTEGER PRIMARY KEY AUTOINCREMENT, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users(
  id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
  confirmed INTEGER NOT NULL DEFAULT 0, created_unix INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS user_sessions(
  token_hash BLOB PRIMARY KEY, user_id INTEGER NOT NULL, expires_unix INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS user_sessions_user ON user_sessions(user_id);
CREATE TABLE IF NOT EXISTS auth_tokens(
  token_hash BLOB PRIMARY KEY, kind TEXT NOT NULL, user_id INTEGER NOT NULL, expires_unix INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS auth_tokens_user ON auth_tokens(user_id);
`

type Store struct {
	db  *sql.DB
	loc *time.Location
}

type CategoryCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type ActivityCount struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Count    int    `json:"count"`
}

type CentreCount struct {
	Name    string           `json:"name"`
	Address schedule.Address `json:"address"`
	Count   int              `json:"count"`
}

func Open(path string) (*Store, error) {
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", schema} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("init sqlite: %w", err)
		}
	}
	// Columns added after a table first shipped; re-running is a no-op.
	for _, q := range []string{`ALTER TABLE users ADD COLUMN name TEXT NOT NULL DEFAULT ''`} {
		if _, err := db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	return &Store{db: db, loc: loc}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

const deleteOrphans = `DELETE FROM sessions WHERE id NOT IN (SELECT session_id FROM session_categories)`

// ReplaceCategory makes sessions the category's set of sessions starting at or
// after since. Links to sessions starting before since are left alone (Prune
// removes them once they age past retention), so fetching from today never
// deletes earlier sessions.
func (s *Store) ReplaceCategory(ctx context.Context, category string, since time.Time, sessions []schedule.Session) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM session_categories WHERE category=? AND session_id IN (SELECT id FROM sessions WHERE start_unix >= ?)`,
			category, since.Unix()); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, ss := range sessions {
			if seen[ss.ID] {
				continue
			}
			seen[ss.ID] = true
			ss.Categories = nil
			data, err := json.Marshal(ss)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO sessions(id,start_unix,end_unix,data) VALUES(?,?,?,?)
				 ON CONFLICT(id) DO UPDATE SET start_unix=excluded.start_unix, end_unix=excluded.end_unix, data=excluded.data`,
				ss.ID, ss.Start.Unix(), ss.End.Unix(), string(data)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO session_categories(session_id,category) VALUES(?,?)`, ss.ID, category); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, deleteOrphans)
		return err
	})
}

func (s *Store) RemoveCategoriesExcept(ctx context.Context, keep []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT category FROM session_categories`)
		if err != nil {
			return err
		}
		keepSet := map[string]bool{}
		for _, k := range keep {
			keepSet[k] = true
		}
		var drop []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return err
			}
			if !keepSet[c] {
				drop = append(drop, c)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range drop {
			if _, err := tx.ExecContext(ctx, `DELETE FROM session_categories WHERE category=?`, c); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, deleteOrphans)
		return err
	})
}

func (s *Store) Prune(ctx context.Context, endedBefore time.Time) (int, error) {
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM session_categories WHERE session_id IN (SELECT id FROM sessions WHERE end_unix < ?)`,
			endedBefore.Unix()); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE end_unix < ?`, endedBefore.Unix())
		if err != nil {
			return err
		}
		c, err := res.RowsAffected()
		n = int(c)
		return err
	})
	return n, err
}

func (s *Store) decode(data string) (schedule.Session, error) {
	var ss schedule.Session
	if err := json.Unmarshal([]byte(data), &ss); err != nil {
		return ss, err
	}
	ss.Start, ss.End = ss.Start.In(s.loc), ss.End.In(s.loc)
	if !ss.ScrapedAt.IsZero() {
		ss.ScrapedAt = ss.ScrapedAt.In(s.loc)
	}
	return ss, nil
}

func (s *Store) Sessions(ctx context.Context, f schedule.Filter) ([]schedule.Session, error) {
	q := `SELECT id, data FROM sessions WHERE 1=1`
	var args []any
	if !f.From.IsZero() {
		q += ` AND start_unix >= ?`
		args = append(args, f.From.Unix())
	}
	if !f.To.IsZero() {
		q += ` AND start_unix < ?`
		args = append(args, f.To.Unix())
	}
	q += ` ORDER BY start_unix, json_extract(data,'$.name'), id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	type rec struct {
		id   string
		data string
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.data); err != nil {
			rows.Close()
			return nil, err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	cats := map[string][]string{}
	crows, err := s.db.QueryContext(ctx, `SELECT session_id, category FROM session_categories ORDER BY category`)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var id, c string
		if err := crows.Scan(&id, &c); err != nil {
			crows.Close()
			return nil, err
		}
		cats[id] = append(cats[id], c)
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return nil, err
	}

	out := []schedule.Session{}
	for _, r := range recs {
		ss, err := s.decode(r.data)
		if err != nil {
			return nil, err
		}
		ss.ID = r.id
		ss.Categories = cats[r.id]
		if ss.Categories == nil {
			ss.Categories = []string{}
		}
		if !f.Matches(ss) {
			continue
		}
		out = append(out, ss)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

func (s *Store) Categories(ctx context.Context) ([]CategoryCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT category, COUNT(*) FROM session_categories GROUP BY category ORDER BY category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CategoryCount{}
	for rows.Next() {
		var c CategoryCount
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Activities lists the distinct session names within each category, sorted by
// category then name. A session in two categories is counted in both.
func (s *Store) Activities(ctx context.Context) ([]ActivityCount, error) {
	all, err := s.Sessions(ctx, schedule.Filter{})
	if err != nil {
		return nil, err
	}
	type key struct{ cat, name string }
	m := map[key]int{}
	for _, ss := range all {
		for _, c := range ss.Categories {
			m[key{c, ss.Name}]++
		}
	}
	out := make([]ActivityCount, 0, len(m))
	for k, n := range m {
		out = append(out, ActivityCount{Category: k.cat, Name: k.name, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Store) Centres(ctx context.Context) ([]CentreCount, error) {
	all, err := s.Sessions(ctx, schedule.Filter{})
	if err != nil {
		return nil, err
	}
	m := map[string]*CentreCount{}
	for _, ss := range all { // ascending by start, so the last address seen is the most recent
		if ss.Centre == "" {
			continue
		}
		c := m[ss.Centre]
		if c == nil {
			c = &CentreCount{Name: ss.Centre}
			m[ss.Centre] = c
		}
		c.Count++
		c.Address = ss.Address
	}
	out := make([]CentreCount, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) SaveStatus(ctx context.Context, st schedule.Status) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO status(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, string(data))
	return err
}

func (s *Store) LoadStatus(ctx context.Context) (schedule.Status, error) {
	var st schedule.Status
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM status WHERE id=1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	err = json.Unmarshal([]byte(data), &st)
	return st, err
}

// RegRules returns every saved registration rule, keyed by PerfectMind EventId.
func (s *Store) RegRules(ctx context.Context) (map[string]schedule.RegRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, data FROM reg_rules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]schedule.RegRule{}
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var r schedule.RegRule
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			return nil, fmt.Errorf("reg rule %s: %w", id, err)
		}
		out[id] = r
	}
	return out, rows.Err()
}

// SaveRegRule inserts or replaces the registration rule for one EventId.
func (s *Store) SaveRegRule(ctx context.Context, eventID string, r schedule.RegRule) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO reg_rules(event_id, data) VALUES(?, ?) ON CONFLICT(event_id) DO UPDATE SET data=excluded.data`,
		eventID, string(data))
	return err
}

// maxRuns is how many refresh records are kept for the status page.
const maxRuns = 300

// AddRun records one refresh attempt and trims history to maxRuns.
func (s *Store) AddRun(ctx context.Context, r schedule.Run) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs(data) VALUES(?)`, string(data)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE id <= (SELECT MAX(id) FROM runs) - ?`, maxRuns)
		return err
	})
}

// Runs returns the most recent refresh records, newest first. limit <= 0 means all kept.
func (s *Store) Runs(ctx context.Context, limit int) ([]schedule.Run, error) {
	if limit <= 0 {
		limit = maxRuns
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []schedule.Run{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var r schedule.Run
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
