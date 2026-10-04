package backend

import (
	"bytes"
	"database/sql"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pierredavidbelanger/raftman/api"
	"github.com/pierredavidbelanger/raftman/utils"
	"log"
	"math"
	"net/url"
	"sync"
	"time"
)

type postgresBackend struct {
	asyncBackend
	batchSize int
	retention utils.Retention
	dsn       string
	db        *sql.DB
	iStmt     *sql.Stmt
}

// raftman-specific query params that must not leak into the libpq/pgx DSN.
var pgReservedParams = []string{"retention", "batchSize", "insertQueueSize", "queryQueueSize", "timeout"}

func newPostgresBackend(backendURL *url.URL) (*postgresBackend, error) {

	b := postgresBackend{}
	err := initAsyncBackend(backendURL, &b.asyncBackend)
	if err != nil {
		return nil, err
	}

	batchSize, err := utils.GetIntQueryParam(backendURL, "batchSize", 32)
	if err != nil {
		return nil, err
	}
	b.batchSize = batchSize

	retention, err := utils.GetRetentionQueryParam(backendURL, "retention", utils.INF)
	if err != nil {
		return nil, err
	}
	b.retention = retention

	// Strip raftman's own params, keep the rest (sslmode, connect_timeout, ...).
	cleaned := *backendURL
	q := cleaned.Query()
	for _, k := range pgReservedParams {
		q.Del(k)
	}
	cleaned.RawQuery = q.Encode()
	b.dsn = cleaned.String()

	return &b, nil
}

func (b *postgresBackend) Start() error {

	db, err := sql.Open("pgx", b.dsn)
	if err != nil {
		return err
	}
	b.db = db

	if err = db.Ping(); err != nil {
		db.Close()
		return err
	}

	schema := []string{
		`CREATE TABLE IF NOT EXISTS logs (
			id      BIGSERIAL PRIMARY KEY,
			ts      TIMESTAMPTZ NOT NULL,
			host    VARCHAR(255),
			app     VARCHAR(255),
			msg     TEXT,
			msg_tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', coalesce(msg, ''))) STORED
		)`,
		`CREATE INDEX IF NOT EXISTS logs_ts_idx ON logs (ts DESC)`,
		`CREATE INDEX IF NOT EXISTS logs_haa_idx ON logs (host, app, ts DESC)`,
		`CREATE INDEX IF NOT EXISTS logs_tsv_idx ON logs USING GIN (msg_tsv)`,
	}
	for _, ddl := range schema {
		if _, err = db.Exec(ddl); err != nil {
			db.Close()
			return err
		}
	}

	iStmt, err := db.Prepare("INSERT INTO logs (ts, host, app, msg) VALUES ($1, $2, $3, $4)")
	if err != nil {
		db.Close()
		return err
	}
	b.iStmt = iStmt

	go b.run()

	return nil
}

func (b *postgresBackend) Close() error {

	cond := sync.NewCond(&sync.Mutex{})
	cond.L.Lock()
	b.stopQ <- cond
	cond.Wait()
	cond.L.Unlock()

	if b.iStmt != nil {
		b.iStmt.Close()
		b.iStmt = nil
	}
	if b.db != nil {
		b.db.Close()
		b.db = nil
	}

	return nil
}

func (b *postgresBackend) Insert(req *api.InsertRequest) (*api.InsertResponse, error) {
	if req.Entry != nil {
		b.insertQ <- req.Entry
	}
	if len(req.Entries) > 0 {
		for _, e := range req.Entries {
			b.insertQ <- e
		}
	}
	return &api.InsertResponse{}, nil
}

func (b *postgresBackend) QueryStat(req *api.QueryRequest) (*api.QueryStatResponse, error) {
	resCh := make(chan *api.QueryStatResponse, 1)
	go func() { resCh <- b.doQueryStat(req) }()
	select {
	case v := <-resCh:
		return v, nil
	case <-time.After(b.timeout):
		return nil, fmt.Errorf("operation timed out after %s", b.timeout)
	}
}

func (b *postgresBackend) QueryList(req *api.QueryRequest) (*api.QueryListResponse, error) {
	resCh := make(chan *api.QueryListResponse, 1)
	go func() { resCh <- b.doQueryList(req) }()
	select {
	case v := <-resCh:
		return v, nil
	case <-time.After(b.timeout):
		return nil, fmt.Errorf("operation timed out after %s", b.timeout)
	}
}

func (b *postgresBackend) run() {
	retentionTicker := time.NewTicker(1 * time.Hour)
	for {
		select {
		case e := <-b.insertQ:
			b.handleInsert(e)
		case now := <-retentionTicker.C:
			b.handleRetention(now)
		case cond := <-b.stopQ:
			cond.Broadcast()
			return
		}
	}
}

func (b *postgresBackend) handleInsert(e *api.LogEntry) {

	tx, err := b.db.Begin()
	if err != nil {
		log.Printf("Unable to begin transaction: %s", err)
		return
	}

	err = b.handleInsertBatch(tx, e)
	if err != nil {
		log.Printf("Unable to insert: %s", err)
		if rbErr := tx.Rollback(); rbErr != nil {
			log.Printf("Unable to rollback: %s", rbErr)
		}
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("Unable to commit transaction: %s", err)
	}
}

