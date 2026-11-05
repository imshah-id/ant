package schema

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Hint is a possible rename that a diff saw but cannot be sure of. A rename and a
// drop plus an add look identical, and only a person knows which was meant.
type Hint struct {
	Table Ref
	// Column is empty when the hint is about the table itself.
	Column string
	To     string
}

// Flag returns the value for a rename in Diff that takes up this hint.
func (h Hint) Flag() string {
	if h.Column == "" {
		return h.Table.String() + "=" + h.To
	}

	return fmt.Sprintf("%s.%s=%s", h.Table, h.Column, h.To)
}

// Change returns the rename this hint suggests.
func (h Hint) Change() Change {
	if h.Column == "" {
		return RenameTable{Table: h.Table, To: h.To}
	}

	return RenameColumn{Table: h.Table, From: h.Column, To: h.To}
}

// String says what was seen, for a person to judge.
func (h Hint) String() string {
	if h.Column == "" {
		return fmt.Sprintf("table %s was removed and %s was added", h.Table, h.To)
	}

	return fmt.Sprintf("column %s.%s was removed and %s was added", h.Table, h.Column, h.To)
}

// Plan is the ordered list of changes from one schema to another.
type Plan struct {
	Changes []Change
	Hints   []Hint
}

// Empty reports whether there is nothing to do.
func (p *Plan) Empty() bool { return len(p.Changes) == 0 }

// Warnings returns the warnings of every change.
func (p *Plan) Warnings() []string {
	var out []string

	for _, c := range p.Changes {
		if w := c.Warning(); w != "" {
			out = append(out, w)
		}
	}

	return out
}

// Online reports whether any change must run outside a transaction.
func (p *Plan) Online() bool {
	return slices.ContainsFunc(p.Changes, func(c Change) bool {
		o, ok := c.(Online)

		return ok && o.RunsOnline()
	})
}

// phases hold changes in the order they must run. Foreign keys go first and
// return last, so that nothing depends on what changes in between, and renames
// come before everything, so that later steps can use the new names.
type phases struct {
	schemas, types, renames, dropKeys, dropRest, dropTables, dropColumns []Change
	createTables, addColumns, alterColumns, addRest, addKeys, comments   []Change
	dropTypes                                                            []Change

	// unique holds the columns of each primary key, unique constraint, and
	// unique index that the plan drops from a table it keeps. A foreign key that
	// points at the same columns may depend on one of them, and PostgreSQL will
	// not drop it until the foreign key is gone.
	unique map[Ref][]columns
}

func (p *phases) plan() []Change {
	return slices.Concat(
		p.schemas,
		p.types,
		p.renames,
		p.dropKeys,
		p.dropRest,
		p.dropTables,
		p.dropColumns,
		p.createTables,
		p.addColumns,
		p.alterColumns,
		p.addRest,
		p.addKeys,
		p.comments,
		p.dropTypes,
	)
}

// compareTypes works out what changes in enums and sequences. They come before
// tables, which use them, and go after, once nothing does.
func (p *phases) compareTypes(old, to *Schema) {
	for _, e := range to.Enums {
		existing := old.Enum(e.Name)
		if existing == nil {
			p.types = append(p.types, CreateEnum{Enum: e})

			continue
		}

		for _, value := range e.Values {
			if !slices.Contains(existing.Values, value) {
				p.types = append(p.types, AddEnumValue{Enum: e.Name, Value: value})
			}
		}

		for _, value := range existing.Values {
			if !slices.Contains(e.Values, value) {
				p.types = append(p.types, RemoveEnumValue{Enum: e.Name, Value: value})
			}
		}
	}

	for _, e := range old.Enums {
		if to.Enum(e.Name) == nil {
			p.dropTypes = append(p.dropTypes, DropEnum{Enum: e.Name})
		}
	}

	for _, q := range to.Sequences {
		if old.Sequence(q.Name) == nil {
			p.types = append(p.types, CreateSequence{Sequence: q})
		}
	}

	for _, q := range old.Sequences {
		if to.Sequence(q.Name) == nil {
			p.dropTypes = append(p.dropTypes, DropSequence{Sequence: q.Name})
		}
	}
}

