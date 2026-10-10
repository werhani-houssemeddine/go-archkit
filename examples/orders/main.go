// Command orders is a complete, compilable example of a service wired with archkit:
// config → logger → Postgres (migrations, outbox) → repository → CQRS mediator →
// HTTP (auth, validation, problem+json) → health → graceful shutdown.
//
// Layers are in one file for readability; in a real service put them in
// internal/orders/{domain,app,infra,http} packages.
package main

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq" // any database/sql Postgres driver works (pgx/stdlib recommended)

	"github.com/werhani-houssemeddine/go-archkit/authz"
	"github.com/werhani-houssemeddine/go-archkit/config"
	"github.com/werhani-houssemeddine/go-archkit/cqrs"
	"github.com/werhani-houssemeddine/go-archkit/domain"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/events"
	"github.com/werhani-houssemeddine/go-archkit/httpx"
	"github.com/werhani-houssemeddine/go-archkit/ids"
	"github.com/werhani-houssemeddine/go-archkit/lifecycle"
	"github.com/werhani-houssemeddine/go-archkit/logx"
	"github.com/werhani-houssemeddine/go-archkit/pg"
	"github.com/werhani-houssemeddine/go-archkit/resilience"
	"github.com/werhani-houssemeddine/go-archkit/security/jwtauth"
	"github.com/werhani-houssemeddine/go-archkit/validate"
)

/* ============================== domain ============================== */

var (
	ErrOrderNotDraft = errs.Define(errs.KindBusinessRule, "ORDER_NOT_DRAFT", "only draft orders can be modified")
	ErrOrderEmpty    = errs.Define(errs.KindBusinessRule, "ORDER_EMPTY", "an order needs at least one line")
)

type OrderPlaced struct {
	OrderID    string  `json:"orderId"`
	CustomerID string  `json:"customerId"`
	Total      float64 `json:"total"`
}

var OrderPlacedEvent = domain.DefineEvent[OrderPlaced]("orders.order_placed.v1")

type Line struct {
	SKU       string  `json:"sku"`
	Qty       int     `json:"qty"`
	UnitPrice float64 `json:"unitPrice"`
}

type Order struct {
	domain.AggregateRoot
	ID         string
	CustomerID string
	Status     string
	Lines      []Line
}

func NewOrder(customerID string) *Order {
	return &Order{ID: ids.NewV7().String(), CustomerID: customerID, Status: "draft"}
}

func (o *Order) AddLine(l Line) error {
	if o.Status != "draft" {
		return ErrOrderNotDraft.New()
	}
	o.Lines = append(o.Lines, l)
	return nil
}

func (o *Order) Place(ctx context.Context) error {
	if len(o.Lines) == 0 {
		return ErrOrderEmpty.New()
	}
	o.Status = "placed"
	o.Record(OrderPlacedEvent.New(ctx, OrderPlaced{OrderID: o.ID, CustomerID: o.CustomerID, Total: o.Total()}, domain.ForAggregate(o.ID, "Order")))
	return nil
}

func (o *Order) Total() float64 {
	t := 0.0
	for _, l := range o.Lines {
		t += float64(l.Qty) * l.UnitPrice
	}
	return t
}

type OrderRepository = domain.Repository[*Order, string]

/* ============================ application =========================== */

type PlaceOrder struct {
	CustomerID string `json:"customerId"`
	Lines      []Line `json:"lines"`
}

func (PlaceOrder) Permissions() []string { return []string{"orders:create"} }

func (c PlaceOrder) Validate() error {
	v := validate.New()
	v.String("customerId", c.CustomerID).Required().UUID()
	validate.Slice(v, "lines", c.Lines).NotEmpty().Max(100)
	validate.Each(v, "lines", c.Lines, func(v *validate.V, l Line) {
		v.String("sku", l.SKU).Required().Max(20)
		validate.Number(v, "qty", l.Qty).Positive()
		validate.Number(v, "unitPrice", l.UnitPrice).NonNegative()
	})
	return v.Err()
}

type GetOrder struct {
	cqrs.Query
	ID string `path:"id"`
}

func (GetOrder) Permissions() []string { return []string{"orders:read"} }
func (q GetOrder) Validate() error {
	v := validate.New()
	v.String("id", q.ID).UUID()
	return v.Err()
}

type OrderView struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Total  float64 `json:"total"`
	Lines  []Line  `json:"lines"`
}

func RegisterHandlers(m *cqrs.Mediator, orders OrderRepository) {
	cqrs.Handle(m, func(ctx context.Context, c PlaceOrder) (string, error) {
		o := NewOrder(c.CustomerID)
		for _, l := range c.Lines {
			if err := o.AddLine(l); err != nil {
				return "", err
			}
		}
		if err := o.Place(ctx); err != nil {
			return "", err
		}
		return o.ID, orders.Save(ctx, o)
	})
	cqrs.Handle(m, func(ctx context.Context, q GetOrder) (OrderView, error) {
		o, err := orders.Get(ctx, q.ID)
		if err != nil {
			return OrderView{}, err
		}
		return OrderView{ID: o.ID, Status: o.Status, Total: o.Total(), Lines: o.Lines}, nil
	})
}

/* =========================== infrastructure ========================= */

//go:embed migrations/*.sql
var migrationsFS embed.FS

