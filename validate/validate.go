// Package validate provides an accumulating, fluent validator: every rule that fails
// adds an errs.Issue with a path, a stable code, a message and params, and Err returns
// one *errs.Error (KindValidation) listing ALL problems, not just the first.
//
//	func (r CreateUserRequest) Validate() error {
//	    v := validate.New()
//	    v.String("email", r.Email).Required().Email()
//	    v.String("name", r.Name).Required().Len(2, 50)
//	    validate.Number(v, "age", r.Age).Min(18)
//	    validate.Each(v, "tags", r.Tags, func(v *validate.V, t string) { v.String("", t).Max(20) })
//	    v.Nested("address", func(v *validate.V) { v.String("city", r.Address.City).Required() })
//	    return v.Err()
//	}
package validate

import (
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/werhani-houssemeddine/go-archkit/clock"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/ids"
)

// Validatable is implemented by request DTOs / commands. The HTTP layer and the
// mediator call it automatically.
type Validatable interface {
	Validate() error
}

// MessageFunc lets you translate messages globally (code + params are always kept on
// the issue, so per-request translation in the HTTP layer is also possible).
var MessageFunc = func(code string, params map[string]any, fallback string) string { return fallback }

// V collects issues. The zero value is not usable; call New.
type V struct {
	issues *[]errs.Issue
	prefix string
	clock  clock.Clock
}

// New returns an empty validator.
func New() *V { return &V{issues: &[]errs.Issue{}, clock: clock.Default} }

// WithClock sets the clock used by time rules (Past, Future, MinAge...).
func (v *V) WithClock(c clock.Clock) *V { v.clock = c; return v }

func (v *V) path(p string) string {
	switch {
	case v.prefix == "":
		return p
	case p == "":
		return v.prefix
	case strings.HasPrefix(p, "["):
		return v.prefix + p
	default:
		return v.prefix + "." + p
	}
}

// Add records an issue at path (relative to the current prefix).
func (v *V) Add(path, code, message string, params map[string]any) {
	*v.issues = append(*v.issues, errs.Issue{Path: v.path(path), Code: code, Message: MessageFunc(code, params, message), Params: params})
}

// Check adds an issue when ok is false. Returns ok.
func (v *V) Check(ok bool, path, code, message string) bool {
	if !ok {
		v.Add(path, code, message, nil)
	}
	return ok
}

// Nested validates a sub-object under path ("address" → "address.city").
func (v *V) Nested(path string, fn func(v *V)) {
	fn(&V{issues: v.issues, prefix: v.path(path), clock: v.clock})
}

// Merge adds the issues of err (if it is a validation error) under path; other errors
// are recorded as a single issue.
func (v *V) Merge(path string, err error) {
	if err == nil {
		return
	}
	if e, ok := errs.As(err); ok && e.Kind == errs.KindValidation && len(e.Issues) > 0 {
		for _, is := range e.Issues {
			p := path
			if is.Path != "" {
				if p == "" {
					p = is.Path
				} else {
					p = p + "." + is.Path
				}
			}
			*v.issues = append(*v.issues, errs.Issue{Path: v.path(p), Code: is.Code, Message: is.Message, Params: is.Params})
		}
		return
	}
	v.Add(path, "invalid", err.Error(), nil)
}

// Valid reports whether no issue was recorded.
func (v *V) Valid() bool { return len(*v.issues) == 0 }

// Issues returns a copy of the recorded issues.
func (v *V) Issues() []errs.Issue { return slices.Clone(*v.issues) }

// Err returns nil when valid, otherwise a KindValidation *errs.Error with every issue.
func (v *V) Err() error {
	if v.Valid() {
		return nil
	}
	return errs.Invalid(v.Issues()...)
}

// Each validates every element of a slice with paths like "items[3]".
func Each[T any](v *V, path string, items []T, fn func(v *V, item T)) {
	for i, item := range items {
		fn(&V{issues: v.issues, prefix: v.path(path + "[" + strconv.Itoa(i) + "]"), clock: v.clock}, item)
	}
}

/* ================================================================== */
/* Strings                                                             */
/* ================================================================== */

// StringRule validates a string. Except for Required, rules are skipped for empty
// strings, so optional fields only need their format rules.
type StringRule struct {
	v      *V
	path   string
	value  string
	failed bool
}

