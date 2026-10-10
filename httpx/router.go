package httpx

import (
	"net/http"
	"strings"

	"github.com/werhani-houssemeddine/go-archkit/errs"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware, outermost first.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Router registers routes on an http.ServeMux (Go 1.22+ "METHOD /path/{param}" patterns)
// with router-wide middleware, groups and problem+json 404/405 responses.
//
//	r := httpx.NewRouter(mapper)
//	r.Use(httpx.RequestContext(httpx.RequestContextOptions{}), httpx.Recover(mapper), httpx.AccessLog(log))
//	api := r.Group("/api/v1", httpx.Authenticate(tokens, httpx.AuthOptions{Optional: true}))
//	api.Handle("GET /users/{id}", getUser, httpx.Require("users:read"))
//	http.ListenAndServe(":8080", r)
type Router struct {
	mux    *http.ServeMux
	mws    []Middleware
	errors *ErrorMapper
}

// NewRouter creates a router; mapper may be nil (defaults).
func NewRouter(mapper *ErrorMapper) *Router {
	if mapper == nil {
		mapper = &ErrorMapper{}
	}
	return &Router{mux: http.NewServeMux(), errors: mapper}
}

// Errors returns the router's error mapper.
func (r *Router) Errors() *ErrorMapper { return r.errors }

// Use adds middleware applied to every request (including 404/405 responses).
func (r *Router) Use(mws ...Middleware) { r.mws = append(r.mws, mws...) }

// Handle registers a handler for a ServeMux pattern with route-specific middleware.
func (r *Router) Handle(pattern string, h http.Handler, mws ...Middleware) {
	r.mux.Handle(pattern, Chain(h, mws...))
}

// HandleFunc registers an error-returning handler function.
func (r *Router) HandleFunc(pattern string, fn func(w http.ResponseWriter, req *http.Request) error, mws ...Middleware) {
	r.Handle(pattern, r.Func(fn), mws...)
}

// Func adapts an error-returning function; errors become problem responses.
func (r *Router) Func(fn func(w http.ResponseWriter, req *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := fn(w, req); err != nil {
			r.errors.Write(w, req, err)
		}
	})
}

// Group returns a sub-router with a path prefix and extra middleware.
func (r *Router) Group(prefix string, mws ...Middleware) *Group {
	return &Group{r: r, prefix: strings.TrimRight(prefix, "/"), mws: mws}
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	Chain(http.HandlerFunc(r.dispatch), r.mws...).ServeHTTP(w, req)
}

func (r *Router) dispatch(w http.ResponseWriter, req *http.Request) {
	h, pattern := r.mux.Handler(req)
	if pattern != "" {
		r.mux.ServeHTTP(w, req)
		return
	}
	// No match: let the mux compute 404 vs 405 (+Allow) and answer with problem+json.
	rec := &statusRecorder{header: http.Header{}}
	h.ServeHTTP(rec, req)
	if rec.status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", rec.header.Get("Allow"))
		r.errors.Write(w, req, errs.New(errs.KindValidation, "METHOD_NOT_ALLOWED", "method "+req.Method+" not allowed"))
		return
	}
	r.errors.Write(w, req, errs.Newf(errs.KindNotFound, "ROUTE_NOT_FOUND", "route %s %s not found", req.Method, req.URL.Path))
}

type statusRecorder struct {
	header http.Header
	status int
}

func (s *statusRecorder) Header() http.Header         { return s.header }
func (s *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (s *statusRecorder) WriteHeader(code int)        { s.status = code }

// Group registers routes under a prefix with shared middleware.
type Group struct {
	r      *Router
	prefix string
	mws    []Middleware
}

// Use adds middleware to the group.
func (g *Group) Use(mws ...Middleware) { g.mws = append(g.mws, mws...) }

// Group creates a nested group.
func (g *Group) Group(prefix string, mws ...Middleware) *Group {
	return &Group{r: g.r, prefix: g.prefix + strings.TrimRight(prefix, "/"), mws: append(append([]Middleware(nil), g.mws...), mws...)}
}

// Handle registers "METHOD /path" relative to the group prefix.
func (g *Group) Handle(pattern string, h http.Handler, mws ...Middleware) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		method, path = "", pattern
	}
	full := g.prefix + path
	if method != "" {
		full = method + " " + full
	}
	g.r.Handle(full, h, append(append([]Middleware(nil), g.mws...), mws...)...)
}

// HandleFunc registers an error-returning handler function.
func (g *Group) HandleFunc(pattern string, fn func(w http.ResponseWriter, req *http.Request) error, mws ...Middleware) {
	g.Handle(pattern, g.r.Func(fn), mws...)
}

// Router returns the owning router.
func (g *Group) Router() *Router { return g.r }
