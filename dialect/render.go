package dialect

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/skudra-go/ant/internal/errs"
	"github.com/skudra-go/ant/qb"
)

// Render converts a statement to SQL using d's placeholders and quoting.
// Dialects call it from their Render method, and describe what differs with
// Syntax.
func Render(d Dialect, stmt qb.Statement) (string, []any, error) {
	r := &renderer{d: d, s: SyntaxOf(d), ctx: context.Background()}

	if c, ok := stmt.(qb.Contextual); ok {
		r.ctx, stmt = c.Context, c.Statement
	}

	switch s := stmt.(type) {
	case *qb.Select:
		r.top = s
		r.selectStmt(s)
	case *qb.Insert:
		r.insert(s)
	case *qb.Update:
		r.update(s)
	case *qb.Delete:
		r.delete(s)
	case *qb.Merge:
		r.merge(s)
	default:
		r.fail("statement", stmt)
	}

	if r.err != nil {
		return "", nil, r.err
	}

	return r.sql.String(), r.args, nil
}

type renderer struct {
	d    Dialect
	s    *Syntax
	ctx  context.Context
	sql  bytes.Buffer
	args []any
	err  error

	top          *qb.Select // the statement itself, when it is a SELECT
	duplicateKey bool       // inside ON DUPLICATE KEY UPDATE

	// writing is the table an UPDATE or DELETE writes, with WrapSelfReads, while
	// its values and conditions render. read records that a subquery among them
	// reads it, maybe that it may, as raw SQL, a name a CTE may have, or a name
	// in a schema only one side gives can, and inner that one is already being
	// rendered.
	writing *qb.TableRef
	read    bool
	maybe   bool
	inner   bool

	// lazies records what Lazy conditions built while a subquery was first
	// written, and replay hands them out again when it is written a second time,
	// so that each is built once.
	lazies    []built
	replay    []built
	recording bool
}

// built is what one Lazy condition gave.
type built struct {
	cond qb.Cond
	err  error
}

// build gives a Lazy condition's result, from a recording if there is one.
func (r *renderer) build(c qb.Lazy) (qb.Cond, error) {
	if len(r.replay) > 0 {
		b := r.replay[0]
		r.replay = r.replay[1:]

		return b.cond, b.err
	}

	cond, err := c.Build(r.ctx)

	if r.recording {
		r.lazies = append(r.lazies, built{cond, err})
	}

	return cond, err
}

func (r *renderer) fail(kind string, node any) {
	if r.err == nil {
		r.err = fmt.Errorf("ant: unknown %s %T", kind, node)
	}
}

// need fails the statement if the database lacks f.
func (r *renderer) need(f Feature) {
	if r.err == nil && r.s.Lacks(f) {
		r.err = fmt.Errorf("%w: %s on %s", errs.ErrUnsupported, f, r.d.Name())
	}
}

func (r *renderer) write(s string) { r.sql.WriteString(s) }

func (r *renderer) quote(ident string) { r.write(r.d.Quote(ident)) }

func (r *renderer) bind(v any) {
	if r.s.UTCTimes {
		v = utc(v)
	}

	r.args = append(r.args, v)
	r.write(r.d.Placeholder(len(r.args)))
}

// derived runs after a derived table is written, with read saying whether a
// sure read of the written table came before it. A sure read found inside it
// counts only as one that may need the wrapper: MySQL 8.0 and 8.4 materialize
// such a derived table before the write, though the manual says the optimizer
// may merge one into the outer query. So the subquery is still wrapped where it
// can be, but not refused where it cannot.
func (r *renderer) derived(read bool) {
	if r.read && !read {
		r.read, r.maybe = false, true
	}
}

func (r *renderer) table(t *qb.TableRef) {
	if w := r.writing; w != nil && strings.EqualFold(t.Name, w.Name) {
		switch {
		case t.Name == w.Name && t.Schema == w.Schema:
			r.read = true
		case strings.EqualFold(t.Schema, w.Schema), t.Schema == "" || w.Schema == "":
			// The same table only if names that differ in case are folded, as
			// MySQL's lower_case_table_names decides, and if the one with no
			// schema is in the other's, neither of which the renderer can tell.
			r.maybe = true
		}
	}

	if t.Schema != "" {
		r.quote(t.Schema)
		r.write(".")
	}

	r.quote(t.Name)
}

