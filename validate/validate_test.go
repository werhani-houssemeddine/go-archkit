package validate_test

import (
	"strings"
	"testing"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/clock"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/validate"
)

type Address struct{ City, Zip string }
type Line struct {
	SKU string
	Qty int
}
type CreateOrder struct {
	Email    string
	Name     string
	Age      int
	Role     string
	Birth    time.Time
	Address  Address
	Lines    []Line
	Password string
}

func (c CreateOrder) Validate(now time.Time) error {
	v := validate.New().WithClock(clock.NewFixed(now))
	v.String("email", c.Email).Required().Email()
	v.String("name", c.Name).Required().Len(2, 50)
	validate.Number(v, "age", c.Age).Min(18)
	v.String("role", c.Role).OneOf("admin", "member")
	v.Time("birth", c.Birth).Past().MinAge(18)
	v.String("password", c.Password).Password(validate.DefaultPasswordPolicy)
	v.Nested("address", func(v *validate.V) {
		v.String("city", c.Address.City).Required()
		v.String("zip", c.Address.Zip).Custom(func(s string) bool { return len(s) == 4 }, "invalid_zip", "must have 4 digits")
	})
	validate.Slice(v, "lines", c.Lines).NotEmpty()
	validate.Unique(validate.Slice(v, "lines", c.Lines), func(l Line) string { return l.SKU })
	validate.Each(v, "lines", c.Lines, func(v *validate.V, l Line) {
		v.String("sku", l.SKU).Required()
		validate.Number(v, "qty", l.Qty).Positive()
	})
	return v.Err()
}

func TestCollectsEveryIssueWithPaths(t *testing.T) {
	now := time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)
	err := CreateOrder{
		Email: "nope", Name: "J", Age: 12, Role: "root", Birth: time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC),
		Address: Address{Zip: "123"}, Lines: []Line{{SKU: "A", Qty: 1}, {SKU: "A", Qty: 0}}, Password: "abc",
	}.Validate(now)
	e, ok := errs.As(err)
	if !ok || e.Kind != errs.KindValidation {
		t.Fatalf("want validation error, got %v", err)
	}
	var got []string
	for _, is := range e.Issues {
		got = append(got, is.Path+":"+is.Code)
	}
	want := []string{"email:invalid_email", "name:too_short", "age:too_small", "role:invalid_enum", "birth:too_young",
		"password:weak_password", "address.city:required", "address.zip:invalid_zip", "lines[1]:not_unique", "lines[1].qty:not_positive"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("issues:\n got  %v\n want %v", got, want)
	}
}

func TestValidInputAndOptionalSemantics(t *testing.T) {
	v := validate.New()
	v.String("nickname", "").Min(3).Email() // empty optional field: format rules skipped
	v.String("site", "https://example.com").URL()
	v.String("id", "01890a5d-ac96-774b-bcce-b302099a8057").UUID() // v7 accepted
	var missing *string
	v.OptionalString("bio", missing).Max(10)
	if err := v.Err(); err != nil {
		t.Fatalf("unexpected %v", err)
	}
}

func TestMergeNestedValidationErrors(t *testing.T) {
	v := validate.New()
	v.Merge("shipping", errs.InvalidField("zip", "required", "is required"))
	if v.Issues()[0].Path != "shipping.zip" {
		t.Fatalf("got %v", v.Issues())
	}
}