func (b *postgresBackend) handleInsertBatch(tx *sql.Tx, e *api.LogEntry) error {

	if err := b.insertEntry(tx, e); err != nil {
		return err
	}

	for i := 0; i < b.batchSize; i++ {
		select {
		case e = <-b.insertQ:
			if err := b.insertEntry(tx, e); err != nil {
				return err
			}
		default:
			return nil
		}
	}

	return nil
}

func (b *postgresBackend) insertEntry(tx *sql.Tx, e *api.LogEntry) error {
	_, err := tx.Stmt(b.iStmt).Exec(e.Timestamp.UTC(), e.Hostname, e.Application, e.Message)
	return err
}

// buildQueryFromAndWhere appends the shared WHERE clause, using $N placeholders.
// argN points at the next placeholder number (1-based) and is advanced in place.
func (b *postgresBackend) buildQueryFromAndWhere(req *api.QueryRequest, sqlBuf *bytes.Buffer, args *[]interface{}, argN *int) {
	fmt.Fprint(sqlBuf, "FROM logs WHERE 1=1 ")
	if !req.FromTimestamp.IsZero() {
		req.FromTimestamp = req.FromTimestamp.Truncate(time.Minute)
		fmt.Fprintf(sqlBuf, "AND ts >= $%d ", *argN)
		*args = append(*args, req.FromTimestamp.UTC())
		*argN++
	}
	if !req.ToTimestamp.IsZero() {
		req.ToTimestamp = req.ToTimestamp.Truncate(time.Minute).Add(time.Minute - time.Nanosecond)
		fmt.Fprintf(sqlBuf, "AND ts <= $%d ", *argN)
		*args = append(*args, req.ToTimestamp.UTC())
		*argN++
	}
	if req.Hostname != "" {
		fmt.Fprintf(sqlBuf, "AND host = $%d ", *argN)
		*args = append(*args, req.Hostname)
		*argN++
		if req.Application != "" {
			fmt.Fprintf(sqlBuf, "AND app = $%d ", *argN)
			*args = append(*args, req.Application)
			*argN++
		}
	}
	if req.Message != "" {
		fmt.Fprintf(sqlBuf, "AND msg_tsv @@ websearch_to_tsquery('simple', $%d) ", *argN)
		*args = append(*args, req.Message)
		*argN++
	}
}

func (b *postgresBackend) buildQueryLimit(req *api.QueryRequest, sqlBuf *bytes.Buffer, args *[]interface{}, argN *int) {
	fmt.Fprintf(sqlBuf, "LIMIT $%d OFFSET $%d ", *argN, *argN+1)
	*args = append(*args, clamp(0, req.Limit, 256))
	*args = append(*args, clamp(0, req.Offset, math.MaxInt16))
	*argN += 2
}

func (b *postgresBackend) doQueryStat(req *api.QueryRequest) *api.QueryStatResponse {

	args := []interface{}{}
	argN := 1

	sqlBuf := &bytes.Buffer{}
	fmt.Fprint(sqlBuf, "SELECT host, app, COUNT(*) ")
	b.buildQueryFromAndWhere(req, sqlBuf, &args, &argN)
	fmt.Fprint(sqlBuf, "GROUP BY host, app ")
	fmt.Fprint(sqlBuf, "ORDER BY host, app ")
	b.buildQueryLimit(req, sqlBuf, &args, &argN)

	res := api.QueryStatResponse{}

	rows, err := b.db.Query(sqlBuf.String(), args...)
	if err != nil {
		res.Error = err.Error()
		return &res
	}
	defer rows.Close()

	stat := make(map[string]map[string]uint64)
	for rows.Next() {
		var app string
		var proc string
		var count uint64
		if err = rows.Scan(&app, &proc, &count); err != nil {
			res.Error = err.Error()
			return &res
		}
		procs, ok := stat[app]
		if !ok {
			procs = make(map[string]uint64)
			stat[app] = procs
		}
		procs[proc] = count
	}

	if err = rows.Err(); err != nil {
		res.Error = err.Error()
		return &res
	}

	res.Stat = stat
	return &res
}

func (b *postgresBackend) doQueryList(req *api.QueryRequest) *api.QueryListResponse {

	args := []interface{}{}
	argN := 1

	sqlBuf := &bytes.Buffer{}
	fmt.Fprint(sqlBuf, "SELECT ts, host, app, msg ")
	b.buildQueryFromAndWhere(req, sqlBuf, &args, &argN)
	fmt.Fprint(sqlBuf, "ORDER BY ts DESC ")
	b.buildQueryLimit(req, sqlBuf, &args, &argN)

	res := api.QueryListResponse{}

	rows, err := b.db.Query(sqlBuf.String(), args...)
	if err != nil {
		res.Error = err.Error()
		return &res
	}
	defer rows.Close()

	entries := make([]*api.LogEntry, 0, clamp(0, req.Limit, 500))
	for rows.Next() {
		entry := api.LogEntry{}
		if err = rows.Scan(&entry.Timestamp, &entry.Hostname, &entry.Application, &entry.Message); err != nil {
			res.Error = err.Error()
			return &res
		}
		entries = append(entries, &entry)
	}

	if err = rows.Err(); err != nil {
		res.Error = err.Error()
		return &res
	}

	res.Entries = entries
	return &res
}

func (b *postgresBackend) handleRetention(now time.Time) {

	if b.retention == utils.INF {
		return
	}

	upto := now.Add(-time.Duration(b.retention))
	if _, err := b.db.Exec("DELETE FROM logs WHERE ts < $1", upto.UTC()); err != nil {
		log.Printf("Unable to delete: %s", err)
	}
}
