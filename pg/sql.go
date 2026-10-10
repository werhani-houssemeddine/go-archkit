package pg

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Query is anything that renders to SQL text with $n placeholders and its arguments.
type Query interface {
	SQL() (string, []any)
}

// Expr is a SQL fragment using "?" placeholders, composed safely and rendered to
// PostgreSQL "$n" placeholders. Write "??" for a literal question mark (e.g. jsonb ? op).
//
//	pg.Q("SELECT * FROM users WHERE email = ? AND status = ?", email, "active")
type Expr struct {
	parts []part
}

type part struct {
	text  string
	arg   any
	isArg bool
}

// Q builds an expression. Args may themselves be Expr (inlined) or Ident (quoted).
// It panics when the number of placeholders and arguments differ (a programming error).
func Q(text string, args ...any) Expr {
	var parts []part
	var b strings.Builder
	ai := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '?' {
			b.WriteByte(c)
			continue
		}
		if i+1 < len(text) && text[i+1] == '?' {
			b.WriteByte('?')
			i++
			continue
		}
		if ai >= len(args) {
			panic("pg: not enough arguments for placeholders in " + strconv.Quote(text))
		}
		if b.Len() > 0 {
			parts = append(parts, part{text: b.String()})
			b.Reset()
		}
		parts = append(parts, part{arg: args[ai], isArg: true})
		ai++
	}
	if ai != len(args) {
		panic("pg: too many arguments for placeholders in " + strconv.Quote(text))
	}
	if b.Len() > 0 {
		parts = append(parts, part{text: b.String()})
	}
	return Expr{parts: parts}
}

// Raw is UNSAFE verbatim SQL; only for trusted constant text.
func Raw(text string) Expr { return Expr{parts: []part{{text: text}}} }

// Ident is a quoted identifier usable as an Expr argument: Q("SELECT * FROM ?", Ident("users")).
type Ident string

// Join concatenates expressions with sep (sep is literal SQL text).
func Join(sep string, exprs ...Expr) Expr {
	var parts []part
	for i, e := range exprs {
		if i > 0 && sep != "" {
			parts = append(parts, part{text: sep})
		}
		parts = append(parts, e.parts...)
	}
	return Expr{parts: parts}
}

// List renders "($1, $2, ...)", or "(NULL)" when empty so IN (...) stays valid.
func List[T any](values []T) Expr {
	if len(values) == 0 {
		return Raw("(NULL)")
	}
	parts := []part{{text: "("}}
	for i, v := range values {
		if i > 0 {
			parts = append(parts, part{text: ", "})
		}
		parts = append(parts, part{arg: v, isArg: true})
	}
	return Expr{parts: append(parts, part{text: ")"})}
}

// IsEmpty reports an empty fragment.
func (e Expr) IsEmpty() bool {
	for _, p := range e.parts {
		if p.isArg || strings.TrimSpace(p.text) != "" {
			return false
		}
	}
	return true
}

// SQL renders the expression.
func (e Expr) SQL() (string, []any) {
	var b strings.Builder
	var args []any
	e.render(&b, &args)
	return b.String(), args
}

func (e Expr) String() string {
	q, a := e.SQL()
	if len(a) == 0 {
		return q
	}
	return fmt.Sprintf("%s -- %v", q, a)
}

func (e Expr) render(b *strings.Builder, args *[]any) {
	for _, p := range e.parts {
		if !p.isArg {
			b.WriteString(p.text)
			continue
		}
		switch v := p.arg.(type) {
		case Expr:
			v.render(b, args)
		case Ident:
			b.WriteString(QuoteIdent(string(v)))
		default:
			*args = append(*args, v)
			b.WriteString("$" + strconv.Itoa(len(*args)))
		}
	}
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// QuoteIdent quotes "schema.table" / "t.col" / "*" identifiers and panics on anything
// that is not a plain identifier, so user input can never smuggle SQL through column
// or sort names.
func QuoteIdent(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		switch {
		case p == "*":
		case identRe.MatchString(p):
			parts[i] = `"` + p + `"`
		default:
			panic(fmt.Sprintf("pg: unsafe SQL identifier %q", name))
		}
	}
	return strings.Join(parts, ".")
}

// SafeIdent reports whether name can be quoted by QuoteIdent (use it to validate user input).
func SafeIdent(name string) bool {
	for _, p := range strings.Split(name, ".") {
		if p != "*" && !identRe.MatchString(p) {
			return false
		}
	}
	return true
}

