package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/validate"
)

// Routes is implemented by *Router and *Group.
type Routes interface {
	Handle(pattern string, h http.Handler, mws ...Middleware)
	mapper() *ErrorMapper
}

func (r *Router) mapper() *ErrorMapper { return r.errors }
func (g *Group) mapper() *ErrorMapper  { return g.r.errors }

// BindOptions tune request binding.
type BindOptions struct {
	MaxBodyBytes int64 // default 1 MiB
	// AllowUnknownFields accepts JSON fields not present in the input struct.
	AllowUnknownFields bool
}

// Empty is an input or output type with no content (an Empty output yields 204).
type Empty struct{}

// Responder lets an output control status, headers and body.
type Responder interface {
	Respond(w http.ResponseWriter, r *http.Request) error
}

// Result is a Responder carrying status, headers, cookies and a JSON body.
type Result[T any] struct {
	Status  int
	Body    T
	Headers http.Header
	Cookies []*http.Cookie
	NoBody  bool
}

// Respond writes the result.
func (res Result[T]) Respond(w http.ResponseWriter, _ *http.Request) error {
	for k, vs := range res.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	for _, c := range res.Cookies {
		http.SetCookie(w, c)
	}
	status := res.Status
	if status == 0 {
		status = http.StatusOK
	}
	if res.NoBody || status == http.StatusNoContent {
		w.WriteHeader(status)
		return nil
	}
	return WriteJSON(w, status, res.Body)
}

// WithCookie returns a copy with a cookie (secure defaults: HttpOnly, Secure, SameSite=Lax, Path=/).
func (res Result[T]) WithCookie(c *http.Cookie) Result[T] {
	cp := *c
	if cp.Path == "" {
		cp.Path = "/"
	}
	if cp.SameSite == 0 {
		cp.SameSite = http.SameSiteLaxMode
	}
	cp.HttpOnly, cp.Secure = true, true
	res.Cookies = append(append([]*http.Cookie(nil), res.Cookies...), &cp)
	return res
}

// WithHeader returns a copy with a header set.
func (res Result[T]) WithHeader(k, v string) Result[T] {
	h := res.Headers.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set(k, v)
	res.Headers = h
	return res
}

// OK returns 200 with body.
func OK[T any](body T) Result[T] { return Result[T]{Status: http.StatusOK, Body: body} }

// Created returns 201 with body and an optional Location header.
func Created[T any](body T, location string) Result[T] {
	r := Result[T]{Status: http.StatusCreated, Body: body}
	if location != "" {
		r = r.WithHeader("Location", location)
	}
	return r
}

// Accepted returns 202 with body.
func Accepted[T any](body T) Result[T] { return Result[T]{Status: http.StatusAccepted, Body: body} }

// NoContent returns 204.
func NoContent() Result[Empty] { return Result[Empty]{Status: http.StatusNoContent, NoBody: true} }

// WriteJSON writes v as JSON with status.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

type reqKey struct{}

// RequestFrom returns the *http.Request inside typed handlers (headers, IP, cookies).
func RequestFrom(ctx context.Context) *http.Request {
	r, _ := ctx.Value(reqKey{}).(*http.Request)
	return r
}

// Handle registers a typed endpoint:
//
//	type GetUserIn struct {
//	    ID     string `path:"id"`
//	    Expand bool   `query:"expand"`
//	}
//	func (in GetUserIn) Validate() error { v := validate.New(); v.String("id", in.ID).UUID(); return v.Err() }
//
//	httpx.Handle(api, "GET /users/{id}", func(ctx context.Context, in GetUserIn) (UserDTO, error) { ... })
//
// Input binding: JSON body (POST/PUT/PATCH/DELETE; unknown fields rejected; size
// limited), then `path:"…"`, `query:"…"` and `header:"…"` tagged fields (which override
// body values). Inputs implementing validate.Validatable are validated; binding and
// validation issues are reported together. Outputs implementing Responder control the
// response; Empty yields 204; anything else is 200 JSON.
func Handle[In, Out any](r Routes, pattern string, fn func(ctx context.Context, in In) (Out, error), mws ...Middleware) {
	r.Handle(pattern, Endpoint(r.mapper(), fn, BindOptions{}), mws...)
}

// Endpoint builds the http.Handler used by Handle (for custom registration).
func Endpoint[In, Out any](m *ErrorMapper, fn func(ctx context.Context, in In) (Out, error), o BindOptions) http.Handler {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 1 << 20
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in In
		if err := Bind(req, &in, o); err != nil {
			m.Write(w, req, err)
			return
		}
		ctx := context.WithValue(req.Context(), reqKey{}, req)
		out, err := fn(ctx, in)
		if err != nil {
			m.Write(w, req, err)
			return
		}
		switch v := any(out).(type) {
		case Responder:
			if err := v.Respond(w, req); err != nil {
				m.Write(w, req, err)
			}
		case Empty:
			w.WriteHeader(http.StatusNoContent)
		default:
			_ = WriteJSON(w, http.StatusOK, out)
		}
	})
}

