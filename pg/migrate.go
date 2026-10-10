package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/logx"
)

// Migrator applies forward-only SQL migrations from an fs.FS (typically //go:embed):
// files named "NNNN_description.sql" run in lexical order, each in its own
// transaction, recorded with a checksum. A Postgres advisory lock prevents two
// instances from migrating concurrently. Paths are confined to the fs.FS, so there is
// no path traversal.
//
//	//go:embed migrations/*.sql
//	var migrations embed.FS
//	sub, _ := fs.Sub(migrations, "migrations")
//	applied, err := pg.NewMigrator(db, sub, pg.MigratorOptions{}).Up(ctx)
type Migrator struct {
	db   *DB
	fsys fs.FS
	o    MigratorOptions
	log  *slog.Logger
}

// MigratorOptions configure a Migrator.
type MigratorOptions struct {
	Table string // default "schema_migrations"
	// AllowChangedChecksums disables the error when an applied file was edited.
	AllowChangedChecksums bool
	Logger                *slog.Logger
}

// MigrationStatus describes one migration file.
type MigrationStatus struct {
	Name      string
	Applied   bool
	AppliedAt time.Time
	Changed   bool
}

var migrationRe = regexp.MustCompile(`^\d+[_-][\w-]+\.sql$`)

// NewMigrator creates a migrator.
func NewMigrator(db *DB, fsys fs.FS, o MigratorOptions) *Migrator {
	if o.Table == "" {
		o.Table = "schema_migrations"
	}
	QuoteIdent(o.Table)
	return &Migrator{db: db, fsys: fsys, o: o, log: logx.OrDiscard(o.Logger).With(slog.String("component", "migrator"))}
}

type migrationFile struct{ name, sql, sum string }

func (m *Migrator) files() ([]migrationFile, error) {
	entries, err := fs.ReadDir(m.fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []migrationFile
	for _, e := range entries {
		if e.IsDir() || !migrationRe.MatchString(e.Name()) {
			continue
		}
		b, err := fs.ReadFile(m.fsys, e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, migrationFile{name: e.Name(), sql: string(b), sum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func (m *Migrator) ensureTable(ctx context.Context, q querier) error {
	_, err := q.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+QuoteIdent(m.o.Table)+" (name text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())")
	return err
}

func (m *Migrator) applied(ctx context.Context, q querier) (map[string]MigrationStatus, map[string]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, checksum, applied_at FROM "+QuoteIdent(m.o.Table))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	st, sums := map[string]MigrationStatus{}, map[string]string{}
	for rows.Next() {
		var name, sum string
		var at time.Time
		if err := rows.Scan(&name, &sum, &at); err != nil {
			return nil, nil, err
		}
		st[name] = MigrationStatus{Name: name, Applied: true, AppliedAt: at}
		sums[name] = sum
	}
	return st, sums, rows.Err()
}

// Status lists migrations and whether they were applied or changed.
func (m *Migrator) Status(ctx context.Context) ([]MigrationStatus, error) {
	if err := m.ensureTable(ctx, m.db.sql); err != nil {
		return nil, Translate(err, nil)
	}
	files, err := m.files()
	if err != nil {
		return nil, err
	}
	st, sums, err := m.applied(ctx, m.db.sql)
	if err != nil {
		return nil, Translate(err, nil)
	}
	out := make([]MigrationStatus, 0, len(files))
	for _, f := range files {
		s, ok := st[f.name]
		if !ok {
			s = MigrationStatus{Name: f.name}
		}
		s.Changed = ok && sums[f.name] != f.sum
		out = append(out, s)
	}
	return out, nil
}

// Up applies pending migrations and returns their names.
func (m *Migrator) Up(ctx context.Context) ([]string, error) {
	conn, err := m.db.sql.Conn(ctx) // one connection: the advisory lock is session-scoped
	if err != nil {
		return nil, Translate(err, nil)
	}
	defer conn.Close()
	h := fnv.New64a()
	h.Write([]byte(m.o.Table))
	lockID := int64(h.Sum64() >> 1)
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return nil, Translate(err, nil)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockID) //nolint:errcheck

	if err := m.ensureTable(ctx, conn); err != nil {
		return nil, Translate(err, nil)
	}
	files, err := m.files()
	if err != nil {
		return nil, err
	}
	_, sums, err := m.applied(ctx, conn)
	if err != nil {
		return nil, Translate(err, nil)
	}
	var done []string
	for _, f := range files {
		if sum, ok := sums[f.name]; ok {
			if sum != f.sum && !m.o.AllowChangedChecksums {
				return done, errs.Newf(errs.KindInternal, "MIGRATION_CHANGED", "migration %s was modified after being applied", f.name)
			}
			continue
		}
		m.log.InfoContext(ctx, "applying migration", slog.String("migration", f.name))
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return done, Translate(err, nil)
		}
		if _, err := tx.ExecContext(ctx, f.sql); err != nil {
			_ = tx.Rollback()
			return done, errs.Wrap(err, errs.KindInternal, "MIGRATION_FAILED", fmt.Sprintf("migration %s failed", f.name))
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+QuoteIdent(m.o.Table)+" (name, checksum) VALUES ($1, $2)", f.name, f.sum); err != nil {
			_ = tx.Rollback()
			return done, Translate(err, nil)
		}
		if err := tx.Commit(); err != nil {
			return done, Translate(err, nil)
		}
		done = append(done, f.name)
	}
	m.log.InfoContext(ctx, "migrations complete", slog.Int("applied", len(done)))
	return done, nil
}
