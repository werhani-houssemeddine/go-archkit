package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/httpx"
	"github.com/werhani-houssemeddine/go-archkit/lifecycle"
	"github.com/werhani-houssemeddine/go-archkit/resilience"
	"github.com/werhani-houssemeddine/go-archkit/testkit"
	"github.com/werhani-houssemeddine/go-archkit/validate"
)

type GetUserIn struct {
	ID     string `path:"id"`
	Expand bool   `query:"expand"`
}

func (in GetUserIn) Validate() error {
	v := validate.New()
	v.String("id", in.ID).Matches(digits, "must be numeric")
	return v.Err()
}

type CreateUserIn struct {
	Name   string `json:"name"`
	Email  string `json:"email"`
	Notify bool   `query:"notify"`
}

func (in CreateUserIn) Validate() error {
	v := validate.New()
	v.String("name", in.Name).Required().Min(2)
	v.String("email", in.Email).Required().Email()
	return v.Err()
}

type UserOut struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	By          string `json:"by,omitempty"`
	Correlation string `json:"corr,omitempty"`
}

var digits = mustRe(`^\d+$`)

func newServer(t *testing.T) (*httptest.Server, *testkit.Tokens) {
	t.Helper()
	tokens := &testkit.Tokens{}
	m := &httpx.ErrorMapper{}
	r := httpx.NewRouter(m)
	r.Use(httpx.RequestContext(httpx.RequestContextOptions{}), httpx.Recover(m), httpx.SecurityHeaders(false))
	api := r.Group("/api", httpx.Authenticate(tokens, m, httpx.AuthOptions{Optional: true}))

	httpx.Handle(api, "GET /users/{id}", func(ctx context.Context, in GetUserIn) (UserOut, error) {
		if in.ID != "1" {
			return UserOut{}, errs.ResourceNotFound("User", in.ID)
		}
		return UserOut{ID: "1", Name: "Ali"}, nil
	})
	httpx.Handle(api, "POST /users", func(ctx context.Context, in CreateUserIn) (httpx.Result[UserOut], error) {
		out := UserOut{ID: "2", Name: in.Name, By: execctx.ActorFrom(ctx).ID, Correlation: execctx.CorrelationID(ctx)}
		return httpx.Created(out, "/api/users/2").WithCookie(&http.Cookie{Name: "seen", Value: "1"}), nil
	}, httpx.Require(m, "users:create"))
	httpx.Handle(api, "DELETE /users/{id}", func(ctx context.Context, in GetUserIn) (httpx.Empty, error) {
		return httpx.Empty{}, nil
	}, httpx.RequireRole(m, "admin"))
	api.HandleFunc("GET /errors/{kind}", func(w http.ResponseWriter, req *http.Request) error {
		switch req.PathValue("kind") {
		case "conflict":
			return errs.New(errs.KindConflict, "EMAIL_TAKEN", "email taken")
		case "rule":
			return errs.Rule("ORDER_EMPTY", "order has no lines")
		case "bug":
			return errors.New("nil pointer in db password=hunter2")
		case "panic":
			panic("boom")
		case "slow":
			<-req.Context().Done()
			return req.Context().Err()
		}
		return nil
	}, httpx.Timeout(20*time.Millisecond))
	limiter := &resilience.TokenBucket{Capacity: 1, Refill: 1, Interval: time.Minute}
	api.HandleFunc("GET /limited", func(w http.ResponseWriter, _ *http.Request) error {
		return httpx.WriteJSON(w, 200, map[string]bool{"ok": true})
	}, httpx.RateLimit(limiter, m, nil))
	h := (&lifecycle.Health{}).Register(lifecycle.Check{Name: "db", Critical: true, Fn: func(context.Context) error { return nil }})
	httpx.RegisterHealth(r, "/health", h, nil)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, tokens
}

func do(t *testing.T, method, url, body, token string, headers ...string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return res, m
}

