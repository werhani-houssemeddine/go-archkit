// Package config loads configuration from environment variables into a struct using
// tags, validates it, and reports every problem at once. The library never reads the
// environment itself: the application's main does it once, then passes typed values on.
//
//	type Config struct {
//	    Env         string        `env:"APP_ENV"      default:"development" oneof:"development,staging,production"`
//	    Port        int           `env:"PORT"         default:"8080"        min:"1" max:"65535"`
//	    DatabaseURL string        `env:"DATABASE_URL" required:"true"       url:"postgres,postgresql"`
//	    JWTSecret   string        `env:"JWT_SECRET"   required:"true"       min:"32" secret:"true"`
//	    Timeout     time.Duration `env:"HTTP_TIMEOUT" default:"15s"`
//	    Origins     []string      `env:"CORS_ORIGINS" sep:","`
//	    DB struct {
//	        MaxConns int `env:"MAX_CONNS" default:"10"`
//	    } `prefix:"DB_"`
//	}
//	var cfg Config
//	if err := config.Load(&cfg); err != nil { log.Fatal(err) }
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/werhani-houssemeddine/go-archkit/errs"
)

// Options configure loading.
type Options struct {
	// Lookup reads a variable (default os.LookupEnv). Use MapLookup in tests.
	Lookup func(key string) (string, bool)
	// Prefix is prepended to every env name ("MYAPP_").
	Prefix string
}

// MapLookup returns a Lookup reading from a map.
func MapLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// Validator is implemented by config structs needing cross-field checks.
type Validator interface{ Validate() error }

// Load fills dst (a pointer to struct) from the environment.
func Load(dst any, opts ...Options) error {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Lookup == nil {
		o.Lookup = os.LookupEnv
	}
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Struct {
		return errors.New("config: Load expects a pointer to a struct")
	}
	var issues []errs.Issue
	loadStruct(rv.Elem(), o.Prefix, o.Lookup, &issues)
	if len(issues) == 0 {
		if v, ok := dst.(Validator); ok {
			if err := v.Validate(); err != nil {
				if e, ok := errs.As(err); ok && len(e.Issues) > 0 {
					issues = append(issues, e.Issues...)
				} else {
					issues = append(issues, errs.Issue{Code: "invalid", Message: err.Error()})
				}
			}
		}
	}
	if len(issues) > 0 {
		var b strings.Builder
		b.WriteString("invalid configuration:")
		for _, is := range issues {
			fmt.Fprintf(&b, "\n  - %s: %s", is.Path, is.Message)
		}
		e := errs.Invalid(issues...)
		e.Code = "INVALID_CONFIG"
		e.Message = b.String()
		return e
	}
	return nil
}

func loadStruct(v reflect.Value, prefix string, lookup func(string) (string, bool), issues *[]errs.Issue) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		if f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeFor[time.Time]() && f.Tag.Get("env") == "" {
			loadStruct(fv, prefix+f.Tag.Get("prefix"), lookup, issues)
			continue
		}
		name := f.Tag.Get("env")
		if name == "" {
			continue
		}
		key := prefix + name
		add := func(msg string) { *issues = append(*issues, errs.Issue{Path: key, Code: "invalid", Message: msg}) }

		raw, ok := lookup(key)
		if !ok || raw == "" {
			if def, has := f.Tag.Lookup("default"); has {
				raw, ok = def, true
			} else if f.Tag.Get("required") == "true" {
				*issues = append(*issues, errs.Issue{Path: key, Code: "required", Message: "is required"})
				continue
			} else {
				continue
			}
		}
		if err := setValue(fv, raw, f.Tag.Get("sep")); err != nil {
			add(err.Error())
			continue
		}
		if msg := checkTags(fv, raw, f.Tag); msg != "" {
			add(msg)
		}
	}
}

func setValue(fv reflect.Value, raw, sep string) error {
	if fv.Type() == reflect.TypeFor[time.Duration]() {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("must be a duration like 30s or 5m")
		}
		fv.SetInt(int64(d))
		return nil
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(strings.ToLower(raw))
		if err != nil {
			switch strings.ToLower(raw) {
			case "yes", "y", "on":
				b = true
			case "no", "n", "off":
				b = false
			default:
				return fmt.Errorf("must be a boolean")
			}
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("must be an integer")
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("must be a non-negative integer")
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(raw, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("must be a number")
		}
		fv.SetFloat(n)
	case reflect.Slice:
		if sep == "" {
			sep = ","
		}
		parts := strings.Split(raw, sep)
		sl := reflect.MakeSlice(fv.Type(), 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			ev := reflect.New(fv.Type().Elem()).Elem()
			if err := setValue(ev, p, ""); err != nil {
				return err
			}
			sl = reflect.Append(sl, ev)
		}
		fv.Set(sl)
	default:
		return fmt.Errorf("unsupported field type %s", fv.Type())
	}
	return nil
}

func checkTags(fv reflect.Value, raw string, tag reflect.StructTag) string {
	if oneof := tag.Get("oneof"); oneof != "" && !slices.Contains(strings.Split(oneof, ","), raw) {
		return "must be one of: " + oneof
	}
	if schemes, ok := tag.Lookup("url"); ok {
		u, err := url.Parse(raw)
		allowed := strings.Split(schemes, ",")
		if err != nil || u.Host == "" || (schemes != "" && !slices.Contains(allowed, u.Scheme)) {
			return "must be a valid URL (" + schemes + ")"
		}
	}
	num := func(name string) (float64, bool) {
		s, ok := tag.Lookup(name)
		if !ok {
			return 0, false
		}
		n, err := strconv.ParseFloat(s, 64)
		return n, err == nil
	}
	var value float64
	isLen := false
	switch fv.Kind() {
	case reflect.String:
		value, isLen = float64(utf8.RuneCountInString(fv.String())), true
	case reflect.Slice:
		value, isLen = float64(fv.Len()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value = float64(fv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value = float64(fv.Uint())
	case reflect.Float32, reflect.Float64:
		value = fv.Float()
	default:
		return ""
	}
	unit := ""
	if isLen {
		unit = " characters"
		if fv.Kind() == reflect.Slice {
			unit = " items"
		}
	}
	if m, ok := num("min"); ok && value < m {
		return fmt.Sprintf("must be at least %v%s", m, unit)
	}
	if m, ok := num("max"); ok && value > m {
		return fmt.Sprintf("must be at most %v%s", m, unit)
	}
	return ""
}

// Describe returns a log-safe map of the config with fields tagged `secret:"true"` masked.
func Describe(cfg any) map[string]any {
	out := map[string]any{}
	describe(reflect.Indirect(reflect.ValueOf(cfg)), "", out)
	return out
}

func describe(v reflect.Value, prefix string, out map[string]any) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		if f.Type.Kind() == reflect.Struct && f.Tag.Get("env") == "" && f.Type != reflect.TypeFor[time.Time]() {
			describe(fv, prefix+f.Tag.Get("prefix"), out)
			continue
		}
		name := f.Tag.Get("env")
		if name == "" {
			name = f.Name
		}
		if f.Tag.Get("secret") == "true" && !fv.IsZero() {
			out[prefix+name] = "***"
		} else {
			out[prefix+name] = fv.Interface()
		}
	}
}
