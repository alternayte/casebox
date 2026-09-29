// Package spool is the local SQLite queue between capture and upload. It never deletes an event
// the server has not acknowledged. Above its size cap it drops the oldest acknowledged events.
package spool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // the pure-Go SQLite driver, so the binary needs no CGO

	"github.com/alternayte/casebox/cli/internal/capture"
)

// DefaultCap is the spool's size cap.
const DefaultCap = 500 << 20

// Spool is one open spool database.
type Spool struct {
	db  *sql.DB
	cap int64
}

// Open opens or creates the spool at path. Several processes (hooks of parallel agent sessions)
// may use it at once.
func Open(path string, capBytes int64) (*Spool, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`PRAGMA auto_vacuum = INCREMENTAL`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			meta TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			session_id TEXT NOT NULL REFERENCES sessions(id),
			seq INTEGER NOT NULL,
			payload TEXT NOT NULL,
			size INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			acked_at INTEGER,
			rejected TEXT,
			PRIMARY KEY (session_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS events_pending ON events (session_id, seq) WHERE acked_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS events_acked ON events (acked_at) WHERE acked_at IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("prepare the spool: %w", err)
		}
	}
	return &Spool{db: db, cap: capBytes}, nil
}

// Close closes the database.
func (s *Spool) Close() error { return s.db.Close() }

// Add stores a session's metadata and events. An event already in the spool is kept as it is,
// so importing the same history twice adds nothing. The metadata merges with what the spool
// already knows, because the import and the hooks each see part of a session.
func (s *Spool) Add(ctx context.Context, session capture.Session, events []capture.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.upsertSession(ctx, tx, session); err != nil {
		return err
	}
	if err := insertEvents(ctx, tx, session.ID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.enforceCap(ctx)
}

// AddHook stores one hook event with the session's next hook sequence number.
func (s *Spool) AddHook(ctx context.Context, session capture.Session, e capture.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.upsertSession(ctx, tx, session); err != nil {
		return err
	}
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq) + 1, ?) FROM events WHERE session_id = ? AND seq >= ?`,
		capture.HookSeqBase, session.ID, capture.HookSeqBase).Scan(&next); err != nil {
		return err
	}
	e.Seq = next
	if err := insertEvents(ctx, tx, session.ID, []capture.Event{e}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.enforceCap(ctx)
}

func (s *Spool) upsertSession(ctx context.Context, tx *sql.Tx, session capture.Session) error {
	var existing string
	switch err := tx.QueryRowContext(ctx, `SELECT meta FROM sessions WHERE id = ?`, session.ID).Scan(&existing); {
	case err == nil:
		var old capture.Session
		if json.Unmarshal([]byte(existing), &old) == nil {
			session = merge(old, session)
		}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	meta, err := json.Marshal(session)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions (id, meta, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET meta = excluded.meta, updated_at = excluded.updated_at`,
		session.ID, string(meta), time.Now().Unix())
	return err
}

// merge keeps the earliest start, the first HEAD, the latest end and HEAD, and every field
// only one side knows.
func merge(old, s capture.Session) capture.Session {
	pick := func(a, b string) string {
		if b != "" {
			return b
		}
		return a
	}
	out := old
	out.AgentVersion = pick(old.AgentVersion, s.AgentVersion)
	out.Model = pick(old.Model, s.Model)
	out.Repo = pick(s.Repo, old.Repo)
	out.Branch = pick(s.Branch, old.Branch)
	out.HeadStart = pick(s.HeadStart, old.HeadStart)
	out.HeadEnd = pick(old.HeadEnd, s.HeadEnd)
	out.WorkItem = pick(old.WorkItem, s.WorkItem)
	out.Person = pick(old.Person, s.Person)
	if old.Source == "" {
		out.Source = s.Source
	}
	if out.StartedAt.IsZero() || (!s.StartedAt.IsZero() && s.StartedAt.Before(out.StartedAt)) {
		out.StartedAt = s.StartedAt
	}
	if s.EndedAt != nil && (out.EndedAt == nil || s.EndedAt.After(*out.EndedAt)) {
		out.EndedAt = s.EndedAt
	}
	return out
}

// State reads a small value the hooks keep between calls.
func (s *Spool) State(ctx context.Context, key string, into any) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM state WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(value), into)
}

// SetState writes a value the hooks keep between calls.
func (s *Spool) SetState(ctx context.Context, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO state (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(data), time.Now().Unix())
	return err
}