// list writes items separated by commas.
func list[T any](r *renderer, items []T, write func(T)) {
	for i, item := range items {
		if i > 0 {
			r.write(", ")
		}

		write(item)
	}
}

func (r *renderer) expr(e qb.Expr) {
	switch e := e.(type) {
	case qb.Column:
		switch {
		case r.duplicateKey && e.Alias == "excluded":
			r.write("VALUES(")
			r.quote(e.Name)
			r.write(")")

			return
		case e.Alias != "":
			r.quote(e.Alias)
			r.write(".")
		case e.Table != nil:
			r.quote(e.Table.Name)
			r.write(".")
		}

		r.quote(e.Name)
	case qb.Param:
		r.bind(e.Value)
	case qb.Star:
		r.write("*")
	case qb.Default:
		r.need(DefaultKeyword)
		r.write("DEFAULT")
	case qb.Func:
		r.function(e)
	case qb.Fragment:
		r.fragment(e)
	case qb.Binary:
		r.write("(")
		r.expr(e.Left)
		r.write(" " + e.Op + " ")
		r.expr(e.Right)
		r.write(")")
	case qb.Subquery:
		r.write("(")
		r.subquery(e.Select, false)
		r.write(")")
	case qb.Case:
		r.write("CASE")

		for _, w := range e.Whens {
			r.write(" WHEN ")
			r.cond(w.If)
			r.write(" THEN ")
			r.expr(w.Then)
		}

		if e.Else != nil {
			r.write(" ELSE ")
			r.expr(e.Else)
		}

		r.write(" END")
	case qb.Collate:
		r.expr(e.Value)
		r.write(" COLLATE ")
		r.quote(e.Collation)
	case qb.Cast:
		r.write("CAST(")
		r.expr(e.Value)
		r.write(" AS " + e.Type + ")")
	case qb.Row:
		r.write("(")
		list(r, e.Values, r.expr)
		r.write(")")
	case qb.Exists:
		if r.s.NoBooleans {
			r.write("CASE WHEN ")
			r.cond(e)
			r.write(" THEN 1 ELSE 0 END")

			return
		}

		r.cond(e)
	default:
		r.fail("expression", e)
	}
}

func (r *renderer) function(f qb.Func) {
	name := f.Name
	if renamed, ok := r.s.Functions[name]; ok {
		name = renamed
	}

	r.write(name + "(")

	if f.Distinct {
		r.write("DISTINCT ")
	}

	if len(f.Filter) > 0 && r.s.NoFilter {
		r.filtered(f)
	} else {
		list(r, f.Args, r.expr)
	}

	r.write(")")

	if len(f.Filter) > 0 && !r.s.NoFilter {
		r.write(" FILTER (WHERE ")
		r.joined(f.Filter, " AND ", "1 = 1")
		r.write(")")
	}

	if f.Over != nil {
		r.write(" OVER (")

		if len(f.Over.PartitionBy) > 0 {
			r.write("PARTITION BY ")
			list(r, f.Over.PartitionBy, r.expr)
		}

		if len(f.Over.OrderBy) > 0 {
			if len(f.Over.PartitionBy) > 0 {
				r.write(" ")
			}

			r.write("ORDER BY ")
			list(r, f.Over.OrderBy, r.order)
		}

		r.write(")")
	}
}

// filtered writes an aggregate's argument for a database with no FILTER. A CASE
// with no ELSE gives NULL for other rows, and aggregates skip NULL, so the two
// mean the same.
func (r *renderer) filtered(f qb.Func) {
	r.write("CASE WHEN ")
	r.joined(f.Filter, " AND ", "1 = 1")
	r.write(" THEN ")

	if len(f.Args) == 1 && f.Args[0] == (qb.Star{}) {
		r.write("1")
	} else {
		list(r, f.Args, r.expr)
	}

	r.write(" END")
}

