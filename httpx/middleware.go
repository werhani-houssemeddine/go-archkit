package httpx

import (
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/authz"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/ids"
	"github.com/werhani-houssemeddine/go-archkit/lifecycle"
	"github.com/werhani-houssemeddine/go-archkit/logx"
	"github.com/werhani-houssemeddine/go-archkit/resilience"
)

/* ------------------------------------------------------------------ */
/* Request context                                                     */
/* ------------------------------------------------------------------ */

// RequestContextOptions configure RequestContext.
type RequestContextOptions struct {
	CorrelationHeader string // default "X-Correlation-ID"
	RequestIDHeader   string // default "X-Request-ID"
	// DistrustIncoming ignores client-provided ids (set at the public edge).
	DistrustIncoming bool
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// RequestContext assigns request and correlation ids (validated; generated when missing
// or malformed), stores them with execctx and echoes them in response headers.
func RequestContext(o RequestContextOptions) Middleware {
	if o.CorrelationHeader == "" {
		o.CorrelationHeader = "X-Correlation-ID"
	}
	if o.RequestIDHeader == "" {
		o.RequestIDHeader = "X-Request-ID"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID, corrID := "", ""
			if !o.DistrustIncoming {
				if v := r.Header.Get(o.RequestIDHeader); idRe.MatchString(v) {
					reqID = v
				}
				if v := r.Header.Get(o.CorrelationHeader); idRe.MatchString(v) {
					corrID = v
				}
			}
			if reqID == "" {
				reqID = ids.NewV7().String()
			}
			if corrID == "" {
				corrID = reqID
			}
			ctx := execctx.WithCorrelationID(execctx.WithRequestID(r.Context(), reqID), corrID)
			if loc := r.Header.Get("Accept-Language"); loc != "" {
				ctx = execctx.WithLocale(ctx, strings.TrimSpace(strings.Split(loc, ",")[0]))
			}
			w.Header().Set(o.RequestIDHeader, reqID)
			w.Header().Set(o.CorrelationHeader, corrID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

/* ------------------------------------------------------------------ */
/* Recovery & logging                                                  */
/* ------------------------------------------------------------------ */

// Recover converts panics into 500 problem responses and logs the stack.
func Recover(m *ErrorMapper) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					err := errs.Internalf("panic: %v", rec).WithMeta("stack", string(debug.Stack()))
					m.Write(w, r, err)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog writes one structured line per request (status, duration, route pattern).
func AccessLog(log *slog.Logger) Middleware {
	log = logx.OrDiscard(log)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			level := slog.LevelInfo
			if sw.status >= 500 {
				level = slog.LevelError
			} else if sw.status >= 400 {
				level = slog.LevelWarn
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("route", r.Pattern),
				slog.Int("status", sw.status), slog.Int("bytes", sw.bytes), slog.Duration("duration", time.Since(start)),
				slog.String("ip", ClientIP(r)),
			}
			log.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

// ClientIP returns the remote IP (without port). Put a trusted proxy in front that
// rewrites RemoteAddr (or use a dedicated middleware) before trusting X-Forwarded-For.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

/* ------------------------------------------------------------------ */
/* Authentication & authorization                                      */
/* ------------------------------------------------------------------ */

// AuthOptions configure Authenticate.
type AuthOptions struct {
	// Optional lets anonymous requests through (an invalid token still fails).
	Optional bool
	// Cookie also reads the token from this cookie (browser apps).
	Cookie string
	// RBAC expands roles into permissions on the actor.
	RBAC *authz.RBAC
	// ToActor maps claims to an actor (default: subject, roles, permissions, tenant).
	ToActor func(ctx context.Context, c app.Claims) (*execctx.Actor, error)
}

// Authenticate verifies the bearer token (or cookie) and stores the actor in the
// request context (execctx.ActorFrom).
func Authenticate(v app.TokenVerifier, m *ErrorMapper, o AuthOptions) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := ""
			if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
				token = strings.TrimSpace(h[7:])
			} else if o.Cookie != "" {
				if c, err := r.Cookie(o.Cookie); err == nil {
					token = c.Value
				}
			}
			if token == "" {
				if o.Optional {
					next.ServeHTTP(w, r)
					return
				}
				m.Write(w, r, errs.ErrMissingToken)
				return
			}
			claims, err := v.Verify(r.Context(), token)
			if err != nil {
				m.Write(w, r, err)
				return
			}
			var actor *execctx.Actor
			if o.ToActor != nil {
				if actor, err = o.ToActor(r.Context(), claims); err != nil {
					m.Write(w, r, err)
					return
				}
			} else {
				actor = &execctx.Actor{ID: claims.Subject, Type: "user", Roles: claims.Roles, Permissions: claims.Permissions, TenantID: claims.TenantID}
			}
			if o.RBAC != nil {
				actor = o.RBAC.Expand(actor)
			}
			next.ServeHTTP(w, r.WithContext(execctx.WithActor(r.Context(), actor)))
		})
	}
}

