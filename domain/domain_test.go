package domain_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/werhani-houssemeddine/go-archkit/domain"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/testkit"
)

type OrderPlaced struct {
	OrderID string  `json:"orderId"`
	Total   float64 `json:"total"`
}

var OrderPlacedEvent = domain.DefineEvent[OrderPlaced]("sales.order_placed.v1")

type Order struct {
	domain.AggregateRoot
	ID     string
	Lines  []float64
	Status string
}

func (o *Order) Place(ctx context.Context) error {
	if err := domain.FirstBroken(
		domain.Rule(o.Status != "draft", "ORDER_NOT_DRAFT", "only draft orders can be placed"),
		domain.Rule(len(o.Lines) == 0, "ORDER_EMPTY", "order has no lines"),
	); err != nil {
		return err
	}
	o.Status = "placed"
	total := 0.0
	for _, l := range o.Lines {
		total += l
	}
	o.Record(OrderPlacedEvent.New(ctx, OrderPlaced{OrderID: o.ID, Total: total}, domain.ForAggregate(o.ID, "Order")))
	return nil
}

func TestAggregateRulesEventsAndContext(t *testing.T) {
	o := &Order{ID: "o-1", Status: "draft"}
	if err := o.Place(context.Background()); errs.CodeOf(err) != "ORDER_EMPTY" || !errors.Is(err, errs.BusinessRule) {
		t.Fatalf("want ORDER_EMPTY business rule, got %v", err)
	}
	o.Lines = []float64{10, 5}
	ctx := execctx.WithActor(execctx.WithCorrelationID(context.Background(), "corr-9"), &execctx.Actor{ID: "u-7", TenantID: "t1"})
	if err := o.Place(ctx); err != nil {
		t.Fatal(err)
	}
	ev := o.PendingEvents()[0]
	if ev.Metadata["correlation_id"] != "corr-9" || ev.Metadata["actor_id"] != "u-7" || ev.Metadata["tenant_id"] != "t1" || ev.AggregateVersion != 1 {
		t.Fatalf("metadata: %+v", ev)
	}
	p, err := OrderPlacedEvent.Decode(ev)
	if err != nil || p.Total != 15 {
		t.Fatalf("decode in-memory: %v %+v", err, p)
	}
	// round trip through JSON (outbox / broker)
	raw, _ := json.Marshal(ev.Payload)
	ev.Payload = json.RawMessage(raw)
	if p2, err := OrderPlacedEvent.Decode(ev); err != nil || p2 != p {
		t.Fatalf("decode raw: %v %+v", err, p2)
	}
}

func TestMemoryRepositoryOptimisticLocking(t *testing.T) {
	pub := &testkit.Publisher{}
	repo := testkit.NewMemoryRepository[*Order, string]("Order", func(o *Order) string { return o.ID })
	repo.Publisher = pub
	ctx := context.Background()
	o := &Order{ID: "o-2", Status: "draft", Lines: []float64{1}}
	_ = o.Place(ctx)
	if err := repo.Save(ctx, o); err != nil {
		t.Fatal(err)
	}
	if o.Version() != 1 || len(o.PendingEvents()) != 0 || len(pub.OfType(OrderPlacedEvent.Type)) != 1 {
		t.Fatalf("after save: v=%d events=%d published=%d", o.Version(), len(o.PendingEvents()), len(pub.Events))
	}
	stale := &Order{ID: "o-2"}
	if err := repo.Save(ctx, stale); errs.CodeOf(err) != "CONCURRENT_MODIFICATION" {
		t.Fatalf("want conflict, got %v", err)
	}
	if _, err := repo.Get(ctx, "missing"); !errors.Is(err, errs.NotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestSpecsAndPagination(t *testing.T) {
	expensive := domain.Spec[float64](func(p float64) bool { return p > 100 })
	if got := expensive.Not().Filter([]float64{5, 500}); len(got) != 1 || got[0] != 5 {
		t.Fatalf("filter: %v", got)
	}
	req := domain.NewPageRequest(0, 1000, 20, 100)
	if req.Page != 1 || req.Size != 100 {
		t.Fatalf("clamp: %+v", req)
	}
	p := domain.NewPage([]int{1, 2}, 45, domain.PageRequest{Page: 2, Size: 20})
	if p.TotalPages != 3 || !p.HasNext || !p.HasPrev {
		t.Fatalf("page: %+v", p)
	}
	sort, err := domain.ParseSort("-createdAt,name", "createdAt", "name")
	if err != nil || !sort[0].Desc || sort[1].Field != "name" {
		t.Fatalf("sort: %v %v", sort, err)
	}
	if _, err := domain.ParseSort("password", "name"); !errors.Is(err, errs.Validation) {
		t.Fatal("unlisted sort fields must be rejected")
	}
	c, _ := domain.EncodeCursor(map[string]int{"id": 5})
	var back map[string]int
	if domain.DecodeCursor(c, &back) != nil || back["id"] != 5 {
		t.Fatal("cursor round trip")
	}
}