// EscapeLike escapes %, _ and \ for LIKE / ILIKE patterns.
func EscapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

/* ================================================================== */
/* WHERE                                                               */
/* ================================================================== */

type cond struct {
	or   bool
	expr Expr
}

// Where accumulates conditions combined LEFT TO RIGHT:
// Where(a).OrWhere(b).Where(c) renders ((a OR b) AND c).
type Where struct{ conds []cond }

var operators = map[string]bool{"=": true, "!=": true, "<>": true, ">": true, ">=": true, "<": true, "<=": true,
	"LIKE": true, "ILIKE": true, "NOT LIKE": true, "NOT ILIKE": true, "@>": true, "<@": true, "&&": true}

func (w *Where) add(or bool, e Expr) { w.conds = append(w.conds, cond{or: or, expr: e}) }

func binary(col, op string, v any) Expr {
	op = strings.ToUpper(strings.TrimSpace(op))
	if !operators[op] {
		panic(fmt.Sprintf("pg: unsupported operator %q", op))
	}
	if v == nil && (op == "=" || op == "!=" || op == "<>") {
		if op == "=" {
			return Q("? IS NULL", Ident(col))
		}
		return Q("? IS NOT NULL", Ident(col))
	}
	return Q("? "+op+" ?", Ident(col), v)
}

// Build renders the conditions (empty Expr when none).
func (w *Where) Build() Expr {
	var acc Expr
	for i, c := range w.conds {
		if i == 0 {
			acc = c.expr
			continue
		}
		conn := " AND "
		if c.or {
			conn = " OR "
		}
		if i >= 2 && w.conds[i-1].or != c.or {
			acc = Q("(?)", acc)
		}
		acc = Q("?"+conn+"?", acc, c.expr)
	}
	return acc
}

// HasConditions reports whether any condition was added.
func (w *Where) HasConditions() bool { return len(w.conds) > 0 }

// Conditions is the fluent condition API shared by SELECT/UPDATE/DELETE builders.
type Conditions[B any] struct {
	w    Where
	self *B
}

// Eq adds col = v (IS NULL when v is nil).
func (c *Conditions[B]) Eq(col string, v any) *B { c.w.add(false, binary(col, "=", v)); return c.self }

// Where adds col <op> v with a whitelisted operator.
func (c *Conditions[B]) Where(col, op string, v any) *B {
	c.w.add(false, binary(col, op, v))
	return c.self
}

// OrWhere adds OR col <op> v.
func (c *Conditions[B]) OrWhere(col, op string, v any) *B {
	c.w.add(true, binary(col, op, v))
	return c.self
}

// WhereExpr adds a raw expression (built with Q).
func (c *Conditions[B]) WhereExpr(e Expr) *B { c.w.add(false, e); return c.self }

// OrWhereExpr adds OR <expr>.
func (c *Conditions[B]) OrWhereExpr(e Expr) *B { c.w.add(true, e); return c.self }

// Group adds a parenthesized group of conditions.
func (c *Conditions[B]) Group(fn func(w *Where)) *B {
	var g Where
	fn(&g)
	if g.HasConditions() {
		c.w.add(false, Q("(?)", g.Build()))
	}
	return c.self
}

// OrGroup adds OR (group).
func (c *Conditions[B]) OrGroup(fn func(w *Where)) *B {
	var g Where
	fn(&g)
	if g.HasConditions() {
		c.w.add(true, Q("(?)", g.Build()))
	}
	return c.self
}

// In adds col IN (...); an empty list renders FALSE.
func In[B any, T any](c *Conditions[B], col string, values []T) *B {
	if len(values) == 0 {
		c.w.add(false, Raw("FALSE"))
	} else {
		c.w.add(false, Q("? IN ?", Ident(col), List(values)))
	}
	return c.self
}

// IsNull adds col IS NULL.
func (c *Conditions[B]) IsNull(col string) *B {
	c.w.add(false, Q("? IS NULL", Ident(col)))
	return c.self
}

// NotNull adds col IS NOT NULL.
func (c *Conditions[B]) NotNull(col string) *B {
	c.w.add(false, Q("? IS NOT NULL", Ident(col)))
	return c.self
}

