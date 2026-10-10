package pg

import (
	"context"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/domain"
	"github.com/werhani-houssemeddine/go-archkit/errs"
)

// Mapping describes how an aggregate is stored in one table.
type Mapping[A any, ID comparable] struct {
	Table    string
	Resource string // name used in errors (default Table)
	IDColumn string // default "id"
	// VersionColumn enables optimistic locking (requires A to implement domain.Aggregate).
	VersionColumn string
	// Columns lists every column, in the order Scan expects them.
	Columns []string
	// Scan builds an aggregate from a row (columns in Columns order). For versioned
	// aggregates, call Restore(version) on the embedded AggregateRoot.
	Scan func(row Scanner) (A, error)
	// Values returns column → value for persistence. The version column is managed by
	// the repository and may be omitted.
	Values func(a A) map[string]any
	// ID returns the aggregate id.
	ID func(a A) ID
}

// Repository is a generic table-backed implementation of domain.Repository.
//
//	orders := pg.NewRepository(db, pg.Mapping[*Order, string]{
//	    Table: "orders", VersionColumn: "version",
//	    Columns: []string{"id", "customer_id", "status", "lines", "version"},
//	    Scan: scanOrder, Values: orderValues, ID: func(o *Order) string { return o.ID },
//	}, pg.WithOutbox(outbox))
type Repository[A any, ID comparable] struct {
	db        *DB
	m         Mapping[A, ID]
	outbox    *Outbox
	publisher app.EventPublisher
}

var _ domain.Repository[any, string] = (*Repository[any, string])(nil)

// RepoOption configures event dispatching.
type RepoOption func(*repoOpts)

type repoOpts struct {
	outbox    *Outbox
	publisher app.EventPublisher
}

// WithOutbox writes domain events to the outbox in the same transaction (recommended).
func WithOutbox(o *Outbox) RepoOption { return func(r *repoOpts) { r.outbox = o } }

// WithPublisher publishes events after commit (simpler; events can be lost on crash).
func WithPublisher(p app.EventPublisher) RepoOption { return func(r *repoOpts) { r.publisher = p } }

// NewRepository validates the mapping (panics on wiring errors).
func NewRepository[A any, ID comparable](db *DB, m Mapping[A, ID], opts ...RepoOption) *Repository[A, ID] {
	if m.Table == "" || m.Scan == nil || m.Values == nil || m.ID == nil || len(m.Columns) == 0 {
		panic("pg: Mapping requires Table, Columns, Scan, Values and ID")
	}
	if m.IDColumn == "" {
		m.IDColumn = "id"
	}
	if m.Resource == "" {
		m.Resource = m.Table
	}
	for _, c := range append([]string{m.Table, m.IDColumn}, m.Columns...) {
		QuoteIdent(c) // validates
	}
	var o repoOpts
	for _, fn := range opts {
		fn(&o)
	}
	return &Repository[A, ID]{db: db, m: m, outbox: o.outbox, publisher: o.publisher}
}

// Query starts a SELECT of the mapped columns (for custom finders).
func (r *Repository[A, ID]) Query() *SelectBuilder { return Select(r.m.Table, r.m.Columns...) }

// Get loads by id or returns a KindNotFound error (code <RESOURCE>_NOT_FOUND).
func (r *Repository[A, ID]) Get(ctx context.Context, id ID) (A, error) {
	a, err := One(ctx, r.db, r.Query().Eq(r.m.IDColumn, id).Limit(1), r.m.Scan)
	if err != nil && errs.KindOf(err) == errs.KindNotFound {
		var zero A
		return zero, errs.ResourceNotFound(r.m.Resource, id)
	}
	return a, err
}

// Exists reports whether a row with id exists.
func (r *Repository[A, ID]) Exists(ctx context.Context, id ID) (bool, error) {
	return Scalar[bool](ctx, r.db, Q("SELECT EXISTS (?)", Select(r.m.Table, r.m.IDColumn).Eq(r.m.IDColumn, id).Limit(1).Expr()))
}

// FindOne returns the first aggregate matching build, or KindNotFound.
func (r *Repository[A, ID]) FindOne(ctx context.Context, build func(*SelectBuilder)) (A, error) {
	q := r.Query()
	build(q)
	return One(ctx, r.db, q.Limit(1), r.m.Scan)
}

