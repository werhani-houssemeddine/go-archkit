// Package pg provides PostgreSQL infrastructure on top of database/sql, so it works
// with any driver (pgx's stdlib adapter, lib/pq...):
//
//   - DB: context-carried (ambient) transactions with savepoints, after-commit hooks,
//     serialization-failure retries, slow-query logging, typed error translation,
//     health checks;
//   - a safe query builder (identifiers quoted, values bound, empty IN handled,
//     UPDATE/DELETE without WHERE refused);
//   - a generic Repository with optimistic locking and transactional outbox;
//   - an outbox Relay (FOR UPDATE SKIP LOCKED) and an fs.FS-based Migrator.
package pg

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/lifecycle"
	"github.com/werhani-houssemeddine/go-archkit/logx"
	"github.com/werhani-houssemeddine/go-archkit/resilience"
)

// Options configure a DB.
type Options struct {
	Logger *slog.Logger
	// SlowQuery logs statements slower than this at warn level (default 500ms).
	SlowQuery time.Duration
	// LogQueries logs every statement (SQL text only, never arguments) at debug level.
	LogQueries bool
	// Constraints maps constraint names to friendly errors.
	Constraints ConstraintMap
	// TxRetries retries transactions failing with serialization errors / deadlocks (default 2).
	TxRetries int
}

// DB wraps *sql.DB. Create one per database in main and share it.
type DB struct {
	sql  *sql.DB
	opts Options
	log  *slog.Logger
}

// New wraps an opened *sql.DB (configure pool limits on it: SetMaxOpenConns, ...).
func New(db *sql.DB, o Options) *DB {
	if o.SlowQuery <= 0 {
		o.SlowQuery = 500 * time.Millisecond
	}
	if o.TxRetries == 0 {
		o.TxRetries = 2
	}
	return &DB{sql: db, opts: o, log: logx.OrDiscard(o.Logger).With(slog.String("component", "postgres"))}
}

// Open opens a database with the given driver name ("pgx", "postgres") and DSN.
func Open(driver, dsn string, o Options) (*DB, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	return New(db, o), nil
}

// SQLDB exposes the underlying *sql.DB.
func (db *DB) SQLDB() *sql.DB { return db.sql }

// Connect pings with retries (call once at startup).
func (db *DB) Connect(ctx context.Context) error {
	err := resilience.Retry(ctx, resilience.RetryPolicy{
		Retries: 5, BaseDelay: 500 * time.Millisecond, MaxDelay: 5 * time.Second,
		RetryIf: func(error) bool { return true },
		OnRetry: func(n int, err error, d time.Duration) {
			db.log.WarnContext(ctx, "postgres connection failed, retrying", slog.Int("attempt", n), slog.Duration("delay", d), logx.Err(err))
		},
	}, db.sql.PingContext)
	if err != nil {
		return errs.Wrap(err, errs.KindUnavailable, "DATABASE_UNAVAILABLE", "cannot connect to database")
	}
	db.log.InfoContext(ctx, "postgres connected")
	return nil
}

// Close closes the pool.
func (db *DB) Close() error { return db.sql.Close() }

/* ------------------------------------------------------------------ */
/* Querying                                                            */
/* ------------------------------------------------------------------ */

type querier interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

type txState struct {
	tx          *sql.Tx
	depth       atomic.Int32
	afterCommit []func(ctx context.Context)
}

type txKey struct{ db *DB }

func (db *DB) state(ctx context.Context) *txState {
	s, _ := ctx.Value(txKey{db}).(*txState)
	return s
}

func (db *DB) q(ctx context.Context) querier {
	if s := db.state(ctx); s != nil {
		return s.tx
	}
	return db.sql
}

// InTx reports whether ctx carries a transaction of this DB.
func (db *DB) InTx(ctx context.Context) bool { return db.state(ctx) != nil }

func (db *DB) observe(ctx context.Context, text string, start time.Time) {
	d := time.Since(start)
	if d > db.opts.SlowQuery {
		db.log.WarnContext(ctx, "slow query", slog.Duration("duration", d), slog.String("sql", truncate(text, 500)))
	} else if db.opts.LogQueries {
		db.log.DebugContext(ctx, "query", slog.Duration("duration", d), slog.String("sql", truncate(text, 500)))
	}
}

func (db *DB) wrapErr(err error, text string) error {
	if err == nil {
		return nil
	}
	err = Translate(err, db.opts.Constraints)
	if e, ok := errs.As(err); ok {
		return e.WithMeta("sql", truncate(text, 500))
	}
	return err
}

// Exec runs a statement (inside the ctx transaction when present).
func (db *DB) Exec(ctx context.Context, q Query) (sql.Result, error) {
	text, args := q.SQL()
	start := time.Now()
	res, err := db.q(ctx).ExecContext(ctx, text, args...)
	db.observe(ctx, text, start)
	return res, db.wrapErr(err, text)
}