func TestRoutingBindingAndErrors(t *testing.T) {
	srv, _ := newServer(t)
	res, body := do(t, "GET", srv.URL+"/api/users/1?expand=true", "", "")
	if res.StatusCode != 200 || body["name"] != "Ali" || res.Header.Get("X-Request-ID") == "" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %v", res.StatusCode, body)
	}
	cases := []struct {
		path   string
		status int
		code   string
	}{
		{"/api/users/999", 404, "USER_NOT_FOUND"},
		{"/api/users/abc", 400, "VALIDATION_FAILED"},
		{"/api/users/1?expand=maybe", 400, "VALIDATION_FAILED"},
		{"/api/errors/conflict", 409, "EMAIL_TAKEN"},
		{"/api/errors/rule", 422, "ORDER_EMPTY"},
		{"/api/errors/bug", 500, "INTERNAL_ERROR"},
		{"/api/errors/panic", 500, "INTERNAL_ERROR"},
		{"/api/errors/slow", 504, "TIMEOUT"},
		{"/nope", 404, "ROUTE_NOT_FOUND"},
	}
	for _, c := range cases {
		res, body := do(t, "GET", srv.URL+c.path, "", "")
		if res.StatusCode != c.status || body["code"] != c.code || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: got %d %v", c.path, res.StatusCode, body)
		}
		if strings.Contains(body["detail"].(string), "hunter2") {
			t.Errorf("internal details leaked: %v", body)
		}
	}
	res, body = do(t, "PUT", srv.URL+"/api/users/1", "", "")
	if res.StatusCode != 405 || body["code"] != "METHOD_NOT_ALLOWED" || !strings.Contains(res.Header.Get("Allow"), "GET") {
		t.Fatalf("405: %d %v %q", res.StatusCode, body, res.Header.Get("Allow"))
	}
}

func TestAuthValidationAndCookies(t *testing.T) {
	srv, tokens := newServer(t)
	ctx := context.Background()
	payload := `{"name":"Sarah","email":"s@x.io"}`
	if res, _ := do(t, "POST", srv.URL+"/api/users", payload, ""); res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous: %d", res.StatusCode)
	}
	noPerm, _ := tokens.Issue(ctx, app.Claims{Subject: "u1"}, 0)
	if res, _ := do(t, "POST", srv.URL+"/api/users", payload, noPerm); res.StatusCode != 403 {
		t.Fatalf("forbidden: %d", res.StatusCode)
	}
	if res, _ := do(t, "POST", srv.URL+"/api/users", payload, "garbage"); res.StatusCode != 401 {
		t.Fatalf("invalid token: %d", res.StatusCode)
	}
	ok, _ := tokens.Issue(ctx, app.Claims{Subject: "u1", Permissions: []string{"users:*"}}, 0)
	res, body := do(t, "POST", srv.URL+"/api/users?notify=true", payload, ok, "X-Correlation-ID", "corr-123")
	if res.StatusCode != 201 || res.Header.Get("Location") != "/api/users/2" || body["by"] != "u1" || body["corr"] != "corr-123" {
		t.Fatalf("create: %d %v", res.StatusCode, body)
	}
	cookie := res.Header.Get("Set-Cookie")
	if !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "Secure") || !strings.Contains(cookie, "SameSite=Lax") {
		t.Fatalf("cookie: %s", cookie)
	}
	// every problem reported at once, unknown fields rejected
	res, body = do(t, "POST", srv.URL+"/api/users", `{"name":"x","email":"bad"}`, ok)
	if res.StatusCode != 400 || len(body["errors"].([]any)) != 2 {
		t.Fatalf("validation: %v", body)
	}
	if res, body = do(t, "POST", srv.URL+"/api/users", `{"name":"Sam","email":"s@x.io","admin":true}`, ok); res.StatusCode != 400 {
		t.Fatalf("unknown field: %v", body)
	}
	if res, body = do(t, "POST", srv.URL+"/api/users", `{oops`, ok); body["code"] != "MALFORMED_REQUEST" {
		t.Fatalf("malformed: %v", body)
	}
	admin, _ := tokens.Issue(ctx, app.Claims{Subject: "a", Roles: []string{"admin"}}, 0)
	if res, _ := do(t, "DELETE", srv.URL+"/api/users/1", "", admin); res.StatusCode != 204 {
		t.Fatalf("delete: %d", res.StatusCode)
	}
}

func TestRateLimitHealthAndCorrelationSafety(t *testing.T) {
	srv, _ := newServer(t)
	if res, _ := do(t, "GET", srv.URL+"/api/limited", "", ""); res.StatusCode != 200 {
		t.Fatal("first request allowed")
	}
	res, body := do(t, "GET", srv.URL+"/api/limited", "", "")
	if res.StatusCode != 429 || res.Header.Get("Retry-After") != "60" || body["code"] != "RATE_LIMITED" {
		t.Fatalf("limit: %d %v %s", res.StatusCode, body, res.Header.Get("Retry-After"))
	}
	if res, body := do(t, "GET", srv.URL+"/health/ready", "", ""); res.StatusCode != 200 || body["status"] != "up" {
		t.Fatalf("health: %v", body)
	}
	res, _ = do(t, "GET", srv.URL+"/api/users/1", "", "", "X-Correlation-ID", "bad id with spaces")
	if strings.Contains(res.Header.Get("X-Correlation-ID"), " ") {
		t.Fatal("malformed correlation ids must be replaced")
	}
}

func mustRe(s string) *regexp.Regexp { return regexp.MustCompile(s) }
