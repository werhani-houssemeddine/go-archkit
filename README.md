# archkit (Go)

Building blocks for **enterprise Go services built with Clean Architecture / DDD**:

- **Core:** one error model, accumulating validation, domain primitives.
- **Application:** a CQRS mediator with behaviors and authorization.
- **Cross-cutting:** structured logging, resilience, events, config, lifecycle.
- **Infrastructure:** PostgreSQL (ambient transactions, optimistic locking, transactional outbox, migrations), `net/http` (typed handlers, problem+json), crypto and JWT, caching and mail.

- **Idiomatic Go.** Uses `context.Context` for request data, `errors.Is`/`errors.As`, `log/slog`, `net/http` routing patterns, `database/sql`, `fs.FS` + `embed`, and generics where they remove boilerplate. There is no DI container, no reflection-heavy framework and no global state.
- **Minimal dependencies.** The only third-party package is `github.com/golang-jwt/jwt/v5`, and only `security/jwtauth` uses it. Password hashing uses the standard library's FIPS-approved `crypto/pbkdf2`; encryption, UUIDv7, SMTP and HTTP are standard library too. Postgres works with **any** `database/sql` driver (pgx's stdlib adapter, lib/pq).
- **Project-agnostic.** The library never reads environment variables, never calls `os.Exit` and contains no business types.
- **Tested.** Every package is checked with `go vet` and `go test -race`. Integration tests run against a real PostgreSQL engine, and the example service was exercised end-to-end over HTTP.

Requires **Go 1.24+**.

```bash
go get github.com/werhani-houssemeddine/go-archkit
```

## Packages