// String starts rules for a string value.
func (v *V) String(path, value string) *StringRule {
	return &StringRule{v: v, path: path, value: value}
}

// OptionalString validates *s only when it is non-nil.
func (v *V) OptionalString(path string, s *string) *StringRule {
	if s == nil {
		return &StringRule{v: v, path: path, failed: true}
	}
	return v.String(path, *s)
}

func (r *StringRule) skip() bool { return r.failed || r.value == "" }

func (r *StringRule) fail(code, msg string, params map[string]any) *StringRule {
	r.v.Add(r.path, code, msg, params)
	r.failed = true
	return r
}

// Required rejects empty and whitespace-only strings.
func (r *StringRule) Required() *StringRule {
	if !r.failed && strings.TrimSpace(r.value) == "" {
		return r.fail("required", "is required", nil)
	}
	return r
}

// Min requires at least n characters (runes).
func (r *StringRule) Min(n int) *StringRule {
	if !r.skip() && utf8.RuneCountInString(r.value) < n {
		return r.fail("too_short", fmt.Sprintf("must contain at least %d characters", n), map[string]any{"min": n})
	}
	return r
}

// Max allows at most n characters (runes).
func (r *StringRule) Max(n int) *StringRule {
	if !r.failed && utf8.RuneCountInString(r.value) > n {
		return r.fail("too_long", fmt.Sprintf("must contain at most %d characters", n), map[string]any{"max": n})
	}
	return r
}

// Len requires between min and max characters.
func (r *StringRule) Len(min, max int) *StringRule { return r.Min(min).Max(max) }

// Matches requires the regular expression to match.
func (r *StringRule) Matches(re *regexp.Regexp, message string) *StringRule {
	if !r.skip() && !re.MatchString(r.value) {
		if message == "" {
			message = "has an invalid format"
		}
		return r.fail("invalid_format", message, map[string]any{"pattern": re.String()})
	}
	return r
}

// Email requires a bare address ("a@b.co", no display name).
func (r *StringRule) Email() *StringRule {
	if r.skip() {
		return r
	}
	addr, err := mail.ParseAddress(r.value)
	if err != nil || addr.Address != r.value || !strings.Contains(r.value[strings.LastIndexByte(r.value, '@'):], ".") || len(r.value) > 254 {
		return r.fail("invalid_email", "must be a valid email address", nil)
	}
	return r
}

// URL requires an absolute URL with one of the schemes (default http, https).
func (r *StringRule) URL(schemes ...string) *StringRule {
	if r.skip() {
		return r
	}
	if len(schemes) == 0 {
		schemes = []string{"http", "https"}
	}
	u, err := url.Parse(r.value)
	if err != nil || u.Host == "" || !slices.Contains(schemes, u.Scheme) {
		return r.fail("invalid_url", "must be a valid URL", map[string]any{"schemes": schemes})
	}
	return r
}

// UUID requires a canonical UUID (versions 1-8 or nil).
func (r *StringRule) UUID() *StringRule {
	if !r.skip() && !ids.IsValid(r.value) {
		return r.fail("invalid_uuid", "must be a valid UUID", nil)
	}
	return r
}

// OneOf requires the value to be one of allowed.
func (r *StringRule) OneOf(allowed ...string) *StringRule {
	if !r.skip() && !slices.Contains(allowed, r.value) {
		return r.fail("invalid_enum", "must be one of: "+strings.Join(allowed, ", "), map[string]any{"allowed": allowed})
	}
	return r
}

// Alphanumeric allows only letters and digits (any script).
func (r *StringRule) Alphanumeric() *StringRule {
	if r.skip() {
		return r
	}
	for _, c := range r.value {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return r.fail("invalid_alphanumeric", "must contain only letters and digits", nil)
		}
	}
	return r
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Slug allows lowercase letters, digits and single dashes.
func (r *StringRule) Slug() *StringRule {
	if !r.skip() && !slugRe.MatchString(r.value) {
		return r.fail("invalid_slug", "must contain lowercase letters, digits and dashes", nil)
	}
	return r
}

// Date requires a calendar date in the given Go layout (e.g. time.DateOnly, "02/01/2006").
func (r *StringRule) Date(layout string) *StringRule {
	if r.skip() {
		return r
	}
	if _, err := time.Parse(layout, r.value); err != nil {
		return r.fail("invalid_date", "must be a valid date ("+layout+")", map[string]any{"layout": layout})
	}
	return r
}