// Contains adds a case-insensitive substring match with LIKE wildcards escaped.
func (c *Conditions[B]) Contains(col, text string) *B {
	c.w.add(false, Q("? ILIKE ?", Ident(col), "%"+EscapeLike(text)+"%"))
	return c.self
}

// Between adds col BETWEEN a AND b.
func (c *Conditions[B]) Between(col string, a, b any) *B {
	c.w.add(false, Q("? BETWEEN ? AND ?", Ident(col), a, b))
	return c.self
}

// Add methods on Where for use inside Group callbacks.

// Eq adds col = v.
func (w *Where) Eq(col string, v any) *Where { w.add(false, binary(col, "=", v)); return w }

// Or adds OR col <op> v.
func (w *Where) Or(col, op string, v any) *Where { w.add(true, binary(col, op, v)); return w }

// And adds AND col <op> v.
func (w *Where) And(col, op string, v any) *Where { w.add(false, binary(col, op, v)); return w }

/* ================================================================== */
/* SELECT                                                              */
/* ================================================================== */

// SelectBuilder builds SELECT statements; identifiers are quoted and values bound.
type SelectBuilder struct {
	Conditions[SelectBuilder]
	table, alias   string
	cols           []Expr
	joins          []Expr
	groups, orders []Expr
	having         []Expr
	limit, offset  int
	distinct       bool
	lock           string
}

// Select starts a SELECT on table ("schema.table" allowed).
func Select(table string, columns ...string) *SelectBuilder {
	b := &SelectBuilder{table: table, limit: -1, offset: -1}
	b.self = b
	b.Columns(columns...)
	return b
}

// As sets a table alias.
func (b *SelectBuilder) As(alias string) *SelectBuilder { b.alias = alias; return b }

// Columns adds quoted columns.
func (b *SelectBuilder) Columns(cols ...string) *SelectBuilder {
	for _, c := range cols {
		b.cols = append(b.cols, Q("?", Ident(c)))
	}
	return b
}

// ColumnExpr adds a computed column: ColumnExpr(pg.Q("count(*)"), "total").
func (b *SelectBuilder) ColumnExpr(e Expr, alias string) *SelectBuilder {
	if alias != "" {
		e = Q("? AS ?", e, Ident(alias))
	}
	b.cols = append(b.cols, e)
	return b
}

// Distinct adds DISTINCT.
func (b *SelectBuilder) Distinct() *SelectBuilder { b.distinct = true; return b }

// Join adds "<kind> JOIN table [AS alias] ON on" (kind: INNER, LEFT, RIGHT, FULL).
func (b *SelectBuilder) Join(kind, table, alias string, on Expr) *SelectBuilder {
	kind = strings.ToUpper(kind)
	if kind != "INNER" && kind != "LEFT" && kind != "RIGHT" && kind != "FULL" {
		panic("pg: invalid join kind " + kind)
	}
	ref := Q("?", Ident(table))
	if alias != "" {
		ref = Q("? AS ?", Ident(table), Ident(alias))
	}
	b.joins = append(b.joins, Q(" "+kind+" JOIN ? ON ?", ref, on))
	return b
}

// GroupBy adds GROUP BY columns.
func (b *SelectBuilder) GroupBy(cols ...string) *SelectBuilder {
	for _, c := range cols {
		b.groups = append(b.groups, Q("?", Ident(c)))
	}
	return b
}

// Having adds a HAVING expression.
func (b *SelectBuilder) Having(e Expr) *SelectBuilder { b.having = append(b.having, e); return b }

// OrderBy adds a sort key (column is quoted, so user-provided sort fields are safe;
// still whitelist them with domain.ParseSort).
func (b *SelectBuilder) OrderBy(col string, desc bool) *SelectBuilder {
	dir := " ASC"
	if desc {
		dir = " DESC"
	}
	b.orders = append(b.orders, Q("?"+dir, Ident(col)))
	return b
}

// Limit sets LIMIT.
func (b *SelectBuilder) Limit(n int) *SelectBuilder { b.limit = n; return b }

// Offset sets OFFSET.
func (b *SelectBuilder) Offset(n int) *SelectBuilder { b.offset = n; return b }

// Page sets LIMIT/OFFSET for a 1-based page.
func (b *SelectBuilder) Page(page, size int) *SelectBuilder {
	if page < 1 {
		page = 1
	}
	return b.Limit(size).Offset((page - 1) * size)
}