// Diff returns the plan from from to to. Either can be nil, meaning empty.
//
// Each rename says that something in from has a new name in to, as
// table=new_name or table.column=new_name, and a table can be written
// schema.table. A rename the diff cannot apply is an error, because the caller
// meant it to prevent a drop.
func Diff(from, to *Schema, renames ...string) (*Plan, error) {
	old := from.Clone()

	if to == nil {
		to = &Schema{}
	}

	var p phases

	for _, text := range renames {
		change, err := old.rename(text, to)
		if err != nil {
			return nil, err
		}

		p.renames = append(p.renames, change)
	}

	for _, name := range to.Schemas {
		if !slices.Contains(old.Schemas, name) {
			p.schemas = append(p.schemas, CreateSchema{Name: name})
		}
	}

	p.compareTypes(old, to)

	gone := replaced(old, to)

	for _, t := range to.Tables {
		o := old.Table(t.Ref())
		if o == nil {
			p.createTable(t)

			continue
		}

		p.alterTable(o, t, gone)
	}

	// Foreign keys wait until every table has said which unique keys it drops.
	for _, t := range to.Tables {
		p.foreignKeys(old.Table(t.Ref()), t, gone)
	}

	for _, o := range old.Tables {
		if to.Table(o.Ref()) == nil {
			for _, key := range o.ForeignKeys {
				p.dropKeys = append(p.dropKeys, DropForeignKey{Table: o.Ref(), Key: key})
			}

			p.dropTables = append(p.dropTables, DropTable{Table: o.Ref()})
		}
	}

	changes := p.plan()

	if len(gone) > 0 {
		atomic(changes)
	}

	return &Plan{Changes: changes, Hints: hints(old, to)}, nil
}

// rename applies a rename to s, as the database would.
func (s *Schema) rename(text string, to *Schema) (Change, error) {
	path, name, ok := strings.Cut(text, "=")
	if !ok || name == "" {
		return nil, fmt.Errorf("ant: rename %q: want table=new_name or table.column=new_name", text)
	}

	parts := strings.Split(path, ".")
	if len(parts) == 1 || len(parts) == 2 && s.Table(Ref{"", parts[0]}) == nil {
		ref := Ref{Name: parts[len(parts)-1]}

		if len(parts) == 2 {
			ref.Schema = parts[0]
		}

		t := s.Table(ref)
		if t == nil {
			return nil, fmt.Errorf("ant: rename %q: no table %s in the snapshot", text, ref)
		}

		if err := s.canRenameTable(t, name, to); err != nil {
			return nil, fmt.Errorf("ant: rename %q: %w", text, err)
		}

		return s.renameTable(t, name), nil
	}

	ref := Ref{Name: parts[0]}

	column := parts[len(parts)-1]

	if len(parts) == 3 {
		ref = Ref{parts[0], parts[1]}
	}

	t := s.Table(ref)
	if t == nil {
		return nil, fmt.Errorf("ant: rename %q: no table %s in the snapshot", text, ref)
	}

	c := t.Column(column)
	if c == nil {
		return nil, fmt.Errorf("ant: rename %q: no column %s in table %s", text, column, ref)
	}

	if err := canRenameColumn(t, name, to); err != nil {
		return nil, fmt.Errorf("ant: rename %q: %w", text, err)
	}

	s.renameColumn(t, c, name)

	return RenameColumn{Table: ref, From: column, To: name}, nil
}

// canRenameTable checks that t's new name is a table the models have, in t's
// schema, and that the snapshot does not have yet. A rename to a name the models
// lack would be followed by a drop of the renamed table, losing what the rename
// was meant to keep.
func (s *Schema) canRenameTable(t *Table, name string, to *Schema) error {
	ref := Ref{t.Schema, name}

	if s.Table(ref) != nil {
		return fmt.Errorf("the snapshot already has a table %s", ref)
	}

	if slices.Contains(s.Unmanaged, ref) {
		return fmt.Errorf("a table %s already exists, outside migrations", ref)
	}

	if to.Table(ref) != nil {
		return nil
	}

	var fresh []string

	for _, candidate := range to.Tables {
		if candidate.Schema == t.Schema && s.Table(candidate.Ref()) == nil &&
			!slices.Contains(s.Unmanaged, candidate.Ref()) {
			fresh = append(fresh, candidate.Name)
		}
	}

	where := ""

	if t.Schema != "" {
		where = " in schema " + t.Schema
	}

	if len(fresh) == 0 {
		return fmt.Errorf("models have no table %s%s; the models have no new table", name, where)
	}

	return fmt.Errorf(
		"models have no table %s%s; new tables in the models: %s",
		name,
		where,
		strings.Join(fresh, ", "),
	)
}

