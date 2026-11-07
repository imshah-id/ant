package dialect

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/skudra-go/ant/qb"
)

// Bind prepares raw SQL for d. Three forms of argument are accepted:
//
//   - The dialect's own placeholders, such as $1, pass through unchanged.
//   - sql.Named arguments are written @name.
//   - Otherwise ? works as a placeholder, but only when the query uses no
//     numbered placeholders and has exactly one ? per argument. PostgreSQL also
//     has a ? operator on jsonb, and those queries fail one of these tests.
//
// With @name or ?, a slice directly inside IN (...) expands to one placeholder
// per element, and an empty slice becomes NULL. Anywhere else a slice is one
// value, so = ANY(@ids) passes an array. Text in strings, quoted identifiers,
// comments, and dollar quotes is left alone, as are operators like @>.
//
// On a database whose columns keep no zone, times among the bound values are put
// in UTC, after lists are expanded, so each element is.
func Bind(d Dialect, query string, args []any) (string, []any, error) {
	text, bound, err := bind(d, query, args)
	if err != nil || !SyntaxOf(d).UTCTimes {
		return text, bound, err
	}

	out := make([]any, len(bound))

	for i, a := range bound {
		out[i] = utc(a)
	}

	return text, out, nil
}

func bind(d Dialect, query string, args []any) (string, []any, error) {
	named := map[string]any{}

	for _, a := range args {
		if n, ok := a.(sql.NamedArg); ok {
			named[n.Name] = n.Value
		}
	}

	switch {
	case len(named) > 0 && len(named) != len(args):
		return "", nil, errors.New("ant: raw query mixes named and positional arguments")
	case len(named) > 0:
		return bindNamed(d, query, named)
	case usesQuestionMarks(query, len(args)):
		return bindMarks(d, query, args)
	}

	return query, args, nil
}

// utc returns v with its time in UTC, for a database whose columns drop the
// zone, wherever the time is held: directly, behind a pointer, in sql.NullTime
// or an sql.Null that holds one, or a type built on either, such as
// ant.DeletedAt, or in a qb.Secret, which stays one. Anything else, and NULL,
// comes back as it was.
func utc(v any) any { return utcAt(v, 0) }

// maxDepth bounds how deep utc follows secrets in secrets, which could hold
// themselves.
const maxDepth = 64

func utcAt(v any, depth int) any {
	if depth > maxDepth {
		return failed{fmt.Errorf("ant: %T holds itself", v)}
	}

	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	case *time.Time:
		if t != nil {
			return t.UTC()
		}
	case qb.Secret:
		return qb.Secret{V: utcAt(t.V, depth+1)}
	case *qb.Secret:
		if t != nil {
			return &qb.Secret{V: utcAt(t.V, depth+1)}
		}
	case driver.Valuer:
		// database/sql treats a nil pointer as NULL without calling Value.
		rv := reflect.ValueOf(t)
		if rv.Kind() == reflect.Pointer && rv.IsNil() || !givesTime(rv, &trail{}) {
			return v
		}

		// A secret inside stays one: what Value gives goes on in a qb.Secret,
		// so that logged values still hide it. That is settled first, as Value
		// may change what the Valuer holds.
		secret := hidesSecret(rv, &trail{})

		// Value runs once: its result goes on in place of v, so that a type
		// that overrides Value is not asked twice.
		value, err := t.Value()

		switch at, ok := value.(time.Time); {
		case err != nil:
			return failed{err}
		case ok:
			value = at.UTC()
		}

		if secret {
			return qb.Secret{V: value}
		}

		return value
	default:
		// database/sql follows pointers, however many, to the first Valuer or
		// value, so ant does too, and a time found there goes on in place of
		// the chain.
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Pointer {
			return v
		}

		// An interface on the way is followed too, as database/sql does when it
		// takes the value a pointer points at.
		onward := func(rv reflect.Value) bool {
			return rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface
		}

		var seen []uintptr

		for onward(rv) && !rv.IsNil() {
			// A chain that comes back to a pointer on it never ends, and
			// database/sql would follow it until its stack ran out.
			if rv.Kind() == reflect.Pointer {
				if slices.Contains(seen, rv.Pointer()) {
					return failed{fmt.Errorf("ant: %T points back at itself", v)}
				}

				seen = append(seen, rv.Pointer())
			}

			rv = rv.Elem()

			if _, ok := rv.Interface().(driver.Valuer); ok {
				break
			}
		}

		// A nil pointer is NULL to database/sql, Valuer or not.
		if onward(rv) && rv.IsNil() {
			return v
		}

		elem := rv.Interface()

		// A Valuer at the end that utc asks gives what goes on, NULL and errors
		// included, rather than having it asked again. Whether it is asked is
		// settled first, since Value may change what the Valuer holds, and the
		// chain may end in an interface, so the Valuer's own type is judged.
		_, valuer := elem.(driver.Valuer)
		asked := valuer && givesTime(reflect.ValueOf(elem), &trail{})

		out := utcAt(elem, depth+1)

		if asked {
			return out
		}

		switch out.(type) {
		case time.Time, qb.Secret, *qb.Secret:
			return out
		}
	}

	return v
}