// PasswordPolicy describes password strength requirements.
type PasswordPolicy struct {
	MinLength                   int
	Upper, Lower, Digit, Symbol bool
}

// DefaultPasswordPolicy requires 10+ characters with upper, lower and digit.
var DefaultPasswordPolicy = PasswordPolicy{MinLength: 10, Upper: true, Lower: true, Digit: true}

// Password checks strength and reports every missing requirement in one issue.
func (r *StringRule) Password(p PasswordPolicy) *StringRule {
	if r.failed {
		return r
	}
	var up, lo, di, sy bool
	for _, c := range r.value {
		switch {
		case unicode.IsUpper(c):
			up = true
		case unicode.IsLower(c):
			lo = true
		case unicode.IsDigit(c):
			di = true
		case !unicode.IsSpace(c):
			sy = true
		}
	}
	var missing []string
	if utf8.RuneCountInString(r.value) < p.MinLength {
		missing = append(missing, fmt.Sprintf("at least %d characters", p.MinLength))
	}
	if p.Upper && !up {
		missing = append(missing, "an uppercase letter")
	}
	if p.Lower && !lo {
		missing = append(missing, "a lowercase letter")
	}
	if p.Digit && !di {
		missing = append(missing, "a digit")
	}
	if p.Symbol && !sy {
		missing = append(missing, "a symbol")
	}
	if len(missing) > 0 {
		return r.fail("weak_password", "must contain "+strings.Join(missing, ", "), map[string]any{"missing": missing})
	}
	return r
}

// Custom adds an issue when pred returns false.
func (r *StringRule) Custom(pred func(string) bool, code, message string) *StringRule {
	if !r.skip() && !pred(r.value) {
		return r.fail(code, message, nil)
	}
	return r
}

/* ================================================================== */
/* Numbers                                                             */
/* ================================================================== */

// Numeric is any integer or float type.
type Numeric interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~float32 | ~float64
}

// NumberRule validates a number.
type NumberRule[T Numeric] struct {
	v     *V
	path  string
	value T
	fail  bool
}

// Number starts rules for a numeric value.
func Number[T Numeric](v *V, path string, value T) *NumberRule[T] {
	return &NumberRule[T]{v: v, path: path, value: value}
}

func (r *NumberRule[T]) add(code, msg string, params map[string]any) *NumberRule[T] {
	r.v.Add(r.path, code, msg, params)
	r.fail = true
	return r
}

// Min requires value >= n.
func (r *NumberRule[T]) Min(n T) *NumberRule[T] {
	if !r.fail && r.value < n {
		return r.add("too_small", fmt.Sprintf("must be at least %v", n), map[string]any{"min": n})
	}
	return r
}

// Max requires value <= n.
func (r *NumberRule[T]) Max(n T) *NumberRule[T] {
	if !r.fail && r.value > n {
		return r.add("too_big", fmt.Sprintf("must be at most %v", n), map[string]any{"max": n})
	}
	return r
}

// Between requires min <= value <= max.
func (r *NumberRule[T]) Between(min, max T) *NumberRule[T] {
	if !r.fail && (r.value < min || r.value > max) {
		return r.add("out_of_range", fmt.Sprintf("must be between %v and %v", min, max), map[string]any{"min": min, "max": max})
	}
	return r
}

// Positive requires value > 0.
func (r *NumberRule[T]) Positive() *NumberRule[T] {
	if !r.fail && r.value <= 0 {
		return r.add("not_positive", "must be positive", nil)
	}
	return r
}

// NonNegative requires value >= 0.
func (r *NumberRule[T]) NonNegative() *NumberRule[T] {
	if !r.fail && r.value < 0 {
		return r.add("negative", "must not be negative", nil)
	}
	return r
}

// OneOf requires the value to be one of allowed.
func (r *NumberRule[T]) OneOf(allowed ...T) *NumberRule[T] {
	if !r.fail && !slices.Contains(allowed, r.value) {
		return r.add("invalid_enum", fmt.Sprintf("must be one of %v", allowed), map[string]any{"allowed": allowed})
	}
	return r
}

/* ================================================================== */
/* Time                                                                */
/* ================================================================== */

// TimeRule validates a time.Time.
type TimeRule struct {
	v     *V
	path  string
	value time.Time
	fail  bool
}