// canRenameColumn checks that a column of t can take the new name: the models'
// table has it, and t does not yet.
func canRenameColumn(t *Table, name string, to *Schema) error {
	if t.Column(name) != nil {
		return fmt.Errorf("table %s already has a column %s", t.Ref(), name)
	}

	target := to.Table(t.Ref())
	if target == nil {
		return fmt.Errorf("models have no table %s", t.Ref())
	}

	if target.Column(name) != nil {
		return nil
	}

	var fresh []string

	for _, c := range target.Columns {
		if t.Column(c.Name) == nil {
			fresh = append(fresh, c.Name)
		}
	}

	if len(fresh) == 0 {
		return fmt.Errorf(
			"models have no column %s.%s; %s has no new column in the models",
			t.Ref(),
			name,
			t.Ref(),
		)
	}

	return fmt.Errorf(
		"models have no column %s.%s; new columns of %s in the models: %s",
		t.Ref(),
		name,
		t.Ref(),
		strings.Join(fresh, ", "),
	)
}

func (s *Schema) renameTable(t *Table, name string) Change {
	change := RenameTable{Table: t.Ref(), To: name}

	for _, other := range s.Tables {
		for _, key := range other.ForeignKeys {
			if key.RefSchema == t.Schema && key.RefTable == t.Name {
				key.RefTable = name
			}
		}
	}

	t.Name = name

	return change
}

func (s *Schema) renameColumn(t *Table, c *Column, name string) {
	swap := func(columns []string) {
		for i := range columns {
			if columns[i] == c.Name {
				columns[i] = name
			}
		}
	}

	if t.PrimaryKey != nil {
		swap(t.PrimaryKey.Columns)
	}

	for _, key := range t.Uniques {
		swap(key.Columns)
	}

	for _, index := range t.Indexes {
		swap(index.Include)

		for i := range index.Parts {
			if index.Parts[i].Column == c.Name {
				index.Parts[i].Column = name
			}
		}
	}

	for _, key := range t.ForeignKeys {
		swap(key.Columns)
	}

	for _, other := range s.Tables {
		for _, key := range other.ForeignKeys {
			if key.RefSchema == t.Schema && key.RefTable == t.Name {
				swap(key.RefColumns)
			}
		}
	}

	c.Name = name
}

func (p *phases) createTable(t *Table) {
	ref := t.Ref()

	p.createTables = append(p.createTables, CreateTable{Table: t})

	if t.Comment != "" {
		p.comments = append(p.comments, SetTableComment{Table: ref, Comment: t.Comment})
	}

	for _, c := range t.Columns {
		if c.Comment != "" {
			p.comments = append(
				p.comments,
				SetColumnComment{Table: ref, Column: c.Name, Comment: c.Comment},
			)
		}
	}

	for _, index := range t.Indexes {
		p.addRest = append(p.addRest, CreateIndex{Table: ref, Index: index, New: true})
	}
}

// replaced returns the columns that the plan drops and adds again, by table.
func replaced(old, to *Schema) map[Ref]columns {
	out := map[Ref]columns{}

	for _, t := range to.Tables {
		o := old.Table(t.Ref())
		if o == nil {
			continue
		}

		for _, c := range t.Columns {
			existing := o.Column(c.Name)

			if existing != nil && (AlterColumn{From: existing, To: c}).Replaces() {
				out[t.Ref()] = append(out[t.Ref()], c.Name)
			}
		}
	}

	return out
}

// columns is a set of column names.
type columns []string

// anyOf reports whether names holds one of the columns.
func (s columns) anyOf(names []string) bool {
	return slices.ContainsFunc(names, func(name string) bool { return slices.Contains(s, name) })
}

// sameSet reports whether names are the columns, in any order, which is how a
// foreign key finds the unique key it points at.
func (s columns) sameSet(names []string) bool {
	return len(s) == len(names) &&
		!slices.ContainsFunc(names, func(name string) bool { return !slices.Contains(s, name) })
}

// key returns the columns of an index a foreign key can point at: a unique one,
// on plain columns, with no predicate.
func (i *Index) key() (columns, bool) {
	if !i.Unique || i.Where != "" {
		return nil, false
	}

	out := make(columns, len(i.Parts))

	for n, part := range i.Parts {
		if part.Expr != "" {
			return nil, false
		}

		out[n] = part.Column
	}

	return out, true
}