func insertEvents(ctx context.Context, tx *sql.Tx, sessionID string, events []capture.Event) error {
	now := time.Now().Unix()
	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (session_id, seq, payload, size, created_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (session_id, seq) DO NOTHING`,
			sessionID, e.Seq, string(payload), len(payload), now); err != nil {
			return err
		}
	}
	return nil
}

// Batch is a session's metadata with some of its unacknowledged events, in order.
type Batch struct {
	Session capture.Session `json:"session"`
	Events  []capture.Event `json:"events"`
}

// Pending returns up to maxBatches batches of unacknowledged events, each at most maxEvents
// events and about maxBytes bytes.
func (s *Spool) Pending(ctx context.Context, maxBatches, maxEvents int, maxBytes int64) ([]Batch, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT e.session_id, s.meta FROM events e JOIN sessions s ON s.id = e.session_id
		 WHERE e.acked_at IS NULL ORDER BY e.session_id LIMIT ?`, maxBatches)
	if err != nil {
		return nil, err
	}
	type pending struct{ id, meta string }
	var sessions []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.meta); err != nil {
			rows.Close()
			return nil, err
		}
		sessions = append(sessions, p)
	}
	rows.Close()

	var batches []Batch
	for _, p := range sessions {
		var b Batch
		if err := json.Unmarshal([]byte(p.meta), &b.Session); err != nil {
			return nil, err
		}
		events, err := s.db.QueryContext(ctx,
			`SELECT payload, size FROM events WHERE session_id = ? AND acked_at IS NULL ORDER BY seq LIMIT ?`, p.id, maxEvents)
		if err != nil {
			return nil, err
		}
		var total int64
		for events.Next() {
			var payload string
			var size int64
			if err := events.Scan(&payload, &size); err != nil {
				events.Close()
				return nil, err
			}
			if len(b.Events) > 0 && total+size > maxBytes {
				break
			}
			var e capture.Event
			if err := json.Unmarshal([]byte(payload), &e); err != nil {
				events.Close()
				return nil, err
			}
			b.Events = append(b.Events, e)
			total += size
		}
		events.Close()
		batches = append(batches, b)
	}
	return batches, nil
}

// Ack marks events as acknowledged by the server.
func (s *Spool) Ack(ctx context.Context, sessionID string, seqs []int64) error {
	return s.mark(ctx, sessionID, seqs, "")
}

// Reject marks events the server refused for good, with the reason, so they stop blocking the
// upload. doctor shows them.
func (s *Spool) Reject(ctx context.Context, sessionID string, seqs []int64, reason string) error {
	if reason == "" {
		return errors.New("a rejection needs a reason")
	}
	return s.mark(ctx, sessionID, seqs, reason)
}

func (s *Spool) mark(ctx context.Context, sessionID string, seqs []int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, seq := range seqs {
		var rejected any
		if reason != "" {
			rejected = reason
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET acked_at = ?, rejected = ? WHERE session_id = ? AND seq = ?`,
			now, rejected, sessionID, seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Stats is what doctor shows.
type Stats struct {
	Sessions  int64
	Pending   int64
	Acked     int64
	Rejected  int64
	SizeBytes int64
}

// Stats counts the spool's contents.
func (s *Spool) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM sessions),
		        (SELECT count(*) FROM events WHERE acked_at IS NULL),
		        (SELECT count(*) FROM events WHERE acked_at IS NOT NULL AND rejected IS NULL),
		        (SELECT count(*) FROM events WHERE rejected IS NOT NULL)`).
		Scan(&st.Sessions, &st.Pending, &st.Acked, &st.Rejected)
	if err != nil {
		return st, err
	}
	st.SizeBytes, err = s.size(ctx)
	return st, err
}

func (s *Spool) size(ctx context.Context) (int64, error) {
	var pages, free, pageSize int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return (pages - free) * pageSize, nil
}

// enforceCap drops the oldest acknowledged events while the spool is above its cap. It never
// touches an unacknowledged event.
func (s *Spool) enforceCap(ctx context.Context) error {
	for {
		size, err := s.size(ctx)
		if err != nil || size <= s.cap {
			return err
		}
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM events WHERE rowid IN (SELECT rowid FROM events WHERE acked_at IS NOT NULL ORDER BY acked_at, rowid LIMIT 1000)`)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id NOT IN (SELECT DISTINCT session_id FROM events)`); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return err
		}
	}
}
