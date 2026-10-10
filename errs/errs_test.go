package errs_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/werhani-houssemeddine/go-archkit/errs"
)

var ErrEmailInUse = errs.Define(errs.KindConflict, "ACCOUNT_EMAIL_IN_USE", "email %s is already in use")

func TestKindsCodesAndStdlibInterop(t *testing.T) {
	e := errs.ResourceNotFound("User", 42)
	if e.Code != "USER_NOT_FOUND" || e.Message != "User '42' not found" || e.Kind != errs.KindNotFound {
		t.Fatalf("unexpected %+v", e)
	}
	wrapped := fmt.Errorf("loading profile: %w", e)
	if !errors.Is(wrapped, errs.NotFound) {
		t.Fatal("kind sentinel should match through wrapping")
	}
	if errors.Is(wrapped, errs.Conflict) {
		t.Fatal("must not match another kind")
	}
	if got, ok := errs.As(wrapped); !ok || got != e {
		t.Fatal("As should extract the *Error")
	}
	if errs.KindOf(context.DeadlineExceeded) != errs.KindTimeout || errs.KindOf(errors.New("x")) != errs.KindInternal {
		t.Fatal("KindOf fallback mapping")
	}
}

func TestCatalogDefinitions(t *testing.T) {
	err := fmt.Errorf("register: %w", ErrEmailInUse.Newf("a@b.co"))
	if !errors.Is(err, ErrEmailInUse) {
		t.Fatal("errors.Is should match catalog definition by code")
	}
	e, _ := errs.As(err)
	if e.Message != "email a@b.co is already in use" || e.Kind != errs.KindConflict {
		t.Fatalf("got %+v", e)
	}
	if errors.Is(errs.New(errs.KindConflict, "OTHER", ""), ErrEmailInUse) {
		t.Fatal("different code must not match")
	}
}

func TestCopiesAreImmutable(t *testing.T) {
	base := errs.New(errs.KindInternal, "", "boom")
	c := base.WithMeta("sql", "SELECT 1").WithCause(errors.New("root"))
	if base.Meta != nil || base.Unwrap() != nil {
		t.Fatal("With* must not mutate the original")
	}
	if c.Meta["sql"] != "SELECT 1" || !errors.Is(c, base) {
		t.Fatal("copy keeps code and gains meta")
	}
}

func TestValidationIssues(t *testing.T) {
	e := errs.Invalid(errs.Issue{Path: "email", Code: "invalid_email", Message: "bad"}, errs.Issue{Path: "items[0].qty", Code: "too_small", Message: "small"})
	fe := e.FieldErrors()
	if len(fe["email"]) != 1 || len(fe["items[0].qty"]) != 1 || e.Code != "VALIDATION_FAILED" {
		t.Fatalf("got %v", fe)
	}
	if errs.From(context.Canceled).Kind != errs.KindTimeout {
		t.Fatal("context errors normalize to timeout")
	}
}