// dropsUnique records that the plan drops a unique key on these columns.
func (p *phases) dropsUnique(table Ref, key columns) {
	if p.unique == nil {
		p.unique = map[Ref][]columns{}
	}

	p.unique[table] = append(p.unique[table], key)
}

// mentioned reports whether a SQL expression names one of the columns. It errs
// toward yes, as a string literal with the name counts too, which costs no more
// than rebuilding something that did not need it.
func (s columns) mentioned(expr string) bool {
	return slices.ContainsFunc(s, func(name string) bool { return mentions(expr, name) })
}

// usedBy reports whether an index is on one of the columns, includes one, or
// names one in an expression or its predicate.
func (s columns) usedBy(i *Index) bool {
	for _, part := range i.Parts {
		if slices.Contains(s, part.Column) || s.mentioned(part.Expr) {
			return true
		}
	}

	return s.anyOf(i.Include) || s.mentioned(i.Where)
}

// mentions reports whether expr has name as a whole identifier. Unquoted names
// fold to lower case, so case does not matter.
func mentions(expr, name string) bool {
	if name == "" {
		return false
	}

	if spelledOut(expr, name) {
		return true
	}

	expr, name = strings.ToLower(expr), strings.ToLower(name)

	// A name with a double quote in it only appears quoted, with the quote doubled.
	if strings.Contains(expr, `"`+strings.ReplaceAll(name, `"`, `""`)+`"`) {
		return true
	}

	for from := 0; ; {
		at := strings.Index(expr[from:], name)
		if at < 0 {
			return false
		}

		start := from + at
		end := start + len(name)

		if !identifierAt(expr, start-1) && !identifierAt(expr, end) {
			return true
		}

		from = start + 1
	}
}

// identifierAt reports whether s[i] can be part of an unquoted identifier. s is in
// lower case, and bytes past ASCII are letters of some other alphabet.
func identifierAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}

	c := s[i]

	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 0x80
}

// spelledOut reports whether expr has name as a Unicode-escaped identifier, such
// as U&"sl\0075g" for slug, in any case. The escape character is a backslash
// unless a UESCAPE clause, which may come after comments, picks another, so every
// ASCII byte in the identifier is tried as one: a wrong one can only find the name
// where it is not.
func spelledOut(expr, name string) bool {
	for from := 0; ; {
		at := strings.Index(expr[from:], `&"`)
		if at < 0 {
			return false
		}

		start := from + at + len(`&"`)
		from = start

		if start < 3 || expr[start-3] != 'u' && expr[start-3] != 'U' {
			continue
		}

		end := start

		for end < len(expr) && (expr[end] != '"' || strings.HasPrefix(expr[end:], `""`)) {
			if expr[end] == '"' {
				end++
			}

			end++
		}

		body := strings.ReplaceAll(expr[start:end], `""`, `"`)

		for i := 0; i < len(body); i++ {
			escape := body[i]

			if escape >= 0x80 || strings.IndexByte(body[:i], escape) >= 0 {
				continue
			}

			if spelled, ok := unescape(body, escape); ok && strings.EqualFold(spelled, name) {
				return true
			}
		}
	}
}

// unescape decodes the body of a Unicode-escaped identifier. The escape and four
// hex digits, or the escape, a plus sign and six, make a code point; two escapes
// make one; and a UTF-16 surrogate pair makes the code point it encodes.
func unescape(body string, escape byte) (string, bool) {
	var out strings.Builder

	var high rune

	for i := 0; i < len(body); {
		if body[i] != escape || strings.HasPrefix(body[i+1:], string(escape)) {
			if high != 0 {
				return "", false
			}

			out.WriteByte(body[i])

			if body[i] == escape {
				i++
			}

			i++

			continue
		}

		at := i + 1
		size := 4

		if strings.HasPrefix(body[at:], "+") {
			at++
			size = 6
		}

		if at+size > len(body) {
			return "", false
		}

		code, err := strconv.ParseUint(body[at:at+size], 16, 32)
		if err != nil {
			return "", false
		}

		i = at + size
		r := rune(code)

		switch {
		case high != 0:
			r = utf16.DecodeRune(high, r)
			high = 0

			if r == utf8.RuneError {
				return "", false
			}
		case r >= 0xd800 && r < 0xdc00:
			high = r

			continue
		case !utf8.ValidRune(r):
			return "", false
		}

		out.WriteRune(r)
	}

	return out.String(), high == 0
}