// Bind fills dst (pointer to struct) from the request and validates it.
func Bind(req *http.Request, dst any, o BindOptions) error {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Struct {
		return errs.Internalf("httpx: Bind expects a pointer to struct, got %T", dst)
	}
	v := validate.New()
	if hasBodyFields(rv.Elem().Type()) && req.Body != nil && req.Body != http.NoBody &&
		(req.Method == http.MethodPost || req.Method == http.MethodPut || req.Method == http.MethodPatch || req.Method == http.MethodDelete) {
		if err := decodeBody(req, dst, o); err != nil {
			return err
		}
	}
	bindTagged(req, rv.Elem(), v)
	if !v.Valid() {
		if val, ok := dst.(validate.Validatable); ok {
			v.Merge("", val.Validate())
		}
		return v.Err()
	}
	if val, ok := dst.(validate.Validatable); ok {
		return val.Validate()
	}
	return nil
}

func hasBodyFields(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() || f.Tag.Get("json") == "-" {
			continue
		}
		if f.Tag.Get("path") == "" && f.Tag.Get("query") == "" && f.Tag.Get("header") == "" {
			return true
		}
	}
	return false
}

func decodeBody(req *http.Request, dst any, o BindOptions) error {
	if ct := req.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
			return errs.New(errs.KindValidation, "UNSUPPORTED_MEDIA_TYPE", "content type must be application/json")
		}
	}
	body := http.MaxBytesReader(nil, req.Body, o.MaxBodyBytes)
	dec := json.NewDecoder(body)
	if !o.AllowUnknownFields {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.Is(err, io.EOF):
			return nil // empty body: let validation report missing fields
		case errors.As(err, &maxErr):
			return errs.Newf(errs.KindValidation, "PAYLOAD_TOO_LARGE", "request body must not exceed %d bytes", o.MaxBodyBytes)
		case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
			return errs.New(errs.KindValidation, "MALFORMED_REQUEST", "malformed JSON body")
		case errors.As(err, &typeErr):
			return errs.InvalidField(typeErr.Field, "invalid_type", "must be "+typeErr.Type.String())
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
			return errs.InvalidField(field, "unrecognized_key", "unknown field")
		default:
			return errs.New(errs.KindValidation, "MALFORMED_REQUEST", "invalid request body")
		}
	}
	if dec.More() {
		return errs.New(errs.KindValidation, "MALFORMED_REQUEST", "body must contain a single JSON value")
	}
	return nil
}

func bindTagged(req *http.Request, v reflect.Value, val *validate.V) {
	t := v.Type()
	q := req.URL.Query()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		var raw []string
		var loc, name string
		switch {
		case f.Tag.Get("path") != "":
			loc, name = "path", f.Tag.Get("path")
			if s := req.PathValue(name); s != "" {
				raw = []string{s}
			}
		case f.Tag.Get("query") != "":
			loc, name = "query", f.Tag.Get("query")
			raw = q[name]
		case f.Tag.Get("header") != "":
			loc, name = "header", f.Tag.Get("header")
			raw = req.Header.Values(name)
		default:
			continue
		}
		if len(raw) == 0 {
			continue
		}
		if err := setField(v.Field(i), raw); err != nil {
			val.Add(loc+"."+name, "invalid_type", err.Error(), nil)
		}
	}
}

func setField(fv reflect.Value, raw []string) error {
	if fv.Kind() == reflect.Pointer {
		p := reflect.New(fv.Type().Elem())
		if err := setField(p.Elem(), raw); err != nil {
			return err
		}
		fv.Set(p)
		return nil
	}
	if fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() != reflect.Uint8 {
		var parts []string
		for _, r := range raw {
			parts = append(parts, strings.Split(r, ",")...)
		}
		sl := reflect.MakeSlice(fv.Type(), 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			ev := reflect.New(fv.Type().Elem()).Elem()
			if err := setField(ev, []string{p}); err != nil {
				return err
			}
			sl = reflect.Append(sl, ev)
		}
		fv.Set(sl)
		return nil
	}
	s := raw[0]
	switch fv.Interface().(type) {
	case time.Time:
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			if t, err = time.Parse(time.DateOnly, s); err != nil {
				return fmt.Errorf("must be a date (YYYY-MM-DD) or RFC 3339 timestamp")
			}
		}
		fv.Set(reflect.ValueOf(t))
		return nil
	case time.Duration:
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("must be a duration")
		}
		fv.SetInt(int64(d))
		return nil
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("must be true or false")
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("must be an integer")
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("must be a non-negative integer")
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("must be a number")
		}
		fv.SetFloat(n)
	default:
		if u, ok := fv.Addr().Interface().(interface{ UnmarshalText([]byte) error }); ok {
			if err := u.UnmarshalText([]byte(s)); err != nil {
				return fmt.Errorf("invalid value")
			}
			return nil
		}
		return fmt.Errorf("unsupported type %s", fv.Type())
	}
	return nil
}