// failed is a Valuer that gives the error another Valuer gave, so that the
// error reaches database/sql as it would have.
type failed struct{ err error }

func (f failed) Value() (driver.Value, error) { return nil, f.err }

var (
	timeType   = reflect.TypeFor[time.Time]()
	nullTime   = reflect.TypeFor[sql.NullTime]()
	secretType = reflect.TypeFor[qb.Secret]()
	valuerType = reflect.TypeFor[driver.Valuer]()
)

// maxSteps bounds the parts of a value one walk looks at. Parts that share
// pointers are each walked, so a value whose parts keep sharing what follows
// them could take a walk that doubles at every level; no Valuer that gives a
// time comes near this.
const maxSteps = 1 << 12

// trail is what one walk of a value is on: the pointers followed on the way to
// the part it looks at now, and how many parts it has looked at. A value can
// hold itself only through a pointer, as sql.Null[any]{V: &itself} does, so a
// walk that comes back to a pointer on its way stops there. A pointer that two
// parts share, as two embedded fields may, is on neither's way to the other, and
// is followed from each.
type trail struct {
	pointers []uintptr
	steps    int
}

// enter starts on a part of the value, and returns where the way stood, for
// leave to go back to once the part is done. ok is false once the walk has
// looked at maxSteps parts.
func (tr *trail) enter() (at int, ok bool) {
	tr.steps++

	return len(tr.pointers), tr.steps <= maxSteps
}

// leave goes back along the way to where it stood at enter.
func (tr *trail) leave(at int) { tr.pointers = tr.pointers[:at] }

// passed reports whether the pointer v is on the way here already, and puts it
// on the way if not.
func (tr *trail) passed(v reflect.Value) bool {
	if slices.Contains(tr.pointers, v.Pointer()) {
		return true
	}

	tr.pointers = append(tr.pointers, v.Pointer())

	return false
}

// follow takes v past pointers and interfaces, and reports false for nil or a
// pointer on the way here already.
func (tr *trail) follow(v reflect.Value) (reflect.Value, bool) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() || v.Kind() == reflect.Pointer && tr.passed(v) {
			return v, false
		}

		v = v.Elem()
	}

	return v, true
}

// isNull reports whether t is an instantiation of sql.Null.
func isNull(t reflect.Type) bool {
	return t.PkgPath() == "database/sql" && strings.HasPrefix(t.Name(), "Null[")
}

// hidesSecret reports whether the Valuer v gives a qb.Secret's value: it is
// one, or an sql.Null or a struct built on one holds one.
func hidesSecret(v reflect.Value, tr *trail) bool {
	at, ok := tr.enter()
	defer tr.leave(at)

	if !ok {
		return false
	}

	v, ok = tr.follow(v)
	if !ok {
		return false
	}

	switch t := v.Type(); {
	case t == secretType:
		return true
	case isNull(t):
		return hidesSecret(v.FieldByName("V"), tr)
	case t.Kind() != reflect.Struct:
		return false
	}

	for i := range v.NumField() {
		if v.Type().Field(i).Anonymous && hidesSecret(v.Field(i), tr) {
			return true
		}
	}

	return false
}

