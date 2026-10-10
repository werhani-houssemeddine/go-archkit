package pg

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"

	"github.com/werhani-houssemeddine/go-archkit/errs"
)

// ConstraintRule gives a friendly, client-safe error for a named constraint.
type ConstraintRule struct {
	Field   string // reported to clients, e.g. "email"
	Message string
	Code    string // e.g. "EMAIL_ALREADY_IN_USE"
}

// ConstraintMap maps constraint names to friendly errors.
type ConstraintMap map[string]ConstraintRule

// driverError extracts SQLSTATE and constraint info from any driver error (pgx's
// *pgconn.PgError, lib/pq's *pq.Error, ...) without importing drivers.
type driverError struct {
	code, constraint, table, column, detail string
}

func asDriverError(err error) (driverError, bool) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := e.(interface{ SQLState() string }); ok {
			d := driverError{code: s.SQLState()}
			rv := reflect.Indirect(reflect.ValueOf(e))
			if rv.Kind() == reflect.Struct {
				d.constraint = field(rv, "ConstraintName", "Constraint")
				d.table = field(rv, "TableName", "Table")
				d.column = field(rv, "ColumnName", "Column")
				d.detail = field(rv, "Detail")
			}
			return d, true
		}
	}
	return driverError{}, false
}

func field(v reflect.Value, names ...string) string {
	for _, n := range names {
		if f := v.FieldByName(n); f.IsValid() && f.Kind() == reflect.String {
			return f.String()
		}
	}
	return ""
}

// Translate converts database errors into *errs.Error so upper layers never see
// SQLSTATE codes. Raw driver details go to Meta (logs), not Details (clients), except
// where a ConstraintMap provides an explicit friendly message.
func Translate(err error, constraints ConstraintMap) error {
	if err == nil {
		return nil
	}
	if _, ok := errs.As(err); ok {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return errs.Wrap(err, errs.KindNotFound, "", "record not found")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errs.Wrap(err, errs.KindTimeout, "QUERY_TIMEOUT", "database operation timed out or was cancelled")
	}
	d, ok := asDriverError(err)
	if !ok {
		msg := strings.ToLower(err.Error())
		if errors.Is(err, sql.ErrConnDone) || strings.Contains(msg, "connection refused") || strings.Contains(msg, "bad connection") ||
			strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset") || errors.Is(err, driver.ErrBadConn) {
			return errs.Wrap(err, errs.KindUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
		}
		return errs.Wrap(err, errs.KindInfrastructure, "DATABASE_ERROR", "database error")
	}
	meta := []any{"sqlstate", d.code, "constraint", d.constraint, "table", d.table, "column", d.column, "detail", d.detail}
	if rule, ok := constraints[d.constraint]; ok && d.constraint != "" {
		code := rule.Code
		if d.code == "23505" {
			if code == "" {
				code = "DUPLICATE"
			}
			e := errs.Wrap(err, errs.KindConflict, code, rule.Message).WithMeta(meta...)
			if rule.Field != "" {
				e = e.WithDetails(map[string]any{"field": rule.Field})
			}
			return e
		}
		if code == "" {
			code = "CONSTRAINT_VIOLATION"
		}
		e := errs.Invalid(errs.Issue{Path: rule.Field, Code: strings.ToLower(code), Message: rule.Message})
		e.Code = code
		return e.WithCause(err).WithMeta(meta...)
	}
	wrap := func(kind errs.Kind, code, msg string) error { return errs.Wrap(err, kind, code, msg).WithMeta(meta...) }
	switch d.code {
	case "23505":
		return wrap(errs.KindConflict, "DUPLICATE", "a record with the same unique value already exists")
	case "23503":
		return wrap(errs.KindConflict, "FOREIGN_KEY_VIOLATION", "operation violates a reference to another record")
	case "23502":
		e := errs.Invalid(errs.Issue{Path: d.column, Code: "required", Message: "is required"})
		e.Code = "NOT_NULL_VIOLATION"
		return e.WithCause(err).WithMeta(meta...)
	case "23514":
		e := errs.Invalid(errs.Issue{Code: "check_violation", Message: "value violates a check constraint"})
		e.Code = "CHECK_VIOLATION"
		return e.WithCause(err).WithMeta(meta...)
	case "23P01":
		return wrap(errs.KindConflict, "EXCLUSION_VIOLATION", "operation conflicts with an existing record")
	case "22P02", "22007", "22008", "22003", "22001":
		e := errs.Invalid(errs.Issue{Code: "invalid_data", Message: "invalid value for the column type"})
		e.Code = "INVALID_DATA"
		return e.WithCause(err).WithMeta(meta...)
	case "40001", "40P01":
		return wrap(errs.KindConflict, "SERIALIZATION_FAILURE", "concurrent transaction conflict, please retry")
	case "57014":
		return wrap(errs.KindTimeout, "QUERY_TIMEOUT", "database statement timed out")
	case "55P03":
		return wrap(errs.KindConflict, "LOCK_NOT_AVAILABLE", "resource is locked")
	case "53300", "57P01", "57P02", "57P03":
		return wrap(errs.KindUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	switch {
	case strings.HasPrefix(d.code, "08"):
		return wrap(errs.KindUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	case strings.HasPrefix(d.code, "42"):
		return wrap(errs.KindInternal, "INVALID_QUERY", "invalid database query") // a bug
	}
	return wrap(errs.KindInfrastructure, "DATABASE_ERROR", "database error")
}

// IsSerializationFailure reports retryable transaction conflicts.
func IsSerializationFailure(err error) bool { return errs.CodeOf(err) == "SERIALIZATION_FAILURE" }