// Require rejects requests whose actor lacks any of the permissions (401 anonymous, 403 missing).
func Require(m *ErrorMapper, permissions ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := authz.Require(r.Context(), permissions...); err != nil {
				m.Write(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole rejects requests whose actor has none of the roles.
func RequireRole(m *ErrorMapper, roles ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := authz.RequireRole(r.Context(), roles...); err != nil {
				m.Write(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

/* ------------------------------------------------------------------ */
/* Protection                                                          */
/* ------------------------------------------------------------------ */

// RateLimit rejects requests over the limit with 429 + Retry-After and sets RateLimit
// headers. key defaults to the actor id, else the client IP.
func RateLimit(l resilience.RateLimiter, m *ErrorMapper, key func(*http.Request) string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := ""
			if key != nil {
				k = key(r)
			} else if a := execctx.ActorFrom(r.Context()); a != nil {
				k = "actor:" + a.ID
			} else {
				k = "ip:" + ClientIP(r)
			}
			d, err := resilience.Enforce(r.Context(), l, k, 1)
			w.Header().Set("RateLimit-Limit", strconv.Itoa(d.Limit))
			w.Header().Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
			if err != nil {
				if !d.Allowed {
					w.Header().Set("RateLimit-Reset", strconv.Itoa(int(math.Ceil(d.RetryAfter.Seconds()))))
				}
				m.Write(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout sets a deadline on the request context; handlers honoring ctx stop early and
// the error mapper answers 504.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// MaxBody limits request body size for every handler (typed handlers also limit).
func MaxBody(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets conservative headers for JSON APIs.
func SecurityHeaders(hsts bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			if hsts {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORSOptions configure CORS.
type CORSOptions struct {
	Origins          []string // exact origins; "*" allows any (not with credentials)
	Methods          []string // default GET, POST, PUT, PATCH, DELETE
	Headers          []string // default Authorization, Content-Type, X-Request-ID, X-Correlation-ID
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAge           time.Duration
}

// CORS answers preflight requests and sets CORS headers for allowed origins.
func CORS(o CORSOptions) Middleware {
	if len(o.Methods) == 0 {
		o.Methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	}
	if len(o.Headers) == 0 {
		o.Headers = []string{"Authorization", "Content-Type", "X-Request-ID", "X-Correlation-ID"}
	}
	anyOrigin := slices.Contains(o.Origins, "*") && !o.AllowCredentials
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			h := w.Header()
			h.Add("Vary", "Origin")
			if origin != "" && (anyOrigin || slices.Contains(o.Origins, origin)) {
				if anyOrigin {
					h.Set("Access-Control-Allow-Origin", "*")
				} else {
					h.Set("Access-Control-Allow-Origin", origin)
				}
				if o.AllowCredentials {
					h.Set("Access-Control-Allow-Credentials", "true")
				}
				if len(o.ExposeHeaders) > 0 {
					h.Set("Access-Control-Expose-Headers", strings.Join(o.ExposeHeaders, ", "))
				}
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					h.Set("Access-Control-Allow-Methods", strings.Join(o.Methods, ", "))
					h.Set("Access-Control-Allow-Headers", strings.Join(o.Headers, ", "))
					if o.MaxAge > 0 {
						h.Set("Access-Control-Max-Age", strconv.Itoa(int(o.MaxAge.Seconds())))
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

/* ------------------------------------------------------------------ */
/* Health                                                              */
/* ------------------------------------------------------------------ */

// RegisterHealth adds GET <prefix>/live (process up) and GET <prefix>/ready (dependencies
// up; 503 while shutting down or when a critical check fails).
func RegisterHealth(r *Router, prefix string, h *lifecycle.Health, shuttingDown func() bool) {
	if prefix == "" {
		prefix = "/health"
	}
	r.Handle("GET "+prefix+"/live", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = WriteJSON(w, http.StatusOK, map[string]string{"status": "up"})
	}))
	r.Handle("GET "+prefix+"/ready", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if shuttingDown != nil && shuttingDown() {
			_ = WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "down", "reason": "shutting_down"})
			return
		}
		rep := h.Run(req.Context())
		status := http.StatusOK
		if rep.Status == lifecycle.Down {
			status = http.StatusServiceUnavailable
		}
		_ = WriteJSON(w, status, rep)
	}))
}