// atomic keeps every index change in the plan's transaction. A plan that
// replaces a column needs one: run in parts, a failure could leave the column
// replaced with its indexes and constraints gone. An online build anywhere in it
// would take the transaction away from all of it.
func atomic(changes []Change) {
	for i, change := range changes {
		switch c := change.(type) {
		case CreateIndex:
			c.Atomic = true
			changes[i] = c
		case DropIndex:
			c.Atomic = true
			changes[i] = c
		}
	}
}

// alterTable compares two versions of a table, except for foreign keys. gone
// holds the columns the plan replaces, by table: dropping a column takes its
// indexes and constraints with it.
func (p *phases) alterTable(o, t *Table, gone map[Ref]columns) {
	ref := t.Ref()

	here := gone[ref]

	if o.Comment != t.Comment {
		p.comments = append(p.comments, SetTableComment{Table: ref, Comment: t.Comment})
	}

	for _, c := range t.Columns {
		existing := o.Column(c.Name)

		switch {
		case existing == nil:
			p.addColumns = append(p.addColumns, AddColumn{Table: ref, Column: c})
		case !existing.sameAs(c):
			p.alterColumns = append(p.alterColumns, AlterColumn{Table: ref, From: existing, To: c})
		}

		// The comment the column has once the changes above have run.
		comment := ""

		if existing != nil && !slices.Contains(here, c.Name) {
			comment = existing.Comment
		}

		if comment != c.Comment {
			p.comments = append(
				p.comments,
				SetColumnComment{Table: ref, Column: c.Name, Comment: c.Comment},
			)
		}
	}

	for _, c := range o.Columns {
		if t.Column(c.Name) == nil {
			p.dropColumns = append(p.dropColumns, DropColumn{Table: ref, Column: c})
		}
	}

	switch {
	case o.PrimaryKey != nil && t.PrimaryKey == nil:
		p.dropRest = append(p.dropRest, DropPrimaryKey{Table: ref, Key: o.PrimaryKey})
		p.dropsUnique(ref, o.PrimaryKey.Columns)
	case o.PrimaryKey == nil && t.PrimaryKey != nil:
		p.addRest = append(p.addRest, AddPrimaryKey{Table: ref, Key: t.PrimaryKey})
	case o.PrimaryKey != nil &&
		(!o.PrimaryKey.sameAs(t.PrimaryKey) || here.anyOf(o.PrimaryKey.Columns)):
		p.dropRest = append(p.dropRest, DropPrimaryKey{Table: ref, Key: o.PrimaryKey})
		p.dropsUnique(ref, o.PrimaryKey.Columns)
		p.addRest = append(p.addRest, AddPrimaryKey{Table: ref, Key: t.PrimaryKey})
	case o.PrimaryKey != nil && o.PrimaryKey.Name != t.PrimaryKey.Name:
		p.renames = append(
			p.renames,
			RenameConstraint{Table: ref, From: o.PrimaryKey.Name, To: t.PrimaryKey.Name},
		)
	}

	objects(
		o.Uniques,
		t.Uniques,
		func(k *Key) string { return k.Name },
		(*Key).sameAs,
		func(k *Key) bool { return here.anyOf(k.Columns) },
		func(k *Key) {
			p.dropRest = append(p.dropRest, DropConstraint{Table: ref, Name: k.Name})
			p.dropsUnique(ref, k.Columns)
		},
		func(k *Key) { p.addRest = append(p.addRest, AddUnique{Table: ref, Key: k}) },
		func(from, to string) {
			p.renames = append(p.renames, RenameConstraint{Table: ref, From: from, To: to})
		},
	)

	objects(
		o.Checks,
		t.Checks,
		func(c *Check) string { return c.Name },
		(*Check).sameAs,
		func(c *Check) bool { return here.mentioned(c.Expr) },
		func(c *Check) {
			p.dropRest = append(p.dropRest, DropConstraint{Table: ref, Name: c.Name})
		},
		func(c *Check) { p.addRest = append(p.addRest, AddCheck{Table: ref, Check: c}) },
		func(from, to string) {
			p.renames = append(p.renames, RenameConstraint{Table: ref, From: from, To: to})
		},
	)

	objects(
		o.Indexes,
		t.Indexes,
		func(i *Index) string { return i.Name },
		(*Index).sameAs,
		here.usedBy,
		func(i *Index) {
			if key, ok := i.key(); ok {
				p.dropsUnique(ref, key)
			}

			p.dropRest = append(p.dropRest, DropIndex{Table: ref, Index: i})
		},
		func(i *Index) { p.addRest = append(p.addRest, CreateIndex{Table: ref, Index: i}) },
		func(from, to string) {
			p.renames = append(p.renames, RenameIndex{Table: ref, From: from, To: to})
		},
	)
}

