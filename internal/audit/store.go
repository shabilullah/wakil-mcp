// Package audit records every tool call the proxy handles.
//
// Writes are buffered and flushed by a background goroutine, so a slow disk
// never delays a tool call. If the buffer fills, events are dropped and
// counted rather than blocking the caller.
package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, so released binaries need no cgo
)

// Status values recorded for a tool call.
const (
	StatusOK     = "ok"
	StatusError  = "error"
	StatusDenied = "denied"
)

// Event is one recorded tool call.
type Event struct {
	ID           int64     `json:"id"`
	Time         time.Time `json:"time"`
	Client       string    `json:"client"`
	Server       string    `json:"server"`
	Tool         string    `json:"tool"`
	UpstreamTool string    `json:"upstreamTool,omitempty"`
	Status       string    `json:"status"`
	Error        string    `json:"error,omitempty"`
	DurationMS   int64     `json:"durationMs"`
}

// Filter selects events for a query. Zero values mean "no constraint".
type Filter struct {
	Client string
	Server string
	Tool   string
	Status string
	Since  time.Time
	Until  time.Time
	Limit  int
	Offset int
}

// Stats summarises the recorded history.
type Stats struct {
	Total    int64            `json:"total"`
	ByStatus map[string]int64 `json:"byStatus"`
	ByServer map[string]int64 `json:"byServer"`
	Dropped  int64            `json:"dropped"`
	OldestAt time.Time        `json:"oldestAt,omitempty"`
	NewestAt time.Time        `json:"newestAt,omitempty"`
}

// Recorder accepts audit events. A nil Recorder is valid and discards them.
type Recorder interface {
	Record(e *Event)
}

// NopRecorder discards everything.
type NopRecorder struct{}

// Record implements Recorder.
func (NopRecorder) Record(*Event) {}

// bufferSize bounds how many events may be queued before writes are dropped.
const bufferSize = 1024

// flushInterval is how long to wait for more events before writing.
const flushInterval = 250 * time.Millisecond

// Store is the SQLite-backed audit log.
type Store struct {
	db      *sql.DB
	events  chan *Event
	done    chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Int64
	closeMu sync.Once
}

// Open creates or opens the audit database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating audit directory: %w", err)
		}
	}

	// _pragma settings are per-connection and set through the DSN.
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening audit database: %w", err)
	}
	// SQLite tolerates a single writer well; more just adds lock contention.
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{
		db:     db,
		events: make(chan *Event, bufferSize),
		done:   make(chan struct{}),
	}
	s.wg.Add(1)
	go s.writeLoop()
	return s, nil
}

// migrate creates the schema if it is missing.
func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS tool_calls (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL,
    client        TEXT    NOT NULL,
    server        TEXT    NOT NULL,
    tool          TEXT    NOT NULL,
    upstream_tool TEXT    NOT NULL DEFAULT '',
    status        TEXT    NOT NULL,
    error         TEXT    NOT NULL DEFAULT '',
    duration_ms   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tool_calls_ts     ON tool_calls (ts DESC);
CREATE INDEX IF NOT EXISTS idx_tool_calls_server ON tool_calls (server, ts DESC);
CREATE INDEX IF NOT EXISTS idx_tool_calls_client ON tool_calls (client, ts DESC);
CREATE INDEX IF NOT EXISTS idx_tool_calls_status ON tool_calls (status, ts DESC);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("creating audit schema: %w", err)
	}
	return nil
}

