package pg_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	_ "github.com/lib/pq"

	"github.com/werhani-houssemeddine/go-archkit/domain"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/events"
	"github.com/werhani-houssemeddine/go-archkit/ids"
	"github.com/werhani-houssemeddine/go-archkit/pg"
)

/* ------------------------------------------------------------------ */
/* Query builder (unit)                                                */
/* ------------------------------------------------------------------ */

func TestExprParameterizesAndQuotes(t *testing.T) {
	email := "x'; DROP TABLE users; --"
	q, args := pg.Q("SELECT * FROM ? WHERE email = ? AND ?", pg.Ident("public.users"), email, pg.Q("id = ?", 5)).SQL()
	if q != `SELECT * FROM "public"."users" WHERE email = $1 AND id = $2` || args[0] != email || args[1] != 5 {
		t.Fatalf("%s %v", q, args)
	}
	if pg.SafeIdent("users; DROP TABLE x") {
		t.Fatal("unsafe identifier accepted")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("QuoteIdent must panic on unsafe names")
		}
	}()
	pg.QuoteIdent(`name"; --`)
}

func TestSelectBuilder(t *testing.T) {
	q := pg.Select("users", "u.id", "u.name").As("u").
		Eq("status", "active").
		Group(func(w *pg.Where) { w.Eq("role", "admin").Or("role", "=", "owner") }).
		Contains("name", "50%_off").
		OrderBy("name", true).Page(3, 10)
	pg.In(&q.Conditions, "id", []int{1, 2})
	text, args := q.SQL()
	want := `SELECT "u"."id", "u"."name" FROM "users" AS "u" WHERE "status" = $1 AND ("role" = $2 OR "role" = $3) AND "name" ILIKE $4 AND "id" IN ($5, $6) ORDER BY "name" DESC LIMIT $7 OFFSET $8`
	if text != want || len(args) != 8 || args[3] != `%50\%\_off%` {
		t.Fatalf("\n got  %s %v\n want %s", text, args, want)
	}
	lr, _ := pg.Select("t").Eq("a", 1).OrWhere("b", "=", 2).Where("c", "=", 3).SQL()
	if lr != `SELECT * FROM "t" WHERE ("a" = $1 OR "b" = $2) AND "c" = $3` {
		t.Fatal("left-to-right grouping: " + lr)
	}
	empty := pg.Select("t")
	pg.In(&empty.Conditions, "id", []string{})
	if s, _ := empty.SQL(); s != `SELECT * FROM "t" WHERE FALSE` {
		t.Fatal("empty IN must stay valid: " + s)
	}
	if s, _ := pg.Select("t").Eq("deleted_at", nil).SQL(); !strings.Contains(s, `"deleted_at" IS NULL`) {
		t.Fatal("nil equality")
	}
}

func TestWriteBuildersAndSafety(t *testing.T) {
	text, args := pg.Insert("users", "id", "name").Values(1, "a").Values(2, "b").OnConflictUpdate([]string{"id"}).Returning("id").SQL()
	if text != `INSERT INTO "users" ("id", "name") VALUES ($1, $2), ($3, $4) ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name" RETURNING "id"` || len(args) != 4 {
		t.Fatal(text)
	}
	u, _ := pg.Update("users").Set("version", pg.Q("version + 1")).Eq("id", 1).SQL()
	if u != `UPDATE "users" SET "version" = version + 1 WHERE "id" = $1` {
		t.Fatal(u)
	}
	mustPanic(t, "update without where", func() { pg.Update("users").Set("a", 1).SQL() })
	mustPanic(t, "delete without where", func() { pg.Delete("users").SQL() })
	mustPanic(t, "bad operator", func() { pg.Select("t").Where("a", "= 1 OR 1=1 --", 1) })
	if d, _ := pg.Delete("users").All().SQL(); d != `DELETE FROM "users"` {
		t.Fatal(d)
	}
}

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected panic", name)
		}
	}()
	fn()
}

/* ------------------------------------------------------------------ */
/* Integration (set PG_TEST_DSN, e.g. postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable) */
/* ------------------------------------------------------------------ */

type ItemRenamed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var ItemRenamedEvent = domain.DefineEvent[ItemRenamed]("inventory.item_renamed")

type Item struct {
	domain.AggregateRoot
	ID   string
	Name string
	Qty  int
}

func (i *Item) Rename(ctx context.Context, name string) {
	i.Name = name
	i.Record(ItemRenamedEvent.New(ctx, ItemRenamed{ID: i.ID, Name: name}, domain.ForAggregate(i.ID, "Item")))
}

