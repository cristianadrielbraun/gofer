package storage

import (
	"container/list"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Gofer opens SQLite through a thin wrapper around the modernc driver that
// adds two things:
//
//   - A per-connection cache of prepared statements. The driver parses the SQL
//     text of every unprepared Exec and Query; the same statements run
//     thousands of times (every synced message, every list page), and parsing
//     was over a third of the work.
//   - Optional logging of statements and transactions slower than a threshold
//     (SetSlowQueryThreshold). A query is timed until its rows are read; a
//     transaction from begin to commit or rollback, which is how long it holds
//     the database's writer. Only statement text is logged, never values.
const sqliteDriverName = "sqlite-gofer"

// statementCacheSize bounds prepared statements kept per connection. Hot
// paths use far fewer; statements built with variable-length IN lists come
// and go through the LRU order.
const statementCacheSize = 128

var slowQueryThreshold atomic.Int64

func init() {
	base, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	sql.Register(sqliteDriverName, wrappedDriver{base: base.Driver()})
	_ = base.Close()
}

// SetSlowQueryThreshold enables logging of slower statements and
// transactions. Zero or a negative value disables it.
func SetSlowQueryThreshold(threshold time.Duration) {
	slowQueryThreshold.Store(int64(threshold))
}

func logIfSlow(kind, query string, started time.Time) {
	threshold := time.Duration(slowQueryThreshold.Load())
	if threshold <= 0 {
		return
	}
	elapsed := time.Since(started)
	if elapsed < threshold {
		return
	}
	log.Printf("storage: slow %s %dms: %s", kind, elapsed.Milliseconds(), compactQuery(query))
}

func compactQuery(query string) string {
	query = strings.Join(strings.Fields(query), " ")
	const limit = 400
	if len(query) > limit {
		return query[:limit] + "…"
	}
	return query
}

type wrappedDriver struct{ base driver.Driver }

func (d wrappedDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	hookContactChanges(name, conn)
	return &wrappedConn{base: conn, cache: make(map[string]*list.Element), order: list.New()}, nil
}

// wrappedConn is used by one goroutine at a time, as database/sql
// guarantees, so its cache needs no lock.
type wrappedConn struct {
	base       driver.Conn
	cache      map[string]*list.Element // of *cachedStmt, most recent first
	order      *list.List
	txStarted  time.Time
	txFirstSQL string
}

type cachedStmt struct {
	query   string
	stmt    driver.Stmt
	busy    bool // its rows are open
	evicted bool // close when no longer busy
}

func (c *wrappedConn) noteStatement(query string) {
	if !c.txStarted.IsZero() && c.txFirstSQL == "" {
		c.txFirstSQL = query
	}
}

func (c *wrappedConn) prepareBase(ctx context.Context, query string) (driver.Stmt, error) {
	if preparer, ok := c.base.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.base.Prepare(query)
}

// statement returns a prepared statement for query and a release function.
// A statement whose rows are still open on this connection is never handed
// out again; a separate one is prepared and closed on release instead.
func (c *wrappedConn) statement(ctx context.Context, query string) (driver.Stmt, func(), error) {
	if element, ok := c.cache[query]; ok {
		entry := element.Value.(*cachedStmt)
		if !entry.busy {
			c.order.MoveToFront(element)
			return entry.stmt, func() {}, nil
		}
		stmt, err := c.prepareBase(ctx, query)
		if err != nil {
			return nil, nil, err
		}
		return stmt, func() { _ = stmt.Close() }, nil
	}
	stmt, err := c.prepareBase(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	entry := &cachedStmt{query: query, stmt: stmt}
	c.cache[query] = c.order.PushFront(entry)
	for c.order.Len() > statementCacheSize {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		old := oldest.Value.(*cachedStmt)
		delete(c.cache, old.query)
		if old.busy {
			old.evicted = true
		} else {
			_ = old.stmt.Close()
		}
	}
	return stmt, func() {}, nil
}

func (c *wrappedConn) markBusy(query string, stmt driver.Stmt) func() {
	element, ok := c.cache[query]
	if !ok || element.Value.(*cachedStmt).stmt != stmt {
		return func() {}
	}
	entry := element.Value.(*cachedStmt)
	entry.busy = true
	return func() {
		entry.busy = false
		if entry.evicted {
			_ = entry.stmt.Close()
		}
	}
}

func (c *wrappedConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext serves explicit prepares (tx.Prepare). They are closed by
// their owner, so they are not taken from the cache.
func (c *wrappedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.prepareBase(ctx, query)
	if err != nil {
		return nil, err
	}
	return &wrappedStmt{base: stmt, conn: c, query: query}, nil
}

func (c *wrappedConn) Close() error {
	for element := c.order.Front(); element != nil; element = element.Next() {
		_ = element.Value.(*cachedStmt).stmt.Close()
	}
	c.cache, c.order = nil, list.New()
	return c.base.Close()
}

func (c *wrappedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *wrappedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.base.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("sqlite connection does not support BeginTx")
	}
	tx, err := beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.txStarted, c.txFirstSQL = time.Now(), ""
	return &wrappedTx{base: tx, conn: c}, nil
}

func (c *wrappedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.noteStatement(query)
	started := time.Now()
	stmt, release, err := c.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	defer release()
	result, err := stmt.(driver.StmtExecContext).ExecContext(ctx, args)
	logIfSlow("statement", query, started)
	return result, err
}

func (c *wrappedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.noteStatement(query)
	started := time.Now()
	stmt, release, err := c.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	rows, err := stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		release()
		logIfSlow("query", query, started)
		return nil, err
	}
	unbusy := c.markBusy(query, stmt)
	return &wrappedRows{base: rows, query: query, started: started, release: func() { unbusy(); release() }}, nil
}

func (c *wrappedConn) Ping(ctx context.Context) error {
	if pinger, ok := c.base.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *wrappedConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.base.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *wrappedConn) IsValid() bool {
	if validator, ok := c.base.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

type wrappedTx struct {
	base driver.Tx
	conn *wrappedConn
}

func (t *wrappedTx) finish(err error) error {
	started, first := t.conn.txStarted, t.conn.txFirstSQL
	t.conn.txStarted, t.conn.txFirstSQL = time.Time{}, ""
	if first == "" {
		first = "(no statements)"
	}
	logIfSlow("transaction", first, started)
	return err
}

func (t *wrappedTx) Commit() error   { return t.finish(t.base.Commit()) }
func (t *wrappedTx) Rollback() error { return t.finish(t.base.Rollback()) }

type wrappedStmt struct {
	base  driver.Stmt
	conn  *wrappedConn
	query string
}

func (s *wrappedStmt) Close() error  { return s.base.Close() }
func (s *wrappedStmt) NumInput() int { return s.base.NumInput() }

func (s *wrappedStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errors.New("sqlite statements require ExecContext")
}

func (s *wrappedStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("sqlite statements require QueryContext")
}

func (s *wrappedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.conn.noteStatement(s.query)
	started := time.Now()
	result, err := s.base.(driver.StmtExecContext).ExecContext(ctx, args)
	logIfSlow("statement", s.query, started)
	return result, err
}

func (s *wrappedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.conn.noteStatement(s.query)
	started := time.Now()
	rows, err := s.base.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		logIfSlow("query", s.query, started)
		return nil, err
	}
	return &wrappedRows{base: rows, query: s.query, started: started}, nil
}

type wrappedRows struct {
	base    driver.Rows
	query   string
	started time.Time
	timed   bool
	release func()
}

func (r *wrappedRows) Columns() []string { return r.base.Columns() }

func (r *wrappedRows) Next(dest []driver.Value) error {
	err := r.base.Next(dest)
	if err == io.EOF && !r.timed {
		// Rows are often read to the end long before Close is deferred.
		r.timed = true
		logIfSlow("query", r.query, r.started)
	}
	return err
}

func (r *wrappedRows) Close() error {
	if !r.timed {
		r.timed = true
		logIfSlow("query", r.query, r.started)
	}
	err := r.base.Close()
	if r.release != nil {
		r.release()
		r.release = nil
	}
	return err
}
