package cqrs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/werhani-houssemeddine/go-archkit/authz"
	"github.com/werhani-houssemeddine/go-archkit/cqrs"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/testkit"
	"github.com/werhani-houssemeddine/go-archkit/validate"
)

type PlaceOrder struct {
	SKU string
	Qty int
}

func (PlaceOrder) Permissions() []string { return []string{"orders:create"} }
func (c PlaceOrder) Validate() error {
	v := validate.New()
	v.String("sku", c.SKU).Required()
	validate.Number(v, "qty", c.Qty).Positive()
	return v.Err()
}

type GetOrder struct {
	cqrs.Query
	ID string
}

type OrderID string

func setup() (*cqrs.Mediator, *testkit.UnitOfWork) {
	uow := &testkit.UnitOfWork{}
	m := cqrs.New(cqrs.Recover(), cqrs.Logging(nil), cqrs.Authorization(), cqrs.Validation(), cqrs.Transaction(uow))
	cqrs.Handle(m, func(ctx context.Context, c PlaceOrder) (OrderID, error) { return OrderID(c.SKU), nil })
	cqrs.Handle(m, func(ctx context.Context, q GetOrder) (string, error) {
		if q.ID == "boom" {
			panic("handler bug")
		}
		return "order " + q.ID, nil
	})
	return m, uow
}

func TestPipeline(t *testing.T) {
	m, uow := setup()
	ctx := execctx.WithActor(context.Background(), &execctx.Actor{ID: "u", Permissions: []string{"orders:*"}})
	id, err := cqrs.Send[OrderID](ctx, m, PlaceOrder{SKU: "A", Qty: 2})
	if err != nil || id != "A" || uow.Transactions != 1 {
		t.Fatalf("command: %v %v tx=%d", id, err, uow.Transactions)
	}
	s, err := cqrs.Send[string](ctx, m, GetOrder{ID: "7"})
	if err != nil || s != "order 7" || uow.Transactions != 1 {
		t.Fatalf("query must not open a transaction: %v %v", s, err)
	}
}

func TestPipelineRejections(t *testing.T) {
	m, _ := setup()
	if _, err := cqrs.Send[OrderID](context.Background(), m, PlaceOrder{SKU: "A", Qty: 1}); !errors.Is(err, errs.Unauthenticated) {
		t.Fatalf("anonymous: %v", err)
	}
	noPerm := execctx.WithActor(context.Background(), &execctx.Actor{ID: "u"})
	if _, err := cqrs.Send[OrderID](noPerm, m, PlaceOrder{SKU: "A", Qty: 1}); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("forbidden: %v", err)
	}
	admin := execctx.WithActor(context.Background(), &execctx.Actor{ID: "u", Permissions: []string{"*"}})
	_, err := cqrs.Send[OrderID](admin, m, PlaceOrder{})
	if e, ok := errs.As(err); !ok || len(e.Issues) != 2 {
		t.Fatalf("validation: %v", err)
	}
	if _, err := cqrs.Send[string](admin, m, GetOrder{ID: "boom"}); errs.KindOf(err) != errs.KindInternal {
		t.Fatalf("panic must become internal error: %v", err)
	}
	if _, err := cqrs.Send[string](admin, m, struct{}{}); err == nil {
		t.Fatal("unregistered request must fail")
	}
}

func TestRBAC(t *testing.T) {
	rbac, err := authz.NewRBAC(map[string]authz.Role{
		"viewer": {Permissions: []string{"orders:read"}},
		"agent":  {Permissions: []string{"orders:update"}, Inherits: []string{"viewer"}},
		"admin":  {Permissions: []string{"*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := &execctx.Actor{ID: "a", Roles: []string{"agent"}}
	if !rbac.Can(agent, "orders:read", "orders:update") || rbac.Can(agent, "orders:delete") {
		t.Fatal("inheritance")
	}
	if authz.Matches("orders:*", "ordersarchive:read") || !authz.Matches("orders:*", "orders:items:write") || !authz.Matches("*:read", "users:read") {
		t.Fatal("wildcards")
	}
	if _, err := authz.NewRBAC(map[string]authz.Role{"a": {Inherits: []string{"b"}}, "b": {Inherits: []string{"a"}}}); err == nil {
		t.Fatal("cycle must be rejected")
	}
	type Doc struct{ Author string }
	canEdit := authz.Any(authz.Owner(func(d Doc) string { return d.Author }), authz.HasRole[Doc]("editor"))
	ctx := execctx.WithActor(context.Background(), &execctx.Actor{ID: "u2"})
	if err := authz.Authorize(ctx, canEdit, Doc{Author: "u1"}); !errors.Is(err, errs.Forbidden) {
		t.Fatal("non-owner must be forbidden")
	}
}
