# Feature mapping: TypeScript archkit → Go archkit

The Go library offers the same capabilities, implemented with Go idioms rather than translated line by line.

| Capability | TypeScript | Go | Go-specific notes |
|---|---|---|---|
| Error model | `AppError` + subclasses, `kind`, `code` | `*errs.Error` with `Kind`, `Code`, `Issues`, `Meta` | `errors.Is(err, errs.NotFound)`, `errors.Is(err, ErrDef)` |
| Error catalogs | `defineErrors("NS", {...})` | `errs.Define(kind, code, msg)` | `Def.Newf(args...)`, matches with `errors.Is` |
| Result type | `Result<T, E>` | multiple return values `(T, error)` | Go's native idiom; no Result type needed |
| Validation | schema builder `v.object(...)` with type inference | fluent accumulating `validate.V` + `Validatable` | Go has no structural type inference; DTO structs are the schema |
| IDs | `uuidv7()` | `ids.NewV7()`, `ids.Generator` | monotonic and safe under concurrency (tested with `-race`) |
| Clock | `Clock`, `FixedClock` | `clock.Clock`, `clock.Fixed` | |
| Execution context | `AsyncLocalStorage` | `context.Context` (`execctx`) | `Detach` for work that outlives a request |
| Aggregates / events | `AggregateRoot`, `defineEvent` | embedded `domain.AggregateRoot`, `domain.DefineEvent[P]` | `Decode` handles in-memory and JSON payloads |
| Business rules / specs | `rule()`, `Specification` | `domain.Rule`, `domain.Spec[T]` | |
| Pagination | `Pagination`, `Page<T>` | `domain.Page[T]`, `ParseSort`, cursors | |
| Mediator / CQRS | `Mediator`, behaviors | `cqrs.Mediator`, `Handle`, `Send[T]` | request capabilities via interfaces (`Permissions`, `Validate`, `cqrs.Query`) |
| Authorization | `Rbac`, policies | `authz.RBAC`, `Policy[R]` | |
| DI container | `Container`, tokens | **intentionally omitted** | explicit constructor wiring in `main` is idiomatic Go |
| Config | `loadConfig(schema)` | `config.Load(&cfg)` with struct tags | |
| Logging | `StructuredLogger`, transports | `log/slog` + `logx` handlers | redaction, context enrichment, fan-out, rotating file |
| Audit | `AuditLogger` | `logx.Auditor` | |
| Event bus | `InMemoryEventBus` | `events.Bus` | goroutine per handler, panic-safe, async mode with `Drain` |
| Resilience | retry, breaker, bulkhead, rate limiter, fallback | same, in `resilience` | channel-based bulkhead, `MapConcurrent` |
| Lifecycle / health | `Lifecycle`, `HealthRegistry` | `lifecycle.Manager`, `lifecycle.Health` | `Wait` uses `signal.NotifyContext` |
| Postgres access | `pg` driver, `PostgresDatabase` | `database/sql`, `pg.DB` | driver-agnostic (pgx/lib/pq) |
| Query builder / SQL tag | `qb`, `sql` tag | `pg.Select/Insert/Update/Delete`, `pg.Q` | `?` placeholders rendered to `$n`; identifiers validated |
| Repository | `PostgresRepository` subclass | `pg.Repository[A, ID]` from a `Mapping` | generics replace inheritance |
| Outbox / relay / migrations | yes | yes | migrations read from `fs.FS` (`//go:embed`) |
| Encryption | `AesGcmEncryptor` | `security.AESGCM` | |
| Password hashing | scrypt, bcrypt adapter | PBKDF2-SHA256 (stdlib, FIPS), `ChainedHasher` | plug argon2/bcrypt via `app.PasswordHasher` |
| JWT | `JwtTokenService` | `jwtauth.Service` | |
| Cache | `InMemoryCache`, `RedisCache` | `cache.Memory`, `cache.Redis`, `cache.Typed[T]` | |
| Mail | Nodemailer adapter | stdlib SMTP mailer + MIME builder | header-injection safe |
| HTTP | `HttpRouter`, `HttpApplication`, Express adapter | `httpx.Router` on `http.ServeMux`, `httpx.Handle[In,Out]` | no framework; problem+json 404/405 |
| Phone numbers | `archkit/phone` | not included | use `github.com/nyaruka/phonenumbers` behind a `validate` `Custom` rule |
| Test fakes | `archkit/testing` | `testkit` | |