// Time starts rules for a time value. Zero times are skipped unless Required is used.
func (v *V) Time(path string, t time.Time) *TimeRule { return &TimeRule{v: v, path: path, value: t} }

func (r *TimeRule) skip() bool { return r.fail || r.value.IsZero() }

func (r *TimeRule) add(code, msg string, params map[string]any) *TimeRule {
	r.v.Add(r.path, code, msg, params)
	r.fail = true
	return r
}

// Required rejects the zero time.
func (r *TimeRule) Required() *TimeRule {
	if !r.fail && r.value.IsZero() {
		return r.add("required", "is required", nil)
	}
	return r
}

// Past requires a time strictly before now.
func (r *TimeRule) Past() *TimeRule {
	if !r.skip() && !r.value.Before(r.v.clock.Now()) {
		return r.add("not_past", "must be in the past", nil)
	}
	return r
}

// Future requires a time strictly after now.
func (r *TimeRule) Future() *TimeRule {
	if !r.skip() && !r.value.After(r.v.clock.Now()) {
		return r.add("not_future", "must be in the future", nil)
	}
	return r
}

// Before requires value < t.
func (r *TimeRule) Before(t time.Time) *TimeRule {
	if !r.skip() && !r.value.Before(t) {
		return r.add("too_late", "must be before "+t.Format(time.RFC3339), map[string]any{"before": t})
	}
	return r
}

// After requires value > t.
func (r *TimeRule) After(t time.Time) *TimeRule {
	if !r.skip() && !r.value.After(t) {
		return r.add("too_early", "must be after "+t.Format(time.RFC3339), map[string]any{"after": t})
	}
	return r
}

// MinAge treats the value as a birth date and requires an age of at least years.
func (r *TimeRule) MinAge(years int) *TimeRule {
	if !r.skip() && clock.Age(r.value, r.v.clock.Now()) < years {
		return r.add("too_young", fmt.Sprintf("must be at least %d years old", years), map[string]any{"minAge": years})
	}
	return r
}

// MaxAge treats the value as a birth date and requires an age of at most years.
func (r *TimeRule) MaxAge(years int) *TimeRule {
	if !r.skip() && clock.Age(r.value, r.v.clock.Now()) > years {
		return r.add("too_old", fmt.Sprintf("must be at most %d years old", years), map[string]any{"maxAge": years})
	}
	return r
}

/* ================================================================== */
/* Slices                                                              */
/* ================================================================== */

// Slice checks the length of a slice.
func Slice[T any](v *V, path string, items []T) *SliceRule[T] {
	return &SliceRule[T]{v: v, path: path, items: items}
}

// SliceRule validates a slice's size and uniqueness.
type SliceRule[T any] struct {
	v     *V
	path  string
	items []T
	fail  bool
}

func (r *SliceRule[T]) add(code, msg string, params map[string]any) *SliceRule[T] {
	r.v.Add(r.path, code, msg, params)
	r.fail = true
	return r
}

// NotEmpty requires at least one item.
func (r *SliceRule[T]) NotEmpty() *SliceRule[T] {
	if !r.fail && len(r.items) == 0 {
		return r.add("empty", "must not be empty", nil)
	}
	return r
}

// Min requires at least n items.
func (r *SliceRule[T]) Min(n int) *SliceRule[T] {
	if !r.fail && len(r.items) < n {
		return r.add("too_few", fmt.Sprintf("must contain at least %d items", n), map[string]any{"min": n})
	}
	return r
}

// Max allows at most n items.
func (r *SliceRule[T]) Max(n int) *SliceRule[T] {
	if !r.fail && len(r.items) > n {
		return r.add("too_many", fmt.Sprintf("must contain at most %d items", n), map[string]any{"max": n})
	}
	return r
}

// Unique requires distinct keys; duplicates are reported at their index.
func Unique[T any, K comparable](r *SliceRule[T], key func(T) K) *SliceRule[T] {
	seen := map[K]int{}
	for i, it := range r.items {
		k := key(it)
		if first, ok := seen[k]; ok {
			r.v.Add(r.path+"["+strconv.Itoa(i)+"]", "not_unique", "must be unique", map[string]any{"duplicateOf": first})
			r.fail = true
		} else {
			seen[k] = i
		}
	}
	return r
}