// Record queues an event. It never blocks; if the queue is full the event is
// dropped and the dropped counter increments, which surfaces in Stats.
func (s *Store) Record(e *Event) {
	if e == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	select {
	case s.events <- e:
	case <-s.done:
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports how many events have been discarded due to back pressure.
func (s *Store) Dropped() int64 { return s.dropped.Load() }

// writeLoop drains the queue into SQLite in batches.
func (s *Store) writeLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	batch := make([]*Event, 0, 64)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insertBatch(batch); err != nil {
			// Losing audit rows is bad but must not take the proxy down.
			s.dropped.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-s.done:
			// Drain whatever is queued, then flush.
			for {
				select {
				case e := <-s.events:
					batch = append(batch, e)
					if len(batch) >= 64 {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case e := <-s.events:
			batch = append(batch, e)
			if len(batch) >= 64 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// insertBatch writes a batch inside one transaction.
func (s *Store) insertBatch(batch []*Event) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	stmt, err := tx.Prepare(`
INSERT INTO tool_calls (ts, client, server, tool, upstream_tool, status, error, duration_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range batch {
		if _, err := stmt.Exec(
			e.Time.UnixMilli(),
			e.Client, e.Server, e.Tool, e.UpstreamTool,
			e.Status, truncate(e.Error, 2000), e.DurationMS,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// truncate caps a string so one pathological error cannot bloat the database.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Query returns events matching the filter, newest first.
func (s *Store) Query(ctx context.Context, f Filter) ([]Event, error) {
	q := `SELECT id, ts, client, server, tool, upstream_tool, status, error, duration_ms
	        FROM tool_calls WHERE 1=1`
	var args []any

	if f.Client != "" {
		q += " AND client = ?"
		args = append(args, f.Client)
	}
	if f.Server != "" {
		q += " AND server = ?"
		args = append(args, f.Server)
	}
	if f.Tool != "" {
		q += " AND tool LIKE ?"
		args = append(args, "%"+f.Tool+"%")
	}
	if f.Status != "" {
		q += " AND status = ?"
		args = append(args, f.Status)
	}
	if !f.Since.IsZero() {
		q += " AND ts >= ?"
		args = append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		q += " AND ts <= ?"
		args = append(args, f.Until.UnixMilli())
	}
	q += " ORDER BY ts DESC, id DESC"

	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q += " LIMIT ? OFFSET ?"
	args = append(args, limit, max(f.Offset, 0))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying audit log: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			e  Event
			ms int64
		)
		if err := rows.Scan(&e.ID, &ms, &e.Client, &e.Server, &e.Tool,
			&e.UpstreamTool, &e.Status, &e.Error, &e.DurationMS); err != nil {
			return nil, fmt.Errorf("scanning audit row: %w", err)
		}
		e.Time = time.UnixMilli(ms)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Stats summarises the log.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{
		ByStatus: map[string]int64{},
		ByServer: map[string]int64{},
		Dropped:  s.dropped.Load(),
	}

	var oldest, newest int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(ts), 0), COALESCE(MAX(ts), 0) FROM tool_calls`,
	).Scan(&st.Total, &oldest, &newest); err != nil {
		return st, err
	}
	if oldest > 0 {
		st.OldestAt = time.UnixMilli(oldest)
	}
	if newest > 0 {
		st.NewestAt = time.UnixMilli(newest)
	}

	if err := s.scanCounts(ctx, `SELECT status, COUNT(*) FROM tool_calls GROUP BY status`, st.ByStatus); err != nil {
		return st, err
	}
	if err := s.scanCounts(ctx, `SELECT server, COUNT(*) FROM tool_calls GROUP BY server`, st.ByServer); err != nil {
		return st, err
	}
	return st, nil
}

// scanCounts fills a map from a two-column grouping query.
func (s *Store) scanCounts(ctx context.Context, q string, dst map[string]int64) error {
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k string
			n int64
		)
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		dst[k] = n
	}
	return rows.Err()
}

// Prune deletes events older than the cutoff, returning how many were removed.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tool_calls WHERE ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Close flushes pending events and closes the database.
func (s *Store) Close() error {
	var err error
	s.closeMu.Do(func() {
		close(s.done)
		s.wg.Wait()
		err = s.db.Close()
	})
	if err != nil && !errors.Is(err, sql.ErrConnDone) {
		return err
	}
	return nil
}