func (r *renderer) fragment(f qb.Fragment) {
	if f.Needs != "" {
		r.need(Feature(f.Needs))
	}

	// Raw SQL cannot be read, so naming the table counts as maybe reading it.
	if r.writing != nil && namesTable(f.SQL, r.writing.Name) {
		r.maybe = true
	}

	args := f.Args

	for i := 0; i < len(f.SQL); {
		if end := skipQuoted(f.SQL, i); end > i {
			r.write(f.SQL[i:end])
			i = end

			continue
		}

		if f.SQL[i] == '?' && len(args) > 0 {
			r.expr(args[0])
			args = args[1:]
		} else {
			r.sql.WriteByte(f.SQL[i])
		}

		i++
	}

	if len(args) > 0 && r.err == nil {
		r.err = fmt.Errorf("ant: SQL fragment %q has %d unused arguments", f.SQL, len(args))
	}
}

// namesTable reports whether raw SQL names table, in any case, as MySQL reads
// it: as a whole word, or quoted in backticks, whatever schema qualifies it,
// since telling a schema from whatever else comes before a dot would take
// parsing the SQL. A name inside a string literal or a comment does not count. How MySQL splits SQL into literals depends on the
// SQL mode, which the renderer does not know: a backslash escapes a quote unless
// NO_BACKSLASH_ESCAPES is set, and ANSI_QUOTES makes double quotes a name rather
// than a string. So the SQL is read in every mode, and a name found in any of
// them counts. Wrapping a subquery that did not need it costs a derived table,
// and missing one fails the write with error 1093.
func namesTable(q, table string) bool {
	for _, backslash := range []bool{true, false} {
		for _, ansiQuotes := range []bool{true, false} {
			if namesTableIn(q, table, backslash, ansiQuotes) {
				return true
			}
		}
	}

	return false
}