// Find returns every aggregate matching build.
func (r *Repository[A, ID]) Find(ctx context.Context, build func(*SelectBuilder)) ([]A, error) {
	q := r.Query()
	if build != nil {
		build(q)
	}
	return All(ctx, r.db, q, r.m.Scan)
}

// Paginate returns one page; sort fields must be mapped columns.
func (r *Repository[A, ID]) Paginate(ctx context.Context, req domain.PageRequest, build func(*SelectBuilder)) (domain.Page[A], error) {
	q := r.Query()
	if build != nil {
		build(q)
	}
	for _, s := range req.Sort {
		if !contains(r.m.Columns, s.Field) {
			return domain.Page[A]{}, errs.InvalidField("sort", "invalid_sort", "cannot sort by '"+s.Field+"'")
		}
		q.OrderBy(s.Field, s.Desc)
	}
	ct, ca := q.CountSQL()
	total, err := Scalar[int64](ctx, r.db, rawQuery{ct, ca})
	if err != nil {
		return domain.Page[A]{}, err
	}
	items, err := All(ctx, r.db, q.Page(req.Page, req.Size), r.m.Scan)
	if err != nil {
		return domain.Page[A]{}, err
	}
	return domain.NewPage(items, int(total), req), nil
}

// Save inserts or updates. With VersionColumn and a domain.Aggregate: version 0 inserts,
// otherwise UPDATE ... WHERE id = ? AND version = ?; zero affected rows yields
// errs.ConcurrentModification instead of silently overwriting another writer's change.
// Without versioning it upserts on the id column. Pending domain events are written to
// the outbox in the same transaction, or published after commit.
func (r *Repository[A, ID]) Save(ctx context.Context, a A) error {
	work := func(ctx context.Context) error {
		vals := r.m.Values(a)
		agg, versioned := any(a).(domain.Aggregate)
		if r.m.VersionColumn != "" && versioned {
			cur := agg.Version()
			next := cur + 1
			vals[r.m.VersionColumn] = next
			if cur == 0 {
				if _, err := r.db.Exec(ctx, insertFromMap(r.m.Table, vals)); err != nil {
					return err
				}
			} else {
				delete(vals, r.m.IDColumn)
				n, err := r.db.ExecAffected(ctx, Update(r.m.Table).SetMap(vals).Eq(r.m.IDColumn, r.m.ID(a)).Eq(r.m.VersionColumn, cur))
				if err != nil {
					return err
				}
				if n == 0 {
					return errs.ConcurrentModification(r.m.Resource, r.m.ID(a))
				}
			}
			agg.MarkPersisted(next)
		} else {
			ins := insertFromMap(r.m.Table, vals)
			ins.OnConflictUpdate([]string{r.m.IDColumn})
			if _, err := r.db.Exec(ctx, ins); err != nil {
				return err
			}
			if versioned {
				agg.MarkPersisted(agg.Version() + 1)
			}
		}
		if versioned {
			return r.dispatch(ctx, agg)
		}
		return nil
	}
	if r.outbox != nil && !r.db.InTx(ctx) {
		return r.db.Transact(ctx, work)
	}
	return work(ctx)
}

// Delete removes the aggregate (and dispatches its pending events).
func (r *Repository[A, ID]) Delete(ctx context.Context, a A) error {
	work := func(ctx context.Context) error {
		if _, err := r.db.Exec(ctx, Delete(r.m.Table).Eq(r.m.IDColumn, r.m.ID(a))); err != nil {
			return err
		}
		if agg, ok := any(a).(domain.Aggregate); ok {
			return r.dispatch(ctx, agg)
		}
		return nil
	}
	if r.outbox != nil && !r.db.InTx(ctx) {
		return r.db.Transact(ctx, work)
	}
	return work(ctx)
}

func (r *Repository[A, ID]) dispatch(ctx context.Context, agg domain.Aggregate) error {
	evs := agg.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	if r.outbox != nil {
		return r.outbox.Append(ctx, evs...)
	}
	if r.publisher != nil {
		pub := r.publisher
		r.db.AfterCommit(ctx, func(ctx context.Context) { _ = pub.Publish(ctx, evs...) })
	}
	return nil
}

func insertFromMap(table string, vals map[string]any) *InsertBuilder {
	cols := sortedKeys(vals)
	return Insert(table, cols...).Row(vals)
}

type rawQuery struct {
	text string
	args []any
}

func (q rawQuery) SQL() (string, []any) { return q.text, q.args }