func openDB(t *testing.T) *pg.DB {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set")
	}
	db, err := pg.Open("postgres", dsn, pg.Options{Constraints: pg.ConstraintMap{
		"items_name_key": {Field: "name", Message: "an item with this name already exists", Code: "ITEM_NAME_TAKEN"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	db.SQLDB().SetMaxOpenConns(1)
	if err := db.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// PGlite's socket server desyncs a connection after a statement error (a test-harness
// quirk, not PostgreSQL behavior): drop idle connections between error cases.
func reset(db *pg.DB) {
	db.SQLDB().SetMaxIdleConns(0)
	time.Sleep(50 * time.Millisecond)
	db.SQLDB().SetMaxIdleConns(2)
}

func TestPostgresIntegration(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	migrations := fstest.MapFS{
		"0001_items.sql":  {Data: []byte(`CREATE TABLE items (id uuid PRIMARY KEY, name text NOT NULL UNIQUE, qty int NOT NULL CHECK (qty >= 0), version int NOT NULL);`)},
		"0002_outbox.sql": {Data: []byte(pg.OutboxDDL(""))},
		"README.md":       {Data: []byte("ignored")},
	}
	// start from a clean slate so the test can be re-run against the same database
	if _, err := db.Exec(ctx, pg.Raw("DROP TABLE IF EXISTS items, outbox_events, schema_migrations")); err != nil {
		t.Fatal(err)
	}
	mig := pg.NewMigrator(db, migrations, pg.MigratorOptions{})
	applied, err := mig.Up(ctx)
	if err != nil || len(applied) != 2 {
		t.Fatalf("migrate: %v %v", applied, err)
	}
	if again, err := mig.Up(ctx); err != nil || len(again) != 0 {
		t.Fatalf("idempotent migrate: %v %v", again, err)
	}

	t.Run("transactions, savepoints, after-commit", func(t *testing.T) {
		var committed []string
		err := db.Transact(ctx, func(ctx context.Context) error {
			if _, err := db.Exec(ctx, pg.Insert("items", "id", "name", "qty", "version").Values(ids.NewV7().String(), "rolled-back", 1, 1)); err != nil {
				return err
			}
			db.AfterCommit(ctx, func(context.Context) { committed = append(committed, "nope") })
			return errors.New("boom")
		})
		if err == nil || err.Error() != "boom" {
			t.Fatalf("user errors propagate unchanged: %v", err)
		}
		if n, _ := pg.Scalar[int64](ctx, db, pg.Q("SELECT count(*) FROM items WHERE name = ?", "rolled-back")); n != 0 {
			t.Fatal("rollback")
		}
		err = db.Transact(ctx, func(ctx context.Context) error {
			if _, err := db.Exec(ctx, pg.Insert("items", "id", "name", "qty", "version").Values(ids.NewV7().String(), "outer", 1, 1)); err != nil {
				return err
			}
			_ = db.Transact(ctx, func(ctx context.Context) error {
				_, _ = db.Exec(ctx, pg.Insert("items", "id", "name", "qty", "version").Values(ids.NewV7().String(), "inner", 1, 1))
				return errors.New("inner fails")
			})
			db.AfterCommit(ctx, func(context.Context) { committed = append(committed, "yes") })
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		n, _ := pg.Scalar[int64](ctx, db, pg.Q("SELECT count(*) FROM items WHERE name IN ('outer', 'inner')"))
		if n != 1 || len(committed) != 1 || committed[0] != "yes" {
			t.Fatalf("savepoint n=%d hooks=%v", n, committed)
		}
	})

	t.Run("repository, optimistic locking, pagination, outbox relay", func(t *testing.T) {
		outbox := pg.NewOutbox(db, "")
		repo := pg.NewRepository(db, pg.Mapping[*Item, string]{
			Table: "items", Resource: "Item", VersionColumn: "version",
			Columns: []string{"id", "name", "qty", "version"},
			Scan: func(s pg.Scanner) (*Item, error) {
				var it Item
				var v int
				if err := s.Scan(&it.ID, &it.Name, &it.Qty, &v); err != nil {
					return nil, err
				}
				it.Restore(v)
				return &it, nil
			},
			Values: func(i *Item) map[string]any { return map[string]any{"id": i.ID, "name": i.Name, "qty": i.Qty} },
			ID:     func(i *Item) string { return i.ID },
		}, pg.WithOutbox(outbox))

		item := &Item{ID: ids.NewV7().String(), Name: "widget", Qty: 3}
		if err := repo.Save(ctx, item); err != nil || item.Version() != 1 {
			t.Fatalf("insert: %v v=%d", err, item.Version())
		}
		a, _ := repo.Get(ctx, item.ID)
		b, _ := repo.Get(ctx, item.ID)
		a.Rename(ctx, "widget-a")
		if err := repo.Save(ctx, a); err != nil {
			t.Fatal(err)
		}
		b.Rename(ctx, "widget-b")
		if err := repo.Save(ctx, b); errs.CodeOf(err) != "CONCURRENT_MODIFICATION" {
			t.Fatalf("lost update must be detected: %v", err)
		}
		if got, _ := repo.Get(ctx, item.ID); got.Name != "widget-a" || got.Version() != 2 {
			t.Fatalf("stored: %+v", got)
		}
		if _, err := repo.Get(ctx, ids.NewV7().String()); errs.CodeOf(err) != "ITEM_NOT_FOUND" {
			t.Fatalf("not found: %v", err)
		}
		if ok, err := repo.Exists(ctx, item.ID); !ok || err != nil {
			t.Fatalf("exists: %v %v", ok, err)
		}
		for i := 0; i < 5; i++ {
			if err := repo.Save(ctx, &Item{ID: ids.NewV7().String(), Name: "bulk-" + string(rune('0'+i)), Qty: i}); err != nil {
				t.Fatal(err)
			}
		}
		page, err := repo.Paginate(ctx, domain.PageRequest{Page: 2, Size: 2, Sort: []domain.SortOrder{{Field: "name"}}}, func(q *pg.SelectBuilder) {
			q.Where("name", "LIKE", "bulk-%")
		})
		if err != nil || page.Total != 5 || len(page.Items) != 2 || page.Items[0].Name != "bulk-2" {
			t.Fatalf("page: %+v %v", page, err)
		}

		bus := events.NewBus(events.Options{})
		var delivered []string
		events.On(bus, ItemRenamedEvent, "projector", func(_ context.Context, _ domain.Event, p ItemRenamed) error {
			delivered = append(delivered, p.Name)
			return nil
		})
		relay := pg.NewRelay(db, bus, pg.RelayOptions{})
		if n, err := relay.ProcessBatch(ctx); err != nil || n != 1 {
			t.Fatalf("relay: n=%d err=%v", n, err)
		}
		if len(delivered) != 1 || delivered[0] != "widget-a" {
			t.Fatalf("only the committed rename is delivered: %v", delivered)
		}
		if n, _ := relay.ProcessBatch(ctx); n != 0 {
			t.Fatal("processed events are not redelivered")
		}
	})

	t.Run("health", func(t *testing.T) {
		if err := db.HealthCheck("").Fn(ctx); err != nil {
			t.Fatal(err)
		}
	})
	// Run last: PGlite's socket server (the test harness) can desync a connection after a
	// statement error, which real PostgreSQL never does. Literal SQL keeps lib/pq on the
	// simple query protocol, which PGlite handles reliably; translation is identical.
	t.Run("error translation", func(t *testing.T) {
		exec := func(q string) error { _, err := db.Exec(ctx, pg.Raw(q)); return err }
		if err := exec(`INSERT INTO items (id, name, qty, version) VALUES ('` + ids.NewV7().String() + `', 'dup', 1, 1)`); err != nil {
			t.Fatal(err)
		}
		err := exec(`INSERT INTO items (id, name, qty, version) VALUES ('` + ids.NewV7().String() + `', 'dup', 1, 1)`)
		if e, ok := errs.As(err); !ok || e.Kind != errs.KindConflict || e.Code != "ITEM_NAME_TAKEN" {
			t.Fatalf("unique: %v", err)
		}
		reset(db)
		if err := exec(`INSERT INTO items (id, name, qty, version) VALUES ('` + ids.NewV7().String() + `', 'neg', -1, 1)`); errs.CodeOf(err) != "CHECK_VIOLATION" {
			t.Fatalf("check: %v", err)
		}
		reset(db)
		if err := exec(`SELECT id FROM items WHERE id = 'not-a-uuid'`); errs.CodeOf(err) != "INVALID_DATA" {
			t.Fatalf("invalid data: %v", err)
		}
		reset(db)
		if err := exec(`SELECT nope FROM items`); errs.KindOf(err) != errs.KindInternal {
			t.Fatalf("bad query is a bug: %v", err)
		}
	})
	_ = sql.ErrNoRows
}