// ForUpdate adds FOR UPDATE [SKIP LOCKED].
func (b *SelectBuilder) ForUpdate(skipLocked bool) *SelectBuilder {
	b.lock = " FOR UPDATE"
	if skipLocked {
		b.lock += " SKIP LOCKED"
	}
	return b
}

func (b *SelectBuilder) from() Expr {
	if b.alias != "" {
		return Q("? AS ?", Ident(b.table), Ident(b.alias))
	}
	return Q("?", Ident(b.table))
}

func (b *SelectBuilder) tail(withGroups bool) Expr {
	e := Join("", b.joins...)
	if where := b.w.Build(); !where.IsEmpty() {
		e = Q("? WHERE ?", e, where)
	}
	if withGroups && len(b.groups) > 0 {
		e = Q("? GROUP BY ?", e, Join(", ", b.groups...))
	}
	return e
}

// SQL renders the statement.
func (b *SelectBuilder) SQL() (string, []any) { return b.Expr().SQL() }

// Expr returns the statement as a composable fragment (for subqueries: EXISTS (...), IN (...)).
func (b *SelectBuilder) Expr() Expr {
	cols := Raw("*")
	if len(b.cols) > 0 {
		cols = Join(", ", b.cols...)
	}
	d := ""
	if b.distinct {
		d = "DISTINCT "
	}
	e := Q("SELECT "+d+"? FROM ?", cols, concat(b.from(), b.tail(true)))
	if len(b.having) > 0 {
		e = Q("? HAVING ?", e, Join(" AND ", b.having...))
	}
	if len(b.orders) > 0 {
		e = Q("? ORDER BY ?", e, Join(", ", b.orders...))
	}
	if b.limit >= 0 {
		e = Q("? LIMIT ?", e, b.limit)
	}
	if b.offset >= 0 {
		e = Q("? OFFSET ?", e, b.offset)
	}
	if b.lock != "" {
		e = Q("?"+b.lock, e)
	}
	return e
}

// CountSQL renders SELECT COUNT(*) with the same FROM/JOIN/WHERE (for pagination totals).
func (b *SelectBuilder) CountSQL() (string, []any) {
	if len(b.groups) > 0 {
		inner := *b
		inner.orders, inner.limit, inner.offset, inner.lock = nil, -1, -1, ""
		q, a := inner.SQL()
		return "SELECT COUNT(*) FROM (" + q + ") AS sub", a
	}
	return Q("SELECT COUNT(*) FROM ?", concat(b.from(), b.tail(false))).SQL()
}

/* ================================================================== */
/* INSERT / UPDATE / DELETE                                            */
/* ================================================================== */

// InsertBuilder builds INSERT statements.
type InsertBuilder struct {
	table     string
	cols      []string
	rows      [][]any
	conflict  Expr
	returning []string
}

// Insert starts an INSERT with explicit column order.
func Insert(table string, columns ...string) *InsertBuilder {
	return &InsertBuilder{table: table, cols: columns}
}

// Values appends a row (same order as columns).
func (b *InsertBuilder) Values(vals ...any) *InsertBuilder {
	if len(vals) != len(b.cols) {
		panic(fmt.Sprintf("pg: insert into %s expects %d values, got %d", b.table, len(b.cols), len(vals)))
	}
	b.rows = append(b.rows, vals)
	return b
}

// Row appends a row from a column→value map (keys must match the columns).
func (b *InsertBuilder) Row(m map[string]any) *InsertBuilder {
	vals := make([]any, len(b.cols))
	for i, c := range b.cols {
		vals[i] = m[c]
	}
	return b.Values(vals...)
}

// OnConflictDoNothing adds ON CONFLICT [(target)] DO NOTHING.
func (b *InsertBuilder) OnConflictDoNothing(target ...string) *InsertBuilder {
	b.conflict = Q(" ON CONFLICT? DO NOTHING", targetExpr(target))
	return b
}

// OnConflictUpdate adds ON CONFLICT (target) DO UPDATE SET col = EXCLUDED.col for
// `update` columns (default: all inserted columns except target).
func (b *InsertBuilder) OnConflictUpdate(target []string, update ...string) *InsertBuilder {
	if len(update) == 0 {
		for _, c := range b.cols {
			if !contains(target, c) {
				update = append(update, c)
			}
		}
	}
	sets := make([]Expr, len(update))
	for i, c := range update {
		sets[i] = Q("? = EXCLUDED.?", Ident(c), Ident(c))
	}
	b.conflict = Q(" ON CONFLICT? DO UPDATE SET ?", targetExpr(target), Join(", ", sets...))
	return b
}