| Package | Layer | Provides |
|---|---|---|
| `errs` | core | `*errs.Error` with `Kind` (validation, not_found, conflict, forbidden…), stable `Code`, `Issues`, `Details`, `Meta`; kind sentinels for `errors.Is`; `Define` catalogs; `From` normalization |
| `validate` | core | fluent, **accumulating** validator: strings, numbers (generic), times, slices, nested paths `items[2].qty`; `Validatable` interface |
| `ids`, `clock` | core | UUIDv4/**v7** (monotonic), `Generator` and `Clock` interfaces with test fakes |
| `execctx` | core | correlation/request/causation ids, `Actor`, tenant and locale carried in `context.Context` |
| `domain` | domain | `AggregateRoot` (events and version), typed `DefineEvent[P]`, `Rule`, `Spec[T]`, `Repository[A,ID]` port, `Page[T]`, `ParseSort`, cursors |
| `app` | application | ports: `UnitOfWork`, `EventBus`, `PasswordHasher`, `Encryptor`, `TokenService`, `Mailer`, `Cache`, `UseCase` |
| `cqrs` | application | `Mediator` with `Handle`/`Send[T]` and behaviors: `Logging`, `Validation`, `Authorization`, `Transaction`, `Recover` |
| `authz` | application | wildcard permissions (`orders:*`), `RBAC` with inheritance, resource `Policy[R]` combinators |
| `events` | infra | in-process `Bus`: typed `On`, isolated handlers, retries, dead-letter hook, async with `Drain` |
| `resilience` | infra | `Retry` (backoff and jitter), `CircuitBreaker`, `Bulkhead`, `TokenBucket` rate limiter, `Fallback`, `WithTimeout`, `MapConcurrent` |
| `logx` | infra | `slog` setup (JSON/text), redaction, execctx enrichment, error rendering, `Fanout`, `RotatingFile`, `Auditor` |
| `config` | infra | env → struct via tags (`env`, `default`, `required`, `min`, `max`, `oneof`, `url`, `secret`, `prefix`), all problems reported at once |
| `lifecycle` | infra | ordered graceful shutdown with timeouts and signal handling; `Health` checks |
| `pg` | infra | `DB` (UnitOfWork, savepoints, `AfterCommit`, error translation), safe query builder, generic `Repository` with optimistic locking, `Outbox` + `Relay`, `Migrator`, `JSON[T]` |
| `security` | infra | `AESGCM` (key rotation, AAD), `PBKDF2Hasher`, `ChainedHasher`, `VerifyPassword`, bias-free `RandomString`/`OTP`, `OpaqueToken`, HMAC webhooks |
| `security/jwtauth` | infra | JWT `TokenService`: pinned algorithm, issuer/audience, expired vs invalid, safe `Refresh`, HMAC/RSA/ECDSA/EdDSA |
| `cache` | infra | LRU+TTL `Memory`, driver-agnostic `Redis`, `Namespaced`, typed `GetOrLoad` with single-flight |
| `mail` | infra | SMTP mailer (STARTTLS/implicit TLS), MIME builder protected against header injection, `html/template` helpers |
| `httpx` | presentation | `Router`/`Group` on `http.ServeMux`, typed `Handle[In,Out]` with binding and validation, RFC 9457 `ErrorMapper`, middleware (request context, recover, access log, authenticate, require, rate limit, timeout, CORS, security headers, body limit), health endpoints |
| `testkit` | tests | `MemoryRepository`, `UnitOfWork`, `Publisher`, `Mailer`, `Hasher`, `Tokens` fakes |

## Quick tour

### Errors

```go
var ErrOrderLocked = errs.Define(errs.KindBusinessRule, "ORDER_LOCKED", "order %s is locked")

return ErrOrderLocked.Newf(id)                  // → HTTP 422, code ORDER_LOCKED
return errs.ResourceNotFound("Invoice", id)     // → HTTP 404, code INVOICE_NOT_FOUND

if errors.Is(err, ErrOrderLocked) { … }         // match a catalog entry
if errors.Is(err, errs.NotFound)  { … }         // match any not-found error
```

### Validation (every issue at once)

```go
func (c PlaceOrder) Validate() error {
    v := validate.New()
    v.String("customerId", c.CustomerID).Required().UUID()
    validate.Slice(v, "lines", c.Lines).NotEmpty().Max(100)
    validate.Each(v, "lines", c.Lines, func(v *validate.V, l Line) {
        v.String("sku", l.SKU).Required()
        validate.Number(v, "qty", l.Qty).Positive()
    })
    return v.Err()   // nil, or one KindValidation error with all issues
}
```

### Domain

```go
var OrderPlaced = domain.DefineEvent[OrderPlacedPayload]("orders.order_placed.v1")

type Order struct {
    domain.AggregateRoot
    ID string; Status string; Lines []Line
}

func (o *Order) Place(ctx context.Context) error {
    if err := domain.Rule(len(o.Lines) == 0, "ORDER_EMPTY", "order has no lines"); err != nil {
        return err
    }
    o.Status = "placed"
    o.Record(OrderPlaced.New(ctx, OrderPlacedPayload{OrderID: o.ID}, domain.ForAggregate(o.ID, "Order")))
    return nil
}
```

### Application: mediator pipeline

```go
m := cqrs.New(cqrs.Recover(), cqrs.Logging(log), cqrs.Authorization(), cqrs.Validation(), cqrs.Transaction(db))
cqrs.Handle(m, func(ctx context.Context, c PlaceOrder) (string, error) { … })
id, err := cqrs.Send[string](ctx, m, PlaceOrder{…})  // auth → validate → BEGIN … COMMIT → handler
```

Requests opt into behaviors through small interfaces: `Permissions() []string`, `Validate() error`, and embedding `cqrs.Query` marks a request read-only (no transaction).

### HTTP

```go
mapper := &httpx.ErrorMapper{Logger: log}
r := httpx.NewRouter(mapper)
r.Use(httpx.RequestContext(httpx.RequestContextOptions{}), httpx.Recover(mapper), httpx.AccessLog(log))
api := r.Group("/api/v1", httpx.Authenticate(tokens, mapper, httpx.AuthOptions{RBAC: rbac}))

type GetOrderIn struct{ ID string `path:"id"`; Expand bool `query:"expand"` }

httpx.Handle(api, "GET /orders/{id}", func(ctx context.Context, in GetOrderIn) (OrderView, error) {
    return cqrs.Send[OrderView](ctx, m, GetOrder{ID: in.ID})
}, httpx.Require(mapper, "orders:read"))
```

Errors come back as `application/problem+json` with the right status code, and validation lists every field. 5xx responses never leak internal messages.

### PostgreSQL

```go
db, _ := pg.Open("pgx", cfg.DatabaseURL, pg.Options{Logger: log, Constraints: pg.ConstraintMap{
    "users_email_key": {Field: "email", Message: "email already in use", Code: "EMAIL_TAKEN"},
}})
_ = db.Connect(ctx)

users, err := pg.All(ctx, db, pg.Select("users", "id", "email").Eq("status", "active").OrderBy("created_at", true).Page(1, 20), scanUser)

err = db.Transact(ctx, func(ctx context.Context) error {   // repositories using ctx join the tx
    if err := accounts.Save(ctx, from); err != nil { return err }
    db.AfterCommit(ctx, func(ctx context.Context) { _ = mailer.Send(ctx, receipt) })
    return accounts.Save(ctx, to)
})
```

### Composition root

[`examples/orders/main.go`](examples/orders/main.go) is a complete, runnable service:

- config, logger, Postgres with embedded migrations;
- repository with outbox, relay and event bus;
- mediator, JWT and RBAC;
- HTTP, health checks and graceful shutdown.

## Testing

```bash
make test               # go vet + go test -race ./...
make integration        # needs PG_TEST_DSN=postgres://user:pass@host:5432/db?sslmode=disable
```

The Postgres integration test exercises:

- migrations;
- constraint error translation;
- rollback, savepoints and after-commit hooks;
- optimistic locking;
- pagination;
- outbox delivery.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): layers, dependency rule, recommended layout, design decisions.
- [docs/FEATURES.md](docs/FEATURES.md): feature-by-feature mapping from the TypeScript `archkit`.

## Notes

- The module path `github.com/werhani-houssemeddine/go-archkit` is a placeholder: change it in `go.mod` and with a search-and-replace on imports.
- `lib/pq` appears in `go.mod` only for the example and tests. In production, prefer `github.com/jackc/pgx/v5/stdlib` (`pg.Open("pgx", dsn, …)`).
