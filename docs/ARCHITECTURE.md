# Architecture

## Layers and the dependency rule

```
 presentation   httpx                       ─┐
 infrastructure pg, security, jwtauth,       │  depend inwards only
                cache, mail, events, logx    │
 application    app (ports), cqrs, authz     │
 domain         domain                       │
 core           errs, validate, ids, clock, execctx ┘
```

- **Domain packages** import only `domain`, `errs`, `ids` and `clock`. Aggregates embed `domain.AggregateRoot`, return `error`s built with `domain.Rule` or `errs.Define`, and record events with `EventDef.New`.
- **Application packages** depend on the *ports* in `app` (`UnitOfWork`, `EventPublisher`, `PasswordHasher`, `TokenService`, `Mailer`, `Cache`) and on `domain.Repository[A, ID]`, never on `pg`, `net/http` or JWT libraries.
- **Infrastructure** implements the ports. `pg.DB` is an `app.UnitOfWork`, `pg.Repository` is a `domain.Repository`, `events.Bus` is an `app.EventBus`, `jwtauth.Service` is an `app.TokenService`.
- **Presentation** (`httpx`) binds and validates input, calls the mediator or use cases, and turns errors into problem+json. The kind → status mapping lives only in `httpx.ErrorMapper`.
- **`main`** is the composition root: the only place that knows concrete types. Go favors explicit wiring, so there is no DI container. Constructors take their dependencies as arguments.

## Recommended layout

```
cmd/orders-api/main.go              composition root
internal/orders/
    domain/      order.go, errors.go, events.go, repository.go (port)
    app/         place_order.go, get_order.go (commands/queries + handlers)
    infra/       pg_repository.go (pg.Mapping), mappers
    http/        routes.go (httpx.Handle), dto.go
internal/billing/ …
migrations/      0001_init.sql, 0002_outbox.sql    (//go:embed)
```

Bounded contexts communicate through **events** (outbox → relay → bus or broker) or **queries through the mediator**, never by importing another module's repository.

## Design decisions

| Decision | Why |
|---|---|
| `*errs.Error` with `Kind` + `Code` | Transport-independent semantics. HTTP, gRPC and queue consumers map `Kind` their own way, and clients and i18n rely on `Code`. Works with `errors.Is`/`As`. |
| Errors are values, created by catalogs (`errs.Define`) | Each bounded context declares its errors in one place. `errors.Is(err, ErrX)` matches by code across wrapping. |
| Request data in `context.Context` (`execctx`) | The idiomatic Go carrier. Logs (`logx.ContextHandler`), events, audit records and authorization read it without extra parameters. |
| Ambient transactions in the context | `db.Transact` puts the `*sql.Tx` in ctx, so repositories need no tx parameter, nested calls become savepoints and `AfterCommit` defers side effects. |
| Transactional outbox + relay | Events are committed atomically with state changes and delivered at least once. `FOR UPDATE SKIP LOCKED` allows several relay instances, with backoff and dead-lettering. |
| Optimistic locking in `pg.Repository` | Lost updates become `CONCURRENT_MODIFICATION` conflicts instead of silent overwrites. |
| `database/sql` instead of a specific driver | Teams choose pgx or lib/pq. Error translation uses the drivers' common `SQLState()` method. |
| Typed HTTP handlers on `http.ServeMux` | No framework lock-in. Input binding, validation and error mapping are uniform, and route patterns appear in logs. |
| PBKDF2-SHA256 from the standard library | FIPS-approved and needs no cgo. Parameters are stored in the hash, `NeedsRehash` supports upgrades, and `ChainedHasher` migrates from bcrypt or argon2 hashes. |
| AES-256-GCM with key ids | Authenticated encryption, key rotation without downtime, and associated data binds a ciphertext to its row. |
| Panics only for programmer errors | Examples: an `UPDATE` without `WHERE`, an unsafe identifier, a duplicate handler registration. These are wiring bugs and must fail fast in tests. Runtime failures are returned as errors. |

## Production checklist

1. `config.Load` into a struct. Mark secrets with `secret:"true"` and log `config.Describe(cfg)`.
2. `logx.New` with JSON output and `service`/`version` attrs. Always use the `*Context` logging methods.
3. `pg.Open` then `SetMaxOpenConns`, `Connect`, and `Migrator.Up` from `embed.FS`.
4. Repositories built with `pg.NewRepository(…, pg.WithOutbox(outbox))`, plus `pg.NewRelay(...).Start(ctx)`.
5. A `cqrs.New` pipeline: `Recover`, `Logging`, `Authorization`, `Validation`, `Transaction(db)`.
6. A router with `RequestContext`, `Recover` and `AccessLog`, plus `SecurityHeaders`, `CORS` if needed, `Timeout` and `Authenticate` on the API group.
7. `lifecycle.Health` with `db.HealthCheck()`, served by `httpx.RegisterHealth` and wired to `IsShuttingDown`.
8. `lifecycle.Manager` hooks in order: http (10), relay (20), bus drain (30), db (90). Finish with `Wait(ctx)`.
9. Unit tests with `testkit` fakes, integration tests with a real Postgres (`PG_TEST_DSN`), and run everything with `-race`.