// Returning adds RETURNING columns.
func (b *InsertBuilder) Returning(cols ...string) *InsertBuilder { b.returning = cols; return b }

// SQL renders the statement.
func (b *InsertBuilder) SQL() (string, []any) {
	if len(b.rows) == 0 {
		panic("pg: insert without values")
	}
	tuples := make([]Expr, len(b.rows))
	for i, r := range b.rows {
		tuples[i] = List(r)
	}
	return Q("INSERT INTO ? ? VALUES ?", Ident(b.table), identList(b.cols), concat(Join(", ", tuples...), b.conflict, returningExpr(b.returning))).SQL()
}

// UpdateBuilder builds UPDATE statements and refuses to run without WHERE unless All().
type UpdateBuilder struct {
	Conditions[UpdateBuilder]
	table     string
	sets      []Expr
	returning []string
	all       bool
}

// Update starts an UPDATE.
func Update(table string) *UpdateBuilder {
	b := &UpdateBuilder{table: table}
	b.self = b
	return b
}

// Set assigns a value (or an Expr such as pg.Q("version + 1")).
func (b *UpdateBuilder) Set(col string, v any) *UpdateBuilder {
	b.sets = append(b.sets, Q("? = ?", Ident(col), v))
	return b
}

// SetMap assigns several columns (sorted by name for stable SQL).
func (b *UpdateBuilder) SetMap(m map[string]any) *UpdateBuilder {
	for _, k := range sortedKeys(m) {
		b.Set(k, m[k])
	}
	return b
}

// All explicitly allows updating every row.
func (b *UpdateBuilder) All() *UpdateBuilder { b.all = true; return b }

// Returning adds RETURNING columns.
func (b *UpdateBuilder) Returning(cols ...string) *UpdateBuilder { b.returning = cols; return b }

// SQL renders the statement (panics on missing SET or WHERE: those are programming errors).
func (b *UpdateBuilder) SQL() (string, []any) {
	if len(b.sets) == 0 {
		panic("pg: update without Set")
	}
	if !b.w.HasConditions() && !b.all {
		panic("pg: update without Where; call All() to update every row")
	}
	e := Q("UPDATE ? SET ?", Ident(b.table), Join(", ", b.sets...))
	if w := b.w.Build(); !w.IsEmpty() {
		e = Q("? WHERE ?", e, w)
	}
	return concat(e, returningExpr(b.returning)).SQL()
}

// DeleteBuilder builds DELETE statements and refuses to run without WHERE unless All().
type DeleteBuilder struct {
	Conditions[DeleteBuilder]
	table     string
	returning []string
	all       bool
}

// Delete starts a DELETE.
func Delete(table string) *DeleteBuilder {
	b := &DeleteBuilder{table: table}
	b.self = b
	return b
}

// All explicitly allows deleting every row.
func (b *DeleteBuilder) All() *DeleteBuilder { b.all = true; return b }

// Returning adds RETURNING columns.
func (b *DeleteBuilder) Returning(cols ...string) *DeleteBuilder { b.returning = cols; return b }

// SQL renders the statement.
func (b *DeleteBuilder) SQL() (string, []any) {
	if !b.w.HasConditions() && !b.all {
		panic("pg: delete without Where; call All() to delete every row")
	}
	e := Q("DELETE FROM ?", Ident(b.table))
	if w := b.w.Build(); !w.IsEmpty() {
		e = Q("? WHERE ?", e, w)
	}
	return concat(e, returningExpr(b.returning)).SQL()
}

func identList(cols []string) Expr {
	parts := make([]Expr, len(cols))
	for i, c := range cols {
		parts[i] = Q("?", Ident(c))
	}
	return Q("(?)", Join(", ", parts...))
}

func targetExpr(target []string) Expr {
	if len(target) == 0 {
		return Expr{}
	}
	return Q(" ?", identList(target))
}

func returningExpr(cols []string) Expr {
	if len(cols) == 0 {
		return Expr{}
	}
	parts := make([]Expr, len(cols))
	for i, c := range cols {
		parts[i] = Q("?", Ident(c))
	}
	return Q(" RETURNING ?", Join(", ", parts...))
}

func concat(exprs ...Expr) Expr { return Join("", exprs...) }

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