// givesTime reports whether the Valuer v gives a time: sql.NullTime, an sql.Null
// of a time or of a pointer to one, an sql.Null of an interface that holds a
// time now, or a struct built on one of these, such as ant.DeletedAt. Other
// Valuers, such as JSON or an encrypted value, are left to database/sql, so
// their Value runs once.
func givesTime(v reflect.Value, tr *trail) bool {
	at, ok := tr.enter()
	defer tr.leave(at)

	if !ok {
		return false
	}

	v, ok = tr.follow(v)
	if !ok {
		return false
	}

	t := v.Type()

	switch {
	case t == nullTime:
		return true
	case t == secretType:
		// A secret gives what it holds, as database/sql converts it.
		return holdsTime(v.FieldByName("V"), tr)
	case isNull(t):
		// sql.Null gives what it holds once Valid, as database/sql converts it,
		// following pointers and asking a Valuer it holds. A time, or a pointer
		// to one, is known by its type. An interface, or a Valuer, is known by
		// what it holds now, which matters only once Valid.
		held := v.FieldByName("V")
		if pointsToTime(held.Type()) {
			return true
		}

		return v.FieldByName("Valid").Bool() && holdsTime(held, tr)
	case t.Kind() != reflect.Struct:
		return false
	}

	// A type that embeds a pointer to itself comes back to a pointer it
	// followed, or to nil, so this ends.
	for i := range t.NumField() {
		if t.Field(i).Anonymous && givesTime(v.Field(i), tr) {
			return true
		}
	}

	return false
}

// holdsTime reports whether v, past pointers and interfaces, is a time or a
// Valuer that gives one, as database/sql asks the first Valuer it meets.
func holdsTime(v reflect.Value, tr *trail) bool {
	at, ok := tr.enter()
	defer tr.leave(at)

	if !ok {
		return false
	}

	for {
		// Judged by its type, which a value read through an unexported field
		// can still give.
		if v.Kind() != reflect.Interface && v.Type().Implements(valuerType) {
			return givesTime(v, tr)
		}

		if v.Kind() != reflect.Pointer && v.Kind() != reflect.Interface {
			return v.Type() == timeType
		}

		if v.IsNil() || v.Kind() == reflect.Pointer && tr.passed(v) {
			return false // nil, or a chain that comes back to itself, holds no time
		}

		v = v.Elem()
	}
}

// pointsToTime reports whether t is a time or a pointer, however deep, to one.
// A pointer type may point to itself, as type loop *loop does.
func pointsToTime(t reflect.Type) bool {
	var seen []reflect.Type

	for t.Kind() == reflect.Pointer && !slices.Contains(seen, t) {
		seen = append(seen, t)
		t = t.Elem()
	}

	return t == timeType
}

func bindNamed(d Dialect, query string, named map[string]any) (string, []any, error) {
	b := &binder{d: d}
	used := map[string]string{}

	for i := 0; i < len(query); {
		if end := skipQuoted(query, i); end > i {
			b.out.WriteString(query[i:end])
			i = end

			continue
		}

		name := nameAt(query, i)
		if name == "" {
			b.out.WriteByte(query[i])
			i++

			continue
		}

		value, ok := named[name]
		if !ok {
			return "", nil, fmt.Errorf("ant: raw query has no argument named %q", name)
		}

		i += len(name) + 1

		if text, seen := used[name]; seen {
			b.out.WriteString(text)

			continue
		}

		used[name] = b.value(value, query[i:])
	}

	for name := range named {
		if _, ok := used[name]; !ok {
			return "", nil, fmt.Errorf("ant: raw query does not use argument %q", name)
		}
	}

	return b.out.String(), b.args, nil
}

func bindMarks(d Dialect, query string, args []any) (string, []any, error) {
	b := &binder{d: d}

	for i := 0; i < len(query); {
		if end := skipQuoted(query, i); end > i {
			b.out.WriteString(query[i:end])
			i = end

			continue
		}

		if query[i] != '?' {
			b.out.WriteByte(query[i])
			i++

			continue
		}

		i++
		b.value(args[0], query[i:])
		args = args[1:]
	}

	return b.out.String(), b.args, nil
}