// ExecAffected runs a statement and returns the affected row count.
func (db *DB) ExecAffected(ctx context.Context, q Query) (int64, error) {
	res, err := db.Exec(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Rows runs a query; the caller must Close the rows.
func (db *DB) Rows(ctx context.Context, q Query) (*sql.Rows, error) {
	text, args := q.SQL()
	start := time.Now()
	rows, err := db.q(ctx).QueryContext(ctx, text, args...)
	db.observe(ctx, text, start)
	return rows, db.wrapErr(err, text)
}

// Scanner is implemented by *sql.Row and *sql.Rows.
type Scanner interface{ Scan(dest ...any) error }

// All runs q and maps every row with scan.
func All[T any](ctx context.Context, db *DB, q Query, scan func(Scanner) (T, error)) ([]T, error) {
	rows, err := db.Rows(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, db.wrapErr(err, "")
		}
		out = append(out, v)
	}
	text, _ := q.SQL()
	return out, db.wrapErr(rows.Err(), text)
}

// One runs q and maps the first row; no row yields an errs.KindNotFound error.
func One[T any](ctx context.Context, db *DB, q Query, scan func(Scanner) (T, error)) (T, error) {
	text, args := q.SQL()
	start := time.Now()
	row := db.q(ctx).QueryRowContext(ctx, text, args...)
	db.observe(ctx, text, start)
	v, err := scan(row)
	if err != nil {
		var zero T
		return zero, db.wrapErr(err, text)
	}
	return v, nil
}

// Scalar returns the first column of the first row (COUNT, EXISTS, RETURNING id).
func Scalar[T any](ctx context.Context, db *DB, q Query) (T, error) {
	return One(ctx, db, q, func(s Scanner) (T, error) {
		var v T
		err := s.Scan(&v)
		return v, err
	})
}

/* ------------------------------------------------------------------ */
/* Transactions (app.UnitOfWork)                                       */
/* ------------------------------------------------------------------ */

var _ app.UnitOfWork = (*DB)(nil)

// Transact runs fn in a transaction carried by the returned ctx: every DB call and
// repository using that ctx joins it. Nested calls become SAVEPOINTs. Serialization
// failures and deadlocks are retried (Options.TxRetries). Side effects registered with
// AfterCommit run only after the outermost commit.
//
// Do not run concurrent queries on the same transaction ctx: a connection executes one
// statement at a time.
func (db *DB) Transact(ctx context.Context, fn func(ctx context.Context) error, opts ...app.TxOptions) error {
	if s := db.state(ctx); s != nil {
		return db.savepoint(ctx, s, fn)
	}
	var o app.TxOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	var err error
	for attempt := 0; attempt <= db.opts.TxRetries; attempt++ {
		err = db.runTx(ctx, fn, o)
		if err == nil || !IsSerializationFailure(err) || ctx.Err() != nil {
			return err
		}
		db.log.WarnContext(ctx, "transaction serialization failure, retrying", slog.Int("attempt", attempt+1))
	}
	return err
}

func (db *DB) runTx(ctx context.Context, fn func(ctx context.Context) error, o app.TxOptions) (err error) {
	txo := &sql.TxOptions{ReadOnly: o.ReadOnly}
	switch o.Isolation {
	case app.ReadCommitted:
		txo.Isolation = sql.LevelReadCommitted
	case app.RepeatableRead:
		txo.Isolation = sql.LevelRepeatableRead
	case app.Serializable:
		txo.Isolation = sql.LevelSerializable
	}
	tx, err := db.sql.BeginTx(ctx, txo)
	if err != nil {
		return db.wrapErr(err, "BEGIN")
	}
	s := &txState{tx: tx}
	tctx := context.WithValue(ctx, txKey{db}, s)
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback()
			panic(r)
		}
	}()
	if err = fn(tctx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			db.log.ErrorContext(ctx, "rollback failed", logx.Err(rbErr))
		}
		return err
	}
	if err = tx.Commit(); err != nil {
		return db.wrapErr(err, "COMMIT")
	}
	hookCtx := execctx.Detach(ctx)
	for _, h := range s.afterCommit {
		func() {
			defer func() {
				if r := recover(); r != nil {
					db.log.ErrorContext(hookCtx, "after-commit hook panicked", slog.Any("panic", r))
				}
			}()
			h(hookCtx)
		}()
	}
	return nil
}

func (db *DB) savepoint(ctx context.Context, s *txState, fn func(ctx context.Context) error) error {
	name := "sp_" + strconv.Itoa(int(s.depth.Add(1)))
	defer s.depth.Add(-1)
	if _, err := s.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return db.wrapErr(err, "SAVEPOINT")
	}
	if err := fn(ctx); err != nil {
		if _, rbErr := s.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name); rbErr != nil {
			return errors.Join(err, db.wrapErr(rbErr, "ROLLBACK TO SAVEPOINT"))
		}
		return err
	}
	_, err := s.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+name)
	return db.wrapErr(err, "RELEASE SAVEPOINT")
}

// AfterCommit defers fn until the outermost transaction in ctx commits (dropped on
// rollback). Outside a transaction fn runs immediately. fn receives a context that
// keeps execctx values but not the request's cancellation.
func (db *DB) AfterCommit(ctx context.Context, fn func(ctx context.Context)) {
	if s := db.state(ctx); s != nil {
		s.afterCommit = append(s.afterCommit, fn)
		return
	}
	fn(execctx.Detach(ctx))
}

/* ------------------------------------------------------------------ */
/* Health                                                              */
/* ------------------------------------------------------------------ */

// HealthCheck returns a critical lifecycle check pinging the database.
func (db *DB) HealthCheck(name string) lifecycle.Check {
	if name == "" {
		name = "postgres"
	}
	return lifecycle.Check{Name: name, Critical: true, Fn: func(ctx context.Context) error {
		return db.sql.PingContext(ctx)
	}}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