func NewOrderRepository(db *pg.DB, outbox *pg.Outbox) *pg.Repository[*Order, string] {
	return pg.NewRepository(db, pg.Mapping[*Order, string]{
		Table: "orders", Resource: "Order", VersionColumn: "version",
		Columns: []string{"id", "customer_id", "status", "lines", "version"},
		Scan: func(s pg.Scanner) (*Order, error) {
			var o Order
			var lines pg.JSON[[]Line]
			var version int
			if err := s.Scan(&o.ID, &o.CustomerID, &o.Status, &lines, &version); err != nil {
				return nil, err
			}
			o.Lines = lines.V
			o.Restore(version)
			return &o, nil
		},
		Values: func(o *Order) map[string]any {
			return map[string]any{"id": o.ID, "customer_id": o.CustomerID, "status": o.Status, "lines": pg.JSON[[]Line]{V: o.Lines}}
		},
		ID: func(o *Order) string { return o.ID },
	}, pg.WithOutbox(outbox))
}

/* ============================ presentation ========================== */

func RegisterRoutes(api *httpx.Group, m *cqrs.Mediator) {
	httpx.Handle(api, "POST /orders", func(ctx context.Context, in PlaceOrder) (httpx.Result[map[string]string], error) {
		id, err := cqrs.Send[string](ctx, m, in)
		if err != nil {
			return httpx.Result[map[string]string]{}, err
		}
		return httpx.Created(map[string]string{"id": id}, "/api/v1/orders/"+id), nil
	})
	httpx.Handle(api, "GET /orders/{id}", func(ctx context.Context, in GetOrder) (OrderView, error) {
		return cqrs.Send[OrderView](ctx, m, in)
	})
}

/* ========================== composition root ======================== */

type Config struct {
	Env         string        `env:"APP_ENV" default:"development" oneof:"development,staging,production"`
	Addr        string        `env:"HTTP_ADDR" default:":8080"`
	LogLevel    string        `env:"LOG_LEVEL" default:"info" oneof:"debug,info,warn,error"`
	DatabaseURL string        `env:"DATABASE_URL" required:"true" url:"postgres,postgresql" secret:"true"`
	JWTSecret   string        `env:"JWT_SECRET" required:"true" min:"32" secret:"true"`
	JWTIssuer   string        `env:"JWT_ISSUER" default:"auth"`
	ReqTimeout  time.Duration `env:"HTTP_REQUEST_TIMEOUT" default:"15s"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", logx.Err(err))
		os.Exit(1)
	}
}

func run() error {
	var cfg Config
	if err := config.Load(&cfg); err != nil {
		return err
	}
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	format := "json"
	if cfg.Env == "development" {
		format = "text"
	}
	log := logx.New(logx.Options{Level: level, Format: format, Attrs: []slog.Attr{slog.String("service", "orders")}})
	log.Info("starting", slog.Any("config", config.Describe(cfg)))

	ctx := context.Background()
	db, err := pg.Open("postgres", cfg.DatabaseURL, pg.Options{Logger: log})
	if err != nil {
		return err
	}
	db.SQLDB().SetMaxOpenConns(20)
	if err := db.Connect(ctx); err != nil {
		return err
	}
	sub, _ := fs.Sub(migrationsFS, "migrations")
	if _, err := pg.NewMigrator(db, sub, pg.MigratorOptions{Logger: log}).Up(ctx); err != nil {
		return err
	}

	bus := events.NewBus(events.Options{Logger: log, Retry: &resilience.RetryPolicy{Retries: 3}})
	events.On(bus, OrderPlacedEvent, "notify-sales", func(ctx context.Context, _ domain.Event, p OrderPlaced) error {
		log.InfoContext(ctx, "order placed", slog.String("order_id", p.OrderID), slog.Float64("total", p.Total))
		return nil
	})
	outbox := pg.NewOutbox(db, "")
	relay := pg.NewRelay(db, bus, pg.RelayOptions{Logger: log})
	relay.Start(ctx)

	orders := NewOrderRepository(db, outbox)
	mediator := cqrs.New(cqrs.Recover(), cqrs.Logging(log), cqrs.Authorization(), cqrs.Validation(), cqrs.Transaction(db))
	RegisterHandlers(mediator, orders)

	tokens, err := jwtauth.New(jwtauth.Options{Secret: []byte(cfg.JWTSecret), Issuer: cfg.JWTIssuer, Audience: []string{"orders-api"}})
	if err != nil {
		return err
	}
	rbac := authz.MustRBAC(map[string]authz.Role{
		"customer": {Permissions: []string{"orders:create", "orders:read"}},
		"support":  {Permissions: []string{"orders:read"}},
		"admin":    {Permissions: []string{"*"}},
	})

	mapper := &httpx.ErrorMapper{Logger: log, Expose: cfg.Env == "development"}
	router := httpx.NewRouter(mapper)
	router.Use(httpx.RequestContext(httpx.RequestContextOptions{}), httpx.Recover(mapper), httpx.AccessLog(log), httpx.SecurityHeaders(cfg.Env == "production"))
	api := router.Group("/api/v1", httpx.Timeout(cfg.ReqTimeout), httpx.Authenticate(tokens, mapper, httpx.AuthOptions{RBAC: rbac}))
	RegisterRoutes(api, mediator)

	lc := lifecycle.NewManager(log)
	health := (&lifecycle.Health{}).Register(db.HealthCheck("postgres"))
	httpx.RegisterHealth(router, "/health", health, lc.IsShuttingDown)

	srv := &http.Server{Addr: cfg.Addr, Handler: router, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		log.Info("listening", slog.String("addr", cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server failed", logx.Err(err))
		}
	}()

	lc.OnShutdown("http", 10, srv.Shutdown)
	lc.OnShutdown("outbox-relay", 20, relay.Stop)
	lc.OnShutdown("event-bus", 30, func(context.Context) error { bus.Drain(); return nil })
	lc.OnShutdown("postgres", 90, func(context.Context) error { return db.Close() })
	return lc.Wait(ctx)
}
