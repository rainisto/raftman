// Package pgstore persists log entries in a PostgreSQL database with full text
// search, as a drop-in alternative to the embedded SQLite store for large
// volumes. It satisfies the same server.Store contract.
//
// Entries live in a single logs table; full text search uses a generated
// tsvector column with a GIN index, and timestamps are stored in UTC so range
// filters and ordering stay consistent across senders.
package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pierredavidbelanger/raftman/api"
	"github.com/pierredavidbelanger/raftman/internal/store"
)

type Config struct {
	DSN             string          // libpq/pgx connection string
	InsertQueueSize int             // entries buffered between Insert and the writer
	QueryQueueSize  int             // queries allowed to run concurrently
	Timeout         time.Duration   // deadline for one query
	BatchSize       int             // extra entries the writer adds to a transaction when they are already queued
	Retention       store.Retention // entries older than this are purged hourly
}

// Store owns one writer goroutine that commits queued entries in batches and
// purges old ones. Queries run concurrently on the connection pool.
type Store struct {
	cfg     Config
	db      *sql.DB
	inserts chan *api.LogEntry
	done    chan struct{}

	mu     sync.RWMutex // guards closed
	closed bool
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS logs (
		id      BIGSERIAL PRIMARY KEY,
		ts      TIMESTAMPTZ NOT NULL,
		host    VARCHAR(255),
		app     VARCHAR(255),
		msg     TEXT,
		msg_tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', coalesce(msg, ''))) STORED
	)`,
	`CREATE INDEX IF NOT EXISTS logs_ts_idx ON logs (ts DESC)`,
	`CREATE INDEX IF NOT EXISTS logs_host_app_idx ON logs (host, app, ts DESC)`,
	`CREATE INDEX IF NOT EXISTS logs_msg_tsv_idx ON logs USING GIN (msg_tsv)`,
}

func Open(cfg Config) (*Store, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("Invalid PostgreSQL DSN ''")
	}
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.QueryQueueSize + 1) // +1 for the writer
	for _, ddl := range schema {
		if _, err := db.Exec(ddl); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{
		cfg:     cfg,
		db:      db,
		inserts: make(chan *api.LogEntry, cfg.InsertQueueSize),
		done:    make(chan struct{}),
	}
	go s.writer()
	return s, nil
}

// Insert queues an entry. It blocks while the queue is full and drops the
// entry once the store is closed.
func (s *Store) Insert(e *api.LogEntry) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	s.inserts <- e
}

// Close stops accepting entries, writes everything still queued, and closes
// the database.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.inserts)
	s.mu.Unlock()
	<-s.done
	return s.db.Close()
}

func (s *Store) writer() {
	defer close(s.done)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case e, ok := <-s.inserts:
			if !ok {
				return
			}
			s.write(s.batch(e))
		case now := <-ticker.C:
			s.purge(now)
		}
	}
}

// batch returns e plus up to BatchSize entries that are already queued.
func (s *Store) batch(e *api.LogEntry) []*api.LogEntry {
	entries := []*api.LogEntry{e}
	for len(entries) <= s.cfg.BatchSize {
		select {
		case e, ok := <-s.inserts:
			if !ok {
				return entries
			}
			entries = append(entries, e)
		default:
			return entries
		}
	}
	return entries
}

func (s *Store) write(entries []*api.LogEntry) {
	err := s.tx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare("INSERT INTO logs (ts, host, app, msg) VALUES ($1, $2, $3, $4)")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, e := range entries {
			// Stored in UTC so ordering and range filters stay right across senders.
			if _, err := stmt.Exec(e.Timestamp.UTC(), e.Hostname, e.Application, e.Message); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("Unable to insert %d entries: %s", len(entries), err)
	}
}

func (s *Store) purge(now time.Time) {
	if s.cfg.Retention < 0 {
		return
	}
	upto := now.Add(-time.Duration(s.cfg.Retention))
	if _, err := s.db.Exec("DELETE FROM logs WHERE ts < $1", upto.UTC()); err != nil {
		log.Printf("Unable to purge entries older than %s: %s", upto, err)
	}
}

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// QueryStat counts entries per hostname and application. An SQL error is
// reported in the response; only a timeout is returned as an error.
func (s *Store) QueryStat(ctx context.Context, req *api.QueryRequest) (*api.QueryStatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	res := &api.QueryStatResponse{}
	where, args := where(req)
	n := len(args)
	q := "SELECT host, app, COUNT(*) " + where +
		" GROUP BY host, app ORDER BY host, app" +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", n+1, n+2)
	rows, err := s.db.QueryContext(ctx, q, append(args, limit(req), offset(req))...)
	if err != nil {
		res.Error, err = s.fail(ctx, err)
		return res, err
	}
	defer rows.Close()
	stat := make(map[string]map[string]uint64)
	for rows.Next() {
		var host, app string
		var count uint64
		if err := rows.Scan(&host, &app, &count); err != nil {
			res.Error, err = s.fail(ctx, err)
			return res, err
		}
		if stat[host] == nil {
			stat[host] = make(map[string]uint64)
		}
		stat[host][app] = count
	}
	if err := rows.Err(); err != nil {
		res.Error, err = s.fail(ctx, err)
		return res, err
	}
	res.Stat = stat
	return res, nil
}

// QueryList returns matching entries, newest first. An SQL error is reported
// in the response; only a timeout is returned as an error.
func (s *Store) QueryList(ctx context.Context, req *api.QueryRequest) (*api.QueryListResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	res := &api.QueryListResponse{}
	where, args := where(req)
	n := len(args)
	q := "SELECT ts, host, app, msg " + where +
		" ORDER BY ts DESC" +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", n+1, n+2)
	rows, err := s.db.QueryContext(ctx, q, append(args, limit(req), offset(req))...)
	if err != nil {
		res.Error, err = s.fail(ctx, err)
		return res, err
	}
	defer rows.Close()
	var entries []*api.LogEntry
	for rows.Next() {
		e := &api.LogEntry{}
		if err := rows.Scan(&e.Timestamp, &e.Hostname, &e.Application, &e.Message); err != nil {
			res.Error, err = s.fail(ctx, err)
			return res, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		res.Error, err = s.fail(ctx, err)
		return res, err
	}
	res.Entries = entries
	return res, nil
}

// Ping runs a trivial query to check the database is usable.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// fail sorts a query failure: a timeout becomes the returned error, anything
// else becomes the message reported in the response.
func (s *Store) fail(ctx context.Context, err error) (string, error) {
	if ctx.Err() != nil {
		return "", fmt.Errorf("operation timed out after %s", s.cfg.Timeout)
	}
	return err.Error(), nil
}

// where builds the shared filter clause with $N placeholders. The full text
// match uses websearch_to_tsquery, which tolerates arbitrary user input.
func where(req *api.QueryRequest) (string, []any) {
	var b strings.Builder
	var args []any
	b.WriteString("FROM logs WHERE 1=1")
	if !req.FromTimestamp.IsZero() {
		fmt.Fprintf(&b, " AND ts >= $%d", len(args)+1)
		args = append(args, req.FromTimestamp.UTC())
	}
	if !req.ToTimestamp.IsZero() {
		fmt.Fprintf(&b, " AND ts < $%d", len(args)+1)
		args = append(args, req.ToTimestamp.UTC())
	}
	if req.Hostname != "" {
		fmt.Fprintf(&b, " AND host = $%d", len(args)+1)
		args = append(args, req.Hostname)
	}
	if req.Application != "" {
		fmt.Fprintf(&b, " AND app = $%d", len(args)+1)
		args = append(args, req.Application)
	}
	if req.Message != "" {
		fmt.Fprintf(&b, " AND msg_tsv @@ websearch_to_tsquery('simple', $%d)", len(args)+1)
		args = append(args, req.Message)
	}
	return b.String(), args
}

func limit(req *api.QueryRequest) int  { return min(max(req.Limit, 0), 256) }
func offset(req *api.QueryRequest) int { return min(max(req.Offset, 0), math.MaxInt16) }