// foreignKeys compares the foreign keys of two versions of a table, or adds them
// to a new one, which o is nil for. A key is dropped first and made again last
// when it is on a replaced column, or points at one, or points at the columns of
// a unique key that the plan drops, since it may depend on that.
func (p *phases) foreignKeys(o, t *Table, gone map[Ref]columns) {
	ref := t.Ref()

	if o == nil {
		for _, key := range t.ForeignKeys {
			p.addKeys = append(p.addKeys, AddForeignKey{Table: ref, Key: key, New: true})
		}

		return
	}

	breaks := func(k *ForeignKey) bool {
		target := Ref{k.RefSchema, k.RefTable}

		return gone[ref].anyOf(k.Columns) || gone[target].anyOf(k.RefColumns) ||
			slices.ContainsFunc(
				p.unique[target],
				func(key columns) bool { return key.sameSet(k.RefColumns) },
			)
	}

	objects(
		o.ForeignKeys,
		t.ForeignKeys,
		func(k *ForeignKey) string { return k.Name },
		(*ForeignKey).sameAs,
		breaks,
		func(k *ForeignKey) { p.dropKeys = append(p.dropKeys, DropForeignKey{Table: ref, Key: k}) },
		func(k *ForeignKey) { p.addKeys = append(p.addKeys, AddForeignKey{Table: ref, Key: k}) },
		func(from, to string) {
			p.renames = append(p.renames, RenameConstraint{Table: ref, From: from, To: to})
		},
	)
}

// objects compares two lists of named objects. An object with the same definition
// under a new name is renamed, so that an index on a large table is not rebuilt
// because its name changed. An object for which uses is true is dropped and made
// again, even if nothing about it changed, because something it needs is dropped:
// a replaced column, or the unique key a foreign key points at.
func objects[T any](
	old, next []T,
	name func(T) string,
	same func(a, b T) bool,
	uses func(T) bool,
	drop, add func(T),
	rename func(from, to string),
) {
	find := func(list []T, wanted string) (T, bool) {
		for _, item := range list {
			if name(item) == wanted {
				return item, true
			}
		}

		var zero T

		return zero, false
	}

	taken := map[string]bool{}

	var added []T

	for _, item := range next {
		if existing, ok := find(old, name(item)); ok {
			taken[name(existing)] = true

			if !same(existing, item) || uses(existing) || uses(item) {
				drop(existing)
				add(item)
			}

			continue
		}

		added = append(added, item)
	}

	for _, item := range added {
		matched := false

		for _, existing := range old {
			if !taken[name(existing)] && same(existing, item) && !uses(existing) {
				taken[name(existing)] = true
				rename(name(existing), name(item))

				matched = true

				break
			}
		}

		if !matched {
			add(item)
		}
	}

	for _, existing := range old {
		if !taken[name(existing)] {
			drop(existing)
		}
	}
}

// hints finds what looks like a rename that nobody declared.
func hints(old, to *Schema) []Hint {
	var (
		out              []Hint
		dropped, created []*Table
	)

	for _, t := range old.Tables {
		if to.Table(t.Ref()) == nil {
			dropped = append(dropped, t)
		}
	}

	for _, t := range to.Tables {
		o := old.Table(t.Ref())
		if o == nil {
			created = append(created, t)

			continue
		}

		for _, gone := range o.Columns {
			if t.Column(gone.Name) != nil {
				continue
			}

			for _, added := range t.Columns {
				if o.Column(added.Name) == nil && added.Type == gone.Type {
					out = append(out, Hint{Table: t.Ref(), Column: gone.Name, To: added.Name})
				}
			}
		}
	}

	for _, gone := range dropped {
		for _, added := range created {
			if sameColumns(gone, added) {
				out = append(out, Hint{Table: gone.Ref(), To: added.Name})
			}
		}
	}

	return out
}

func sameColumns(a, b *Table) bool {
	return len(a.Columns) == len(b.Columns) &&
		!slices.ContainsFunc(a.Columns, func(c *Column) bool {
			other := b.Column(c.Name)

			return other == nil || other.Type != c.Type
		})
}