// namesTableIn is namesTable in one SQL mode. Its comments are MySQL's: # and
// -- to the end of the line, where -- needs a space or a control character
// after it, and /* */, except that /*! */ holds SQL that MySQL runs, so that is
// read on, past the version it may start with.
func namesTableIn(q, table string, backslash, ansiQuotes bool) bool {
	word := func(c byte) bool {
		return isNameByte(c, false) || c == '$' || c >= 0x80
	}

	lineEnd := func(i int) int {
		if end := strings.IndexByte(q[i:], '\n'); end >= 0 {
			return i + end
		}

		return len(q)
	}

	for i := 0; i < len(q); {
		c := q[i]
		rest := q[i:]

		switch {
		case c == '`' || (c == '"' && ansiQuotes):
			name, end := mysqlQuoted(q, i, false)
			if strings.EqualFold(name, table) {
				return true
			}

			i = end
		case c == '\'' || c == '"':
			_, i = mysqlQuoted(q, i, backslash)
		case c == '#':
			i = lineEnd(i)
		case strings.HasPrefix(rest, "--") && (len(rest) == 2 || rest[2] <= ' '):
			i = lineEnd(i)
		case strings.HasPrefix(rest, "/*!"):
			i += len("/*!")

			// A version may come first, of five digits, which MySQL 8.4 reads,
			// or six, as a later version may, and the SQL may follow it with no
			// space between. Both are read, as either may be the server's.
			digits := 0
			for i+digits < len(q) && '0' <= q[i+digits] && q[i+digits] <= '9' {
				digits++
			}

			for _, version := range []int{5, 6} {
				j := i + version
				if version > digits || j == len(q) || !word(q[j]) {
					continue
				}

				end := j
				for end < len(q) && word(q[end]) {
					end++
				}

				if strings.EqualFold(q[j:end], table) {
					return true
				}
			}

			if digits >= 5 {
				i += 5
			}
		case strings.HasPrefix(rest, "/*"):
			if end := strings.Index(rest[2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(q)
			}
		case word(c):
			j := i + 1

			for j < len(q) && word(q[j]) {
				j++
			}

			if strings.EqualFold(q[i:j], table) {
				return true
			}

			i = j
		default:
			i++
		}
	}

	return false
}

// mysqlQuoted reads the quoted text that starts at q[i], and returns it and the
// index after its closing quote. A doubled quote stands for one, and with
// backslash, a backslash takes the character after it as it is.
func mysqlQuoted(q string, i int, backslash bool) (string, int) {
	quote := q[i]

	var text strings.Builder

	for j := i + 1; j < len(q); j++ {
		switch {
		case backslash && q[j] == '\\' && j+1 < len(q):
			j++
		case q[j] == quote && j+1 < len(q) && q[j+1] == quote:
			j++
		case q[j] == quote:
			return text.String(), j + 1
		}

		text.WriteByte(q[j])
	}

	return text.String(), len(q)
}

func (r *renderer) selected(s qb.Selected) {
	r.expr(s.Value)

	if s.Alias != "" {
		r.write(" AS ")
		r.quote(s.Alias)
	}
}

func (r *renderer) source(s qb.Source) {
	switch {
	case s.Sub != nil:
		read := r.read

		r.write("(")
		r.selectStmt(s.Sub)
		r.write(")")
		r.derived(read)
	case s.Name != "":
		// A name may be the written table itself, or a CTE, which may read it.
		if r.writing != nil && strings.EqualFold(s.Name, r.writing.Name) {
			r.maybe = true
		}

		r.quote(s.Name)
	case s.Raw != nil:
		r.write("(")
		r.fragment(*s.Raw)
		r.write(")")
	case s.Values != nil && r.s.ValuesUnion:
		r.valuesUnion(s.Values)
	case s.Values != nil:
		r.write("(VALUES ")

		list(r, s.Values.Rows, func(row []qb.Expr) {
			r.write("(")
			list(r, row, r.expr)
			r.write(")")
		})

		r.write(")")
	default:
		r.table(s.Table)
	}

	if s.Alias != "" {
		r.write(" AS ")
		r.quote(s.Alias)
	}

	if s.Values != nil && !r.s.ValuesUnion {
		r.write(" (")
		list(r, s.Values.Columns, r.quote)
		r.write(")")
	}
}

// valuesUnion writes VALUES rows as SELECTs, the first of which names the columns.
func (r *renderer) valuesUnion(v *qb.Values) {
	r.write("(")

	for i, row := range v.Rows {
		if i > 0 {
			r.write(" UNION ALL ")
		}

		r.write("SELECT ")

		for j, value := range row {
			if j > 0 {
				r.write(", ")
			}

			r.expr(value)

			if i == 0 {
				r.write(" AS ")
				r.quote(v.Columns[j])
			}
		}
	}

	r.write(")")
}

var joinWords = map[qb.JoinKind]string{
	qb.InnerJoin: " JOIN ",
	qb.LeftJoin:  " LEFT JOIN ",
	qb.RightJoin: " RIGHT JOIN ",
	qb.FullJoin:  " FULL JOIN ",
	qb.CrossJoin: " CROSS JOIN ",
}

func (r *renderer) join(j qb.Join) {
	if j.Kind == qb.FullJoin {
		r.need(FullJoin)
	}

	r.write(joinWords[j.Kind])

	if j.Lateral {
		r.need(LateralJoin)
		r.write("LATERAL ")
	}

	r.source(j.Source)

	switch {
	case len(j.Using) > 0:
		r.write(" USING (")
		list(r, j.Using, r.quote)
		r.write(")")
	case len(j.On) > 0:
		r.write(" ON ")
		r.joined(j.On, " AND ", "1 = 1")
	case j.Kind != qb.CrossJoin:
		r.write(" ON 1 = 1")
	}
}

func safeComment(s string) string {
	var b strings.Builder

	b.Grow(len(s))

	for i := 0; i < len(s); i++ {
		if i > 0 && (s[i-1] == '/' && s[i] == '*' || s[i-1] == '*' && s[i] == '/') {
			b.WriteByte(' ')
		}

		b.WriteByte(s[i])
	}

	return b.String()
}

func (r *renderer) selectStmt(s *qb.Select) {
	hint := ""

	if s.Hint != "" {
		hint = "/*+ " + safeComment(s.Hint) + " */ "
	}

	if !r.s.HintInSelect {
		r.write(hint)
	}

	if s.Comment != "" {
		r.write("/* " + safeComment(s.Comment) + " */ ")
	}

	if len(s.With) > 0 {
		if s != r.top {
			r.need(SubqueryWith)
		}

		r.write("WITH ")

		if s.Recursive {
			r.write("RECURSIVE ")
		}

		list(r, s.With, func(c qb.CTE) {
			r.quote(c.Name)
			r.write(" AS (")
			r.selectStmt(c.Select)
			r.write(")")
		})

		r.write(" ")
	}

	r.write("SELECT ")

	if r.s.HintInSelect {
		r.write(hint)
	}

	if s.Distinct {
		r.write("DISTINCT ")
	}

	list(r, s.Columns, r.selected)

	if s.From.Table != nil || s.From.Sub != nil || s.From.Name != "" || s.From.Raw != nil ||
		s.From.Values != nil {
		r.write(" FROM ")
		r.source(s.From)
	}

	for _, j := range s.Joins {
		r.join(j)
	}

	r.where(s.Where)

	if len(s.GroupBy) > 0 {
		r.write(" GROUP BY ")
		list(r, s.GroupBy, r.expr)
	}

	if len(s.Having) > 0 {
		r.write(" HAVING ")
		r.joined(s.Having, " AND ", "1 = 1")
	}

	for _, set := range s.Sets {
		r.write(" " + set.Kind + " ")

		if set.All {
			r.write("ALL ")
		}

		if r.s.BareSets {
			r.selectStmt(set.Select)

			continue
		}

		r.write("(")
		r.selectStmt(set.Select)
		r.write(")")
	}

	if len(s.Order) > 0 {
		r.write(" ORDER BY ")
		list(r, s.Order, r.order)
	}

	r.paging(s)

	if s.Lock != nil {
		r.need(RowLocks)

		if s.Lock.Mode == "SHARE" {
			r.need(ShareLocks)
		}

		r.write(" FOR " + s.Lock.Mode)

		switch {
		case s.Lock.NoWait:
			r.write(" NOWAIT")
		case s.Lock.SkipLocked:
			r.write(" SKIP LOCKED")
		}
	}
}

func (r *renderer) paging(s *qb.Select) {
	switch {
	case s.Limit == nil && s.Offset == nil:
	case r.s.OffsetFetch:
		if len(s.Order) == 0 {
			r.write(" ORDER BY (SELECT NULL)")
		}

		r.write(" OFFSET ")

		if s.Offset != nil {
			r.bind(*s.Offset)
		} else {
			r.write("0")
		}

		r.write(" ROWS")

		if s.Limit != nil {
			r.write(" FETCH NEXT ")
			r.bind(*s.Limit)
			r.write(" ROWS ONLY")
		}
	default:
		if s.Limit != nil {
			r.write(" LIMIT ")
			r.bind(*s.Limit)
		} else if r.s.Unlimited != "" {
			r.write(" LIMIT " + r.s.Unlimited)
		}

		if s.Offset != nil {
			r.write(" OFFSET ")
			r.bind(*s.Offset)
		}
	}
}

func (r *renderer) order(o qb.Order) {
	if r.s.NoNullsOrder {
		switch o.Nulls {
		case qb.NullsFirst:
			r.write("CASE WHEN ")
			r.expr(o.Value)
			r.write(" IS NULL THEN 0 ELSE 1 END, ")
		case qb.NullsLast:
			r.write("CASE WHEN ")
			r.expr(o.Value)
			r.write(" IS NULL THEN 1 ELSE 0 END, ")
		}

		o.Nulls = qb.NullsDefault
	}

	r.expr(o.Value)

	if o.Desc {
		r.write(" DESC")
	}

	switch o.Nulls {
	case qb.NullsFirst:
		r.write(" NULLS FIRST")
	case qb.NullsLast:
		r.write(" NULLS LAST")
	}
}

func (r *renderer) assignments(set []qb.Assignment) {
	list(r, set, func(a qb.Assignment) {
		r.quote(a.Column)
		r.write(" = ")
		r.expr(a.Value)
	})
}

func (r *renderer) returning(items []qb.Selected) {
	if len(items) > 0 && !r.s.Output {
		r.write(" RETURNING ")
		list(r, items, r.selected)
	}
}

// output writes returned columns where OUTPUT goes, before a statement's rows or
// conditions. Row is INSERTED or DELETED.
func (r *renderer) output(items []qb.Selected, row string) {
	if len(items) == 0 || !r.s.Output {
		return
	}

	r.write(" OUTPUT ")

	list(r, items, func(s qb.Selected) {
		column, ok := s.Value.(qb.Column)
		if !ok {
			r.fail("returned value", s.Value)

			return
		}

		r.write(row + ".")
		r.quote(column.Name)

		if s.Alias != "" {
			r.write(" AS ")
			r.quote(s.Alias)
		}
	})
}

func (r *renderer) insert(s *qb.Insert) {
	if len(s.Returning) > 0 {
		r.need(ReturningInsert)
	}

	r.write("INSERT INTO ")
	r.table(s.Table)

	if len(s.Columns) > 0 {
		r.write(" (")
		list(r, s.Columns, r.quote)
		r.write(")")
	}

	r.output(s.Returning, "INSERTED")

	switch {
	case s.From != nil:
		r.write(" ")
		r.selectStmt(s.From)
	case len(s.Rows) == 0 && r.s.EmptyValues:
		r.write(" VALUES ()")
	case len(s.Rows) == 0:
		r.write(" DEFAULT VALUES")
	default:
		r.write(" VALUES ")

		list(r, s.Rows, func(row []qb.Expr) {
			r.write("(")
			list(r, row, r.expr)
			r.write(")")
		})
	}

	if s.Conflict != nil {
		r.conflict(s)
	}

	r.returning(s.Returning)
}

func (r *renderer) conflict(s *qb.Insert) {
	c := s.Conflict

	r.need(OnConflict)

	if r.s.DuplicateKey {
		r.duplicate(s)

		return
	}

	r.write(" ON CONFLICT")

	if len(c.Columns) > 0 {
		r.write(" (")
		list(r, c.Columns, r.quote)
		r.write(")")
	}

	if len(c.Set) == 0 {
		r.write(" DO NOTHING")
	} else {
		r.write(" DO UPDATE SET ")
		r.assignments(c.Set)
		r.where(c.Where)
	}
}

// duplicate writes an upsert for a database that has no DO NOTHING and no target:
// any unique key triggers it, and setting a column to itself does nothing.
func (r *renderer) duplicate(s *qb.Insert) {
	c := s.Conflict
	if len(c.Where) > 0 {
		r.need(ConflictWhere)
	}

	r.write(" ON DUPLICATE KEY UPDATE ")

	if len(c.Set) == 0 {
		r.quote(s.Columns[0])
		r.write(" = ")
		r.quote(s.Columns[0])

		return
	}

	r.duplicateKey = true
	r.assignments(c.Set)
	r.duplicateKey = false
}

func (r *renderer) update(s *qb.Update) {
	if len(s.Returning) > 0 {
		r.need(ReturningUpdate)
	}

	r.write("UPDATE ")
	r.table(s.Table)
	r.write(" SET ")

	r.writes(s.Table, func() {
		r.assignments(s.Set)
		r.output(s.Returning, "INSERTED")
		r.where(s.Where)
	})

	r.returning(s.Returning)
}

func (r *renderer) delete(s *qb.Delete) {
	if len(s.Returning) > 0 {
		r.need(ReturningDelete)
	}

	r.write("DELETE FROM ")
	r.table(s.Table)

	r.writes(s.Table, func() {
		r.output(s.Returning, "DELETED")
		r.where(s.Where)
	})

	r.returning(s.Returning)
}

// writes renders the values and conditions of a write on table, watching its
// subqueries for reads of table if the database needs them wrapped.
func (r *renderer) writes(table *qb.TableRef, body func()) {
	if r.s.WrapSelfReads {
		r.writing = table

		defer func() { r.writing = nil }()
	}

	body()
}

// subquery writes the SELECT of IN, EXISTS, or a scalar subquery. MySQL refuses
// an UPDATE or DELETE whose subquery reads the table being written, at any
// depth, but reads a derived table, which it materializes before writing. So
// there such a subquery is wrapped in one, and NO_MERGE keeps the optimizer from
// folding it back in.
func (r *renderer) subquery(s *qb.Select, exists bool) {
	if r.writing == nil || r.inner {
		r.selectStmt(s)

		return
	}

	start, args := r.sql.Len(), len(r.args)
	r.read, r.maybe, r.inner = false, false, true
	r.recording, r.lazies = true, nil

	r.selectStmt(s)

	r.recording = false

	derived, columns := derivable(s, exists)

	switch {
	case !r.read && !r.maybe:
		r.inner, r.lazies = false, nil

		return
	case derived == nil && r.read:
		if r.err == nil {
			r.err = fmt.Errorf(
				"%w: a SELECT * beside other columns, or over a join or a set, that reads the table being written, on %s",
				errs.ErrUnsupported,
				r.d.Name(),
			)
		}

		r.inner, r.lazies = false, nil

		return
	case derived == nil:
		// A read the renderer is not sure of does not refuse a write that may
		// not need the wrapper: the subquery stays as it was written, and if it
		// reads the table, MySQL says so.
		r.inner, r.lazies = false, nil

		return
	}

	// Written again inside the wrapper, which names its columns itself, as IN,
	// EXISTS, and a value read them by position.
	r.sql.Truncate(start)
	r.args = r.args[:args]
	r.replay = r.lazies

	r.write("SELECT /*+ NO_MERGE(materialized) */ * FROM (")
	r.selectStmt(derived)
	r.write(") AS materialized")

	if columns > 0 {
		r.write("(")

		for i := range columns {
			if i > 0 {
				r.write(", ")
			}

			r.quote(fmt.Sprintf("ant_c%d", i+1))
		}

		r.write(")")
	}

	r.inner, r.replay, r.lazies = false, nil, nil
}

// derivable returns s as a derived table takes it, and how many columns the
// derived table must name itself, or 0 if their own names do. A derived table
// needs unique names, which MySQL makes up for a column with none, from its SQL.
// One column is unique however it is named. Of several, two may share a name,
// or have none the renderer can tell, as an expression or raw SQL has not, and
// then the derived table's own column list, which MySQL takes from 8.0.19,
// names them.
// The subquery keeps its select list either way, so what refers to a column by
// name, as ORDER BY and HAVING can, still finds the same one. A star cannot be
// counted. In EXISTS its columns matter only to what reads them, so
// SELECT * there becomes SELECT 1 unless grouping, ordering, an offset, or a set
// reads the columns. A star over one table is left as it is, with the table's
// names, which are unique. One beside other columns, or over a join or a set,
// may name a column twice, which a derived table refuses, so that is nil.
func derivable(s *qb.Select, exists bool) (*qb.Select, int) {
	star := func(c qb.Selected) bool {
		_, ok := c.Value.(qb.Star)

		return ok
	}

	if !slices.ContainsFunc(s.Columns, star) {
		if len(s.Columns) < 2 {
			return s, 0
		}

		seen := map[string]bool{}

		for _, c := range s.Columns {
			name := strings.ToLower(c.Alias)

			if column, ok := c.Value.(qb.Column); ok && name == "" {
				name = strings.ToLower(column.Name)
			}

			if name == "" || seen[name] {
				return s, len(s.Columns)
			}

			seen[name] = true
		}

		return s, 0
	}

	simple := exists && len(s.GroupBy) == 0 && len(s.Having) == 0 && len(s.Order) == 0 &&
		s.Offset == nil && len(s.Sets) == 0 && !slices.ContainsFunc(s.Columns, func(c qb.Selected) bool {
		return !star(c)
	})

	switch {
	case simple:
		out := *s
		out.Columns = []qb.Selected{{Value: qb.Fragment{SQL: "1"}}}

		return &out, 0
	case len(s.Columns) > 1 || len(s.Joins) > 0 || len(s.Sets) > 0:
		return nil, 0
	}

	return s, 0
}

func (r *renderer) merge(s *qb.Merge) {
	r.need(Merge)
	r.write("MERGE INTO ")
	r.table(s.Table)
	r.write(" USING ")
	r.source(s.Using)
	r.write(" ON ")
	r.joined(s.On, " AND ", "1 = 1")

	for _, a := range s.Actions {
		if a.Matched {
			r.write(" WHEN MATCHED")
		} else {
			r.write(" WHEN NOT MATCHED")
		}

		if len(a.If) > 0 {
			r.write(" AND ")
			r.joined(a.If, " AND ", "1 = 1")
		}

		r.write(" THEN ")

		switch {
		case a.Delete:
			r.write("DELETE")
		case len(a.Set) > 0:
			r.write("UPDATE SET ")
			r.assignments(a.Set)
		case len(a.Columns) > 0:
			r.write("INSERT (")
			list(r, a.Columns, r.quote)
			r.write(") VALUES (")
			list(r, a.Values, r.expr)
			r.write(")")
		default:
			r.write("DO NOTHING")
		}
	}

	if r.s.MergeSemicolon {
		r.write(";")
	}
}

func (r *renderer) where(conds []qb.Cond) {
	if len(conds) > 0 {
		r.write(" WHERE ")
		r.joined(conds, " AND ", "1 = 1")
	}
}

// joined writes conds separated by sep, or empty when there are none.
func (r *renderer) joined(conds []qb.Cond, sep, empty string) {
	if len(conds) == 0 {
		r.write(empty)

		return
	}

	for i, c := range conds {
		if i > 0 {
			r.write(sep)
		}

		r.cond(c)
	}
}

var operators = map[qb.Op]string{
	qb.OpEq:    " = ",
	qb.OpNe:    " <> ",
	qb.OpGt:    " > ",
	qb.OpGte:   " >= ",
	qb.OpLt:    " < ",
	qb.OpLte:   " <= ",
	qb.OpLike:  " LIKE ",
	qb.OpILike: " ILIKE ",
}

func (r *renderer) cond(c qb.Cond) {
	switch c := c.(type) {
	case qb.Compare:
		if c.Op == qb.OpILike && r.s.NoILike {
			r.write("LOWER(")
			r.expr(c.Left)
			r.write(") LIKE LOWER(")
			r.expr(c.Right)
			r.write(")")

			return
		}

		r.expr(c.Left)
		r.write(operators[c.Op])
		r.expr(c.Right)
	case qb.In:
		r.in(c)
	case qb.Between:
		r.expr(c.Left)
		r.write(" BETWEEN ")
		r.expr(c.Low)
		r.write(" AND ")
		r.expr(c.High)
	case qb.IsNull:
		r.expr(c.Left)

		if c.Negate {
			r.write(" IS NOT NULL")
		} else {
			r.write(" IS NULL")
		}
	case qb.Exists:
		if c.Negate {
			r.write("NOT ")
		}

		r.write("EXISTS (")
		r.subquery(c.Select, true)
		r.write(")")
	case qb.Fragment:
		r.write("(")
		r.fragment(c)
		r.write(")")
	case qb.Lazy:
		built, err := r.build(c)

		switch {
		case err != nil:
			if r.err == nil {
				r.err = err
			}
		case built == nil:
			r.write("1 = 1")
		default:
			r.cond(built)
		}
	case qb.And:
		r.group(c.Children, " AND ", "1 = 1")
	case qb.Or:
		r.group(c.Children, " OR ", "1 = 0")
	case qb.Not:
		r.write("NOT (")
		r.cond(c.Child)
		r.write(")")
	default:
		r.fail("condition", c)
	}
}

func (r *renderer) group(conds []qb.Cond, sep, empty string) {
	r.write("(")
	r.joined(conds, sep, empty)
	r.write(")")
}

// in renders membership. SQL has no empty list, so an empty list renders as a
// constant: nothing is a member of it.
func (r *renderer) in(c qb.In) {
	if c.Sub == nil && len(c.Values) == 0 {
		if c.Negate {
			r.write("1 = 1")
		} else {
			r.write("1 = 0")
		}

		return
	}

	if row, ok := c.Left.(qb.Row); ok && c.Sub == nil && r.s.NoRowIn {
		r.rowIn(row, c)

		return
	}

	r.expr(c.Left)

	if c.Negate {
		r.write(" NOT")
	}

	r.write(" IN (")

	switch {
	case c.Sub == nil:
		list(r, c.Values, r.expr)
	case c.Sub.Limit != nil && r.s.WrapLimitedIn:
		inner, read := r.inner, r.read
		r.inner = true

		r.write("SELECT * FROM (")
		r.selectStmt(c.Sub)
		r.write(") AS limited")

		r.inner = inner
		r.derived(read)
	default:
		r.subquery(c.Sub, false)
	}

	r.write(")")
}

// rowIn spells out (a, b) IN ((1, 2), (3, 4)) as a list of ORs.
func (r *renderer) rowIn(left qb.Row, c qb.In) {
	if c.Negate {
		r.write("NOT ")
	}

	r.write("(")

	for i, value := range c.Values {
		row, ok := value.(qb.Row)
		if !ok || len(row.Values) != len(left.Values) {
			r.fail("row", value)

			return
		}

		if i > 0 {
			r.write(" OR ")
		}

		r.write("(")

		for j, column := range left.Values {
			if j > 0 {
				r.write(" AND ")
			}

			r.expr(column)
			r.write(" = ")
			r.expr(row.Values[j])
		}

		r.write(")")
	}

	r.write(")")
}

// Numbered returns a numbered placeholder, such as "$1" or "@p1".
func Numbered(prefix string, n int) string {
	return prefix + strconv.Itoa(n)
}