// usesQuestionMarks reports whether ? marks are this query's placeholders.
func usesQuestionMarks(query string, args int) bool {
	if args == 0 {
		return false
	}

	marks := 0

	for i := 0; i < len(query); {
		if end := skipQuoted(query, i); end > i {
			i = end

			continue
		}

		switch {
		case query[i] == '?':
			marks++
		case query[i] == '$' && i+1 < len(query) && '0' <= query[i+1] && query[i+1] <= '9':
			return false
		}

		i++
	}

	return marks == args
}

// nameAt returns the parameter name of an @name starting at i, if there is one.
func nameAt(query string, i int) string {
	if query[i] != '@' {
		return ""
	}

	end := i + 1
	for end < len(query) && isNameByte(query[end], end == i+1) {
		end++
	}

	return query[i+1 : end]
}

// binder collects arguments and writes their placeholders.
type binder struct {
	d    Dialect
	out  strings.Builder
	args []any
}

// value writes the placeholders for v, given the query text after it, and
// returns what it wrote.
func (b *binder) value(v any, rest string) string {
	values := []any{v}

	if list, ok := expandable(v); ok && insideIn(b.out.String(), rest) {
		values = list
	}

	if len(values) == 0 {
		b.out.WriteString("NULL")

		return "NULL"
	}

	placeholders := make([]string, len(values))

	for i, value := range values {
		b.args = append(b.args, value)
		placeholders[i] = b.d.Placeholder(len(b.args))
	}

	text := strings.Join(placeholders, ", ")
	b.out.WriteString(text)

	return text
}

// expandable returns the elements of a slice that stands for a list of values.
// []byte is one value, as is any type that converts itself.
func expandable(v any) ([]any, bool) {
	if _, ok := v.(driver.Valuer); ok {
		return nil, false
	}

	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice || rv.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}

	list := make([]any, rv.Len())
	for i := range list {
		list[i] = rv.Index(i).Interface()
	}

	return list, true
}

// insideIn reports whether a placeholder between before and after is the whole
// of an IN (...) list.
func insideIn(before, after string) bool {
	before = strings.TrimSpace(before)
	if !strings.HasPrefix(strings.TrimSpace(after), ")") || !strings.HasSuffix(before, "(") {
		return false
	}

	before = strings.TrimSpace(strings.TrimSuffix(before, "("))

	return len(before) >= 2 && strings.EqualFold(before[len(before)-2:], "IN") &&
		(len(before) == 2 || !isNameByte(before[len(before)-3], true))
}

func isNameByte(c byte, first bool) bool {
	letter := c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'

	return letter || !first && '0' <= c && c <= '9'
}

// skipQuoted returns the end of the quoted region starting at i, or i if none
// starts there.
func skipQuoted(q string, i int) int {
	rest := q[i:]

	switch {
	case rest[0] == '\'', rest[0] == '"':
		for j := 1; j < len(rest); j++ {
			if rest[j] == rest[0] {
				if j+1 < len(rest) && rest[j+1] == rest[0] {
					j++

					continue
				}

				return i + j + 1
			}
		}

		return len(q)
	case strings.HasPrefix(rest, "--"):
		if end := strings.IndexByte(rest, '\n'); end >= 0 {
			return i + end
		}

		return len(q)
	case strings.HasPrefix(rest, "/*"):
		if end := strings.Index(rest, "*/"); end >= 0 {
			return i + end + 2
		}

		return len(q)
	case rest[0] == '$':
		tag := 1
		for tag < len(rest) && isNameByte(rest[tag], tag == 1) {
			tag++
		}

		if tag == len(rest) || rest[tag] != '$' {
			return i
		}

		delimiter := rest[:tag+1]
		if end := strings.Index(rest[tag+1:], delimiter); end >= 0 {
			return i + tag + 1 + end + len(delimiter)
		}

		return len(q)
	}

	return i
}
