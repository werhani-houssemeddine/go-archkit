package pg

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/domain"
	"github.com/werhani-houssemeddine/go-archkit/logx"
	"github.com/werhani-houssemeddine/go-archkit/resilience"
)

// OutboxDDL returns the DDL for the outbox table (put it in a migration).
func OutboxDDL(table string) string {
	if table == "" {
		table = "outbox_events"
	}
	t := QuoteIdent(table)
	return `CREATE TABLE IF NOT EXISTS ` + t + ` (
  id                uuid PRIMARY KEY,
  type              text        NOT NULL,
  aggregate_type    text,
  aggregate_id      text,
  aggregate_version integer,
  payload           jsonb       NOT NULL,
  metadata          jsonb       NOT NULL DEFAULT '{}'::jsonb,
  occurred_at       timestamptz NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  available_at      timestamptz NOT NULL DEFAULT now(),
  processed_at      timestamptz,
  failed_at         timestamptz,
  attempts          integer     NOT NULL DEFAULT 0,
  last_error        text
);
CREATE INDEX IF NOT EXISTS outbox_pending_idx ON ` + t + ` (available_at) WHERE processed_at IS NULL AND failed_at IS NULL;`
}

// Outbox writes events in the caller's transaction: an event is never lost, and never
// published for a rolled-back change.
type Outbox struct {
	db    *DB
	table string
}

// NewOutbox uses table (default "outbox_events").
func NewOutbox(db *DB, table string) *Outbox {
	if table == "" {
		table = "outbox_events"
	}
	QuoteIdent(table)
	return &Outbox{db: db, table: table}
}

// Append inserts events (idempotent on event id).
func (o *Outbox) Append(ctx context.Context, events ...domain.Event) error {
	if len(events) == 0 {
		return nil
	}
	ins := Insert(o.table, "id", "type", "aggregate_type", "aggregate_id", "aggregate_version", "payload", "metadata", "occurred_at")
	for _, e := range events {
		payload, err := json.Marshal(e.Payload)
		if err != nil {
			return err
		}
		meta, _ := json.Marshal(e.Metadata)
		ins.Values(e.ID, e.Type, nullStr(e.AggregateType), nullStr(e.AggregateID), nullInt(e.AggregateVersion), string(payload), string(meta), e.OccurredAt)
	}
	ins.OnConflictDoNothing("id")
	_, err := o.db.Exec(ctx, ins)
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// RelayOptions configure a Relay.
type RelayOptions struct {
	Table        string
	BatchSize    int           // default 100
	PollInterval time.Duration // default 1s
	MaxAttempts  int           // then the event is dead-lettered (failed_at set); default 10
	Logger       *slog.Logger
}

// Relay polls the outbox and publishes pending events at least once. Safe to run on
// several instances (rows are claimed with FOR UPDATE SKIP LOCKED). Handlers must be
// idempotent (deduplicate on event id).
type Relay struct {
	db   *DB
	pub  app.EventPublisher
	o    RelayOptions
	log  *slog.Logger
	stop context.CancelFunc
	wg   sync.WaitGroup
}

// NewRelay creates a relay.
func NewRelay(db *DB, pub app.EventPublisher, o RelayOptions) *Relay {
	if o.Table == "" {
		o.Table = "outbox_events"
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 100
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 10
	}
	return &Relay{db: db, pub: pub, o: o, log: logx.OrDiscard(o.Logger).With(slog.String("component", "outbox-relay"))}
}

// Start runs the polling loop in a goroutine until Stop or ctx is done.
func (r *Relay) Start(ctx context.Context) {
	ctx, r.stop = context.WithCancel(ctx)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			n, err := r.ProcessBatch(ctx)
			if err != nil && ctx.Err() == nil {
				r.log.ErrorContext(ctx, "outbox batch failed", logx.Err(err))
			}
			wait := r.o.PollInterval
			if n >= r.o.BatchSize {
				wait = 0 // backlog: drain quickly
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}

// Stop stops polling and waits for the current batch (use as a shutdown hook).
func (r *Relay) Stop(context.Context) error {
	if r.stop != nil {
		r.stop()
	}
	r.wg.Wait()
	return nil
}

type outboxRow struct {
	id, typ           string
	aggType, aggID    *string
	aggVersion        *int
	payload, metadata []byte
	occurredAt        time.Time
	attempts          int
}

// ProcessBatch claims and publishes one batch; returns how many events were handled.
func (r *Relay) ProcessBatch(ctx context.Context) (int, error) {
	n := 0
	err := r.db.Transact(ctx, func(ctx context.Context) error {
		q := Select(r.o.Table, "id", "type", "aggregate_type", "aggregate_id", "aggregate_version", "payload", "metadata", "occurred_at", "attempts").
			IsNull("processed_at").IsNull("failed_at").WhereExpr(Q("available_at <= now()")).
			OrderBy("created_at", false).Limit(r.o.BatchSize).ForUpdate(true)
		rows, err := All(ctx, r.db, q, func(s Scanner) (outboxRow, error) {
			var o outboxRow
			err := s.Scan(&o.id, &o.typ, &o.aggType, &o.aggID, &o.aggVersion, &o.payload, &o.metadata, &o.occurredAt, &o.attempts)
			return o, err
		})
		if err != nil {
			return err
		}
		n = len(rows)
		for _, row := range rows {
			e := domain.Event{ID: row.id, Type: row.typ, OccurredAt: row.occurredAt, Payload: json.RawMessage(row.payload)}
			_ = json.Unmarshal(row.metadata, &e.Metadata)
			if row.aggType != nil {
				e.AggregateType = *row.aggType
			}
			if row.aggID != nil {
				e.AggregateID = *row.aggID
			}
			if row.aggVersion != nil {
				e.AggregateVersion = *row.aggVersion
			}
			if perr := r.pub.Publish(ctx, e); perr != nil {
				attempts := row.attempts + 1
				msg := perr.Error()
				if len(msg) > 2000 {
					msg = msg[:2000]
				}
				if attempts >= r.o.MaxAttempts {
					r.log.ErrorContext(ctx, "outbox event dead-lettered", slog.String("event_id", row.id), slog.String("type", row.typ), slog.Int("attempts", attempts), logx.Err(perr))
					_, err = r.db.Exec(ctx, Update(r.o.Table).Set("failed_at", Q("now()")).Set("attempts", attempts).Set("last_error", msg).Eq("id", row.id))
				} else {
					delay := resilience.RetryPolicy{BaseDelay: time.Second, MaxDelay: 5 * time.Minute}.Delay(attempts)
					r.log.WarnContext(ctx, "outbox delivery failed, will retry", slog.String("event_id", row.id), slog.Int("attempts", attempts), slog.Duration("delay", delay), logx.Err(perr))
					_, err = r.db.Exec(ctx, Update(r.o.Table).Set("attempts", attempts).Set("last_error", msg).
						Set("available_at", Q("now() + (? * interval '1 millisecond')", delay.Milliseconds())).Eq("id", row.id))
				}
			} else {
				_, err = r.db.Exec(ctx, Update(r.o.Table).Set("processed_at", Q("now()")).Set("attempts", Q("attempts + 1")).Eq("id", row.id))
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}
