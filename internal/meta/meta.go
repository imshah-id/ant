// Package meta is the model registry. It reflects each struct once and maps
// probe field addresses to columns.
package meta

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"

	"github.com/skudra-go/ant/internal/errs"
	"github.com/skudra-go/ant/qb"
)

// Field is a mapped column.
type Field struct {
	Name    string       // Go path, such as "Base.ID"
	Short   string       // Go name, such as "ID"
	Column  string       // database column
	Type    reflect.Type // Go type
	Offset  uintptr      // offset from the start of the model
	Primary bool
	Unique  bool
	// Generated means the database assigns the value when the field is zero.
	// This is the only place ant reads meaning into a zero value.
	Generated bool
	// Computed fields are never written, and are read only when a query asks.
	Computed bool
	// ReadOnly fields are filled by the database and never written.
	ReadOnly bool
	// WriteOnly fields are written and never read, such as password hashes.
	WriteOnly bool
	// Default fields use the column's default when zero, as generated keys do.
	Default bool
	// Immutable fields are written on insert and never updated.
	Immutable bool
	// Token fields are compared on update and delete: the row must still hold
	// the value that was loaded.
	Token bool
	// Version is a token that goes up by one with every write. It must be a
	// signed integer.
	Version bool
	// Named means the column name was set explicitly, so naming leaves it alone.
	Named bool
	// Prefix is what an embedded struct adds to the column name.
	Prefix string
	// Sensitive values stay out of logs, even with LogValues.
	Sensitive bool
	// Generator makes a key for a new entity in Go.
	Generator func(entity unsafe.Pointer)
	// Equal compares two values of this field for change tracking, in place of
	// the default deep comparison.
	Equal func(a, b unsafe.Pointer) bool
	// Storage is how migrations declare the column.
	Storage Storage

	pointer func(unsafe.Pointer) any
}

// Storage is how a column is declared in migrations. Queries ignore it. Unset
// values come from the Go type.
type Storage struct {
	Type       string // database type, used as written
	Length     int
	Fixed      bool // Length is exact, not a limit
	Precision  int
	Scale      int
	Nullable   *bool
	Default    string // SQL expression
	Generated  string // SQL expression of a stored generated column
	Collation  string
	UniqueName string
	Comment    string
	Sequence   *Sequence
	Enum       *Enum
}

// Sequence is a database sequence a column draws from.
type Sequence struct {
	Name      string
	Start     int64
	Increment int64
}

// Enum is a native enum type a column uses.
type Enum struct {
	Name   string
	Values []string
}

// FromDatabase reports whether the database fills this field when it is zero.
func (f *Field) FromDatabase() bool { return f.Generated || f.Default }

// Addr returns a pointer to this field of the model value at entity, typed as
// *Field.Type. Offsets come from reflect, so the result stays within entity.
func (f *Field) Addr(entity unsafe.Pointer) any {
	return f.pointer(unsafe.Add(entity, f.Offset))
}

// Value returns the value of this field of the model value at entity.
func (f *Field) Value(entity unsafe.Pointer) reflect.Value {
	return reflect.NewAt(f.Type, unsafe.Add(entity, f.Offset)).Elem()
}

// Key returns this field of the model value at entity in a form that compares
// equal across keys and foreign keys of different Go types, or false if NULL.
func (f *Field) Key(entity unsafe.Pointer) (any, bool) {
	return keyOf(f.Value(entity))
}

// KeyValue returns v in the form Field.Key uses.
func KeyValue(v any) (any, bool) { return keyOf(reflect.ValueOf(v)) }

func keyOf(v reflect.Value) (any, bool) {
	if !v.IsValid() {
		return nil, false
	}

	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil, false
		}

		v = v.Elem()
	}

	if valuer, ok := v.Interface().(driver.Valuer); ok {
		value, err := valuer.Value()
		if err != nil || value == nil {
			return nil, false
		}

		return keyOf(reflect.ValueOf(value))
	}

	switch {
	case v.CanInt():
		return v.Int(), true
	case v.CanUint():
		return int64(v.Uint()), true
	case v.Kind() == reflect.String:
		return v.String(), true
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8:
		return string(v.Bytes()), true
	}

	return v.Interface(), true
}

// pointerTo returns the function that types a field pointer. Common types need
// no reflection, which saves an allocation per column per row.
func pointerTo(t reflect.Type) func(unsafe.Pointer) any {
	if f, ok := pointers[t]; ok {
		return f
	}

	return func(p unsafe.Pointer) any { return reflect.NewAt(t, p).Interface() }
}

func typed[T any]() (reflect.Type, func(unsafe.Pointer) any) {
	return reflect.TypeFor[T](), func(p unsafe.Pointer) any { return (*T)(p) }
}

var pointers = func() map[reflect.Type]func(unsafe.Pointer) any {
	out := map[reflect.Type]func(unsafe.Pointer) any{}
	add := func(t reflect.Type, f func(unsafe.Pointer) any) { out[t] = f }
	add(typed[string]())
	add(typed[int]())
	add(typed[int16]())
	add(typed[int32]())
	add(typed[int64]())
	add(typed[uint]())
	add(typed[uint32]())
	add(typed[uint64]())
	add(typed[bool]())
	add(typed[float32]())
	add(typed[float64]())
	add(typed[[]byte]())
	add(typed[time.Time]())
	add(typed[*string]())
	add(typed[*int]())
	add(typed[*int32]())
	add(typed[*int64]())
	add(typed[*bool]())
	add(typed[*float64]())
	add(typed[*time.Time]())

	return out
}()

// RelationKind is the shape of a relation field.
type RelationKind int

// Relation kinds.
const (
	Many RelationKind = iota // []T
	One                      // *T
)

// Action is what the database does to dependent rows.
type Action int

// Slot returns a pointer to the relation field of the model value at entity.
func (r *Relation) Slot(entity unsafe.Pointer) unsafe.Pointer {
	return unsafe.Add(entity, r.Offset)
}

// Related returns the entities this relation holds on the model value at
// entity.
func (r *Relation) Related(entity unsafe.Pointer) []unsafe.Pointer {
	if r.Kind == One {
		if p := *(*unsafe.Pointer)(r.Slot(entity)); p != nil {
			return []unsafe.Pointer{p}
		}

		return nil
	}

	v := reflect.NewAt(r.Type, r.Slot(entity)).Elem()

	out := make([]unsafe.Pointer, v.Len())
	for i := range out {
		out[i] = v.Index(i).Addr().UnsafePointer()
	}

	return out
}

// Relation is a field that refers to another model and is not a column.
type Relation struct {
	Name   string
	Kind   RelationKind
	Target reflect.Type
	Type   reflect.Type
	Offset uintptr
	Owner  *Model

	ForeignKey *Field // set by configuration, on either side
	References *Field // set by configuration
	OnDelete   Action
	OnUpdate   Action
	Pivot      *Pivot // set for many-to-many, by configuration

	// ConstraintName, NoForeignKey, and NoIndex shape what migrations declare.
	ConstraintName string
	NoForeignKey   bool
	NoIndex        bool

	// Scope holds conditions on the target that every use of the relation applies.
	Scope []qb.Cond
	// MorphField and MorphValue make the relation polymorphic: MorphField says
	// which model a row's foreign key refers to. It is on whichever side holds the
	// foreign key, and has no database constraint.
	MorphField *Field
	MorphValue any
	// Pick chooses one row of many for a has-one relation.
	Pick *qb.Order
	// ThroughKey is the column on the target that refers to the intermediate
	// table of a has-many-through or has-one-through relation.
	ThroughKey *Field
	// Touch updates the parent's UpdatedAt when a child is written.
	Touch bool
	// Inverse is the relation on the target that points back at the owner, filled
	// when this relation loads.
	Inverse *Relation

	link     Link
	linkErr  error
	linkOnce sync.Once
}

// Pivot is the join table of a many-to-many relation.
type Pivot struct {
	Table   *qb.TableRef
	Local   string // column referring to the relation's owner
	Foreign string // column referring to its target
	// ReadOnly means the table is a model of its own, reached in passing by a
	// through relation, and is never written through it.
	ReadOnly bool
}

// Link is how a relation's rows connect. Local is on the relation's owner, and
// Foreign is on its target. BelongsTo means the owner holds the foreign key.
type Link struct {
	Target    *Model
	Local     *Field
	Foreign   *Field
	BelongsTo bool
}

// Link resolves the relation from configuration and conventions, once.
// Configure both models first.
func (r *Relation) Link() (Link, error) {
	r.linkOnce.Do(func() { r.link, r.linkErr = r.resolve() })

	return r.link, r.linkErr
}

func (r *Relation) resolve() (Link, error) {
	target := ofType(r.Target)

	missing := fmt.Errorf("%w: %s.%s", errs.ErrNoForeignKey, r.Owner.Type.Name(), r.Name)

	if p := r.Pivot; p != nil {
		local, foreign := r.Owner.Key(), target.Key()
		if local == nil || (foreign == nil && r.ThroughKey == nil) {
			return Link{}, missing
		}

		if p.Local == "" {
			p.Local = ColumnName(r.Owner.Type.Name()) + "_id"
		}

		if p.Foreign == "" {
			p.Foreign = ColumnName(target.Type.Name()) + "_id"
		}

		if r.ThroughKey != nil {
			foreign = r.ThroughKey
		}

		return Link{Target: target, Local: local, Foreign: foreign}, nil
	}

	child := func(fk *Field) (Link, error) {
		local := r.References
		if local == nil {
			local = r.Owner.Key()
		}

		if local == nil {
			return Link{}, missing
		}

		return Link{Target: target, Local: local, Foreign: fk}, nil
	}

	parent := func(fk *Field) (Link, error) {
		foreign := r.References
		if foreign == nil {
			foreign = target.Key()
		}

		if foreign == nil {
			return Link{}, missing
		}

		return Link{
			Target:    target,
			Local:     fk,
			Foreign:   foreign,
			BelongsTo: true,
		}, nil
	}

	if fk := r.ForeignKey; fk != nil {
		if r.Kind == One && r.Owner.Has(fk) {
			return parent(fk)
		}

		return child(fk)
	}

	if r.Kind == One {
		if fk := r.Owner.field(r.Name + "ID"); fk != nil {
			return parent(fk)
		}
	}

	if fk := target.field(r.Owner.Type.Name() + "ID"); fk != nil {
		return child(fk)
	}

	if r.Kind == Many {
		if fk := target.backReference(r.Owner); fk != nil {
			return child(fk)
		}
	}

	return Link{}, missing
}

// backReference returns the foreign key of the only relation from m to owner.
func (m *Model) backReference(owner *Model) *Field {
	var found *Field

	for _, other := range m.Relations {
		if other.Kind != One || other.Target != owner.Type {
			continue
		}

		fk := other.ForeignKey
		if fk == nil {
			fk = m.field(other.Name + "ID")
		}

		if fk == nil || !m.Has(fk) {
			continue
		}

		if found != nil {
			return nil
		}

		found = fk
	}

	return found
}

// Empty reports whether this relation holds nothing on the model value at
// entity.
func (r *Relation) Empty(entity unsafe.Pointer) bool {
	v := reflect.NewAt(r.Type, unsafe.Add(entity, r.Offset)).Elem()

	if r.Kind == Many {
		return v.Len() == 0
	}

	return v.IsNil()
}

// Model is the metadata for one struct type.
type Model struct {
	Type      reflect.Type
	Table     *qb.TableRef // shared by AST nodes, so a rename reaches them
	Fields    []*Field
	Relations []*Relation

	// Keys holds the primary key, of several fields for a composite one.
	Keys []*Field
	// ReadOnly models, such as views, refuse writes.
	ReadOnly bool
	// Created and Updated are maintained on writes.
	Created, Updated *Field
	// Discriminator and DiscriminatorValue mark a type sharing its table: rows of
	// this model hold that value, and queries see only those.
	Discriminator      *Field
	DiscriminatorValue any
	// Version counts writes, and detects conflicting ones.
	Version *Field
	// SoftDelete marks rows deleted instead of removing them.
	SoftDelete *Field
	// Filters apply to every query of this model.
	Filters []Filter
	// Rules check an entity's fields before it is written.
	Rules []Rule
	// Indexes and Checks are declared by migrations.
	Indexes []*Index
	Checks  []Check
	// KeyName overrides the name of the primary key constraint.
	KeyName string
	// Unmanaged models have a table that migrations leave alone.
	Unmanaged bool
	// Comment describes the table in the database.
	Comment string
	// Partition splits the table.
	Partition *Partition
	// Constraints are foreign keys declared on their own, for those no relation
	// can express, such as a key of several columns.
	Constraints []*ForeignKey

	columns   map[string]*Field
	configure sync.Once
	err       error
}

// Partition says how a table is split, declared with b.PartitionBy.
type Partition struct {
	Method  string
	Columns []*Field
}

// ForeignKey is a foreign key declared with b.ForeignKey.
type ForeignKey struct {
	Name     string
	Columns  []*Field
	Target   *Model
	Refs     []*Field
	OnDelete Action
	OnUpdate Action
	NoIndex  bool
}

// Rule is a validation of one field. Check returns an error for a bad value.
type Rule struct {
	Field *Field
	Check func(entity unsafe.Pointer) error
}

// Validators returns the model's field rules.
func (m *Model) Validators() []Rule { return m.Rules }

// Index is an index declared in Configure.
type Index struct {
	Name       string
	Parts      []IndexPart
	Unique     bool
	Method     string
	Where      string
	Include    []*Field
	Concurrent bool
}

// IndexPart is one indexed column or expression.
type IndexPart struct {
	Field *Field
	Expr  string
	Desc  bool
}

// Check is a check constraint declared in Configure.
type Check struct {
	Name string
	Expr string
}

// Filter is a global query filter. A nil condition from Build matches every row.
type Filter struct {
	Name  string
	Build func(context.Context) (qb.Cond, error)
}

// SoftDeleteFilter names the built-in filter of a soft-deleted model.
const SoftDeleteFilter = "ant.soft_delete"

// DeletedAt is the type of a soft delete field. A model with one is deleted by
// setting it, and queries skip rows where it is set.
type DeletedAt struct {
	sql.Null[time.Time]
}

// UnixTime is a time stored as whole seconds since 1970, for a schema that keeps
// integers. On CreatedAt and UpdatedAt, it is maintained like a time.Time.
type UnixTime struct {
	time.Time
}

// ColumnType implements schema.Typed.
func (UnixTime) ColumnType(string) string { return "bigint" }

// Value implements driver.Valuer.
func (t UnixTime) Value() (driver.Value, error) {
	if t.IsZero() {
		return int64(0), nil
	}

	return t.Unix(), nil
}

// Scan implements sql.Scanner.
func (t *UnixTime) Scan(src any) error {
	var seconds sql.NullInt64
	if err := seconds.Scan(src); err != nil {
		return err
	}

	t.Time = time.Time{}

	if seconds.Int64 != 0 {
		t.Time = time.Unix(seconds.Int64, 0).UTC()
	}

	return nil
}

func (m *Model) field(short string) *Field {
	for _, f := range m.Fields {
		if f.Short == short {
			return f
		}
	}

	return nil
}

// Has reports whether f is a field of m.
func (m *Model) Has(f *Field) bool { return slices.Contains(m.Fields, f) }

// Configure runs f once, recording the error it returns.
func (m *Model) Configure(f func() error) {
	m.configure.Do(func() {
		if m.err == nil {
			m.err = f()
		}
	})
}

// Err returns the error from building or configuring the model.
func (m *Model) Err() error { return m.err }

// KeyOf returns the primary key of the model value at entity, false if the
// model has none or part of it is NULL. A composite key comes as a string.
func (m *Model) KeyOf(entity unsafe.Pointer) (any, bool) {
	switch len(m.Keys) {
	case 0:
		return nil, false
	case 1:
		return m.Keys[0].Key(entity)
	}

	var parts strings.Builder

	for _, f := range m.Keys {
		v, ok := f.Key(entity)
		if !ok {
			return nil, false
		}

		text := fmt.Sprint(v)
		fmt.Fprintf(&parts, "%d:%s,", len(text), text)
	}

	return parts.String(), true
}

// Key returns the primary key field, or nil if the model has none or the key is
// composite.
func (m *Model) Key() *Field {
	if len(m.Keys) == 1 {
		return m.Keys[0]
	}

	return nil
}

// SetKey makes fields the primary key. A single integer key is generated.
func (m *Model) SetKey(fields ...*Field) {
	for _, other := range m.Fields {
		other.Primary, other.Generated = false, false
	}

	m.Keys = fields

	for _, f := range fields {
		f.Primary = true
	}

	if len(fields) == 1 {
		fields[0].Generated = IsInteger(fields[0].Type)
	}
}

// Col returns the AST column for f.
func (m *Model) Col(f *Field) qb.Column {
	return qb.Column{Table: m.Table, Name: f.Column}
}

// Stored returns the fields queries read, in declaration order.
func (m *Model) Stored() []*Field {
	out := make([]*Field, 0, len(m.Fields))

	for _, f := range m.Fields {
		if !f.Computed && !f.WriteOnly {
			out = append(out, f)
		}
	}

	return out
}

// Writable returns the fields inserts write, in declaration order.
func (m *Model) Writable() []*Field {
	out := make([]*Field, 0, len(m.Fields))

	for _, f := range m.Fields {
		if !f.Computed && !f.ReadOnly {
			out = append(out, f)
		}
	}

	return out
}

// Tokens returns the fields compared on writes.
func (m *Model) Tokens() []*Field {
	var out []*Field

	for _, f := range m.Fields {
		if f.Token {
			out = append(out, f)
		}
	}

	return out
}

// Updatable returns the fields updates can write.
func (m *Model) Updatable() []*Field {
	return slices.DeleteFunc(m.Writable(), func(f *Field) bool {
		return f.Primary || f.Immutable || f == m.Created
	})
}

// Column returns the field mapped to the given column name.
func (m *Model) Column(name string) *Field { return m.columns[name] }

// Rename applies naming functions to a model not yet used, keeping names set
// explicitly. Nil functions keep the conventions.
func (m *Model) Rename(table, column func(string) string) {
	if table != nil {
		m.Table.Name = table(m.Type.Name())
	}

	if column == nil {
		return
	}

	clear(m.columns)

	for _, f := range m.Fields {
		if !f.Named {
			f.Column = f.Prefix + column(f.Short)
		}

		m.columns[f.Column] = f
	}
}

// Of returns the metadata for T, building it on first use.
func Of[T any]() *Model {
	return ofType(reflect.TypeFor[T]())
}

var (
	mu     sync.RWMutex
	models = map[reflect.Type]*Model{}
	probes []probe
)

// probe keeps a probe value reachable, and with it the address range that
// identifies its fields. It relies on heap objects not moving, which the Go
// runtime does not do today. TestProbeSurvivesGC covers this use.
type probe struct {
	ptr   unsafe.Pointer
	base  uintptr
	model *Model
}

// Probe returns the probe for T, allocating and registering it on first use.
// There is one per type, so calling this in a loop costs nothing and leaks nothing.
func Probe[T any]() *T {
	model := Of[T]()

	mu.Lock()
	defer mu.Unlock()

	for _, existing := range probes {
		if existing.model == model {
			return (*T)(existing.ptr)
		}
	}

	p := new(T)
	probes = append(probes, probe{unsafe.Pointer(p), uintptr(unsafe.Pointer(p)), model})

	return p
}

// Ref is a resolved field pointer.
type Ref struct {
	Model    *Model
	Field    *Field
	Relation *Relation
}

// Resolve identifies a probe field by address and type. The type is needed
// because a struct and its first field share an address.
func Resolve[V any](ptr *V) (Ref, bool) {
	addr := uintptr(unsafe.Pointer(ptr))
	t := reflect.TypeFor[V]()

	mu.RLock()
	defer mu.RUnlock()

	for _, p := range probes {
		if addr < p.base || addr >= p.base+p.model.Type.Size() {
			continue
		}

		off := addr - p.base

		for _, f := range p.model.Fields {
			if f.Offset == off && (f.Type == t || f.deletionTime(t)) {
				return Ref{Model: p.model, Field: f}, true
			}
		}

		for _, r := range p.model.Relations {
			if r.Offset == off && r.Type == t {
				return Ref{Model: p.model, Relation: r}, true
			}
		}
	}

	return Ref{}, false
}

// deletionTime reports whether t is the time inside a DeletedAt field, so that
// &N.DeletedAt.V names the column for a condition on when a row was deleted.
func (f *Field) deletionTime(t reflect.Type) bool {
	return f.Type == deletedAtType && t == timeType
}

// OfType returns the metadata for a struct type.
func OfType(t reflect.Type) *Model { return ofType(t) }

func ofType(t reflect.Type) *Model {
	mu.RLock()

	m := models[t]

	mu.RUnlock()

	if m != nil {
		return m
	}

	m = build(t)

	mu.Lock()
	defer mu.Unlock()

	if existing := models[t]; existing != nil {
		return existing
	}

	models[t] = m

	return m
}

func build(t reflect.Type) *Model {
	m := &Model{
		Type:    t,
		Table:   &qb.TableRef{Name: TableName(t.Name())},
		columns: map[string]*Field{},
	}
	m.err = walk(m, t, 0, "", "")

	for _, f := range m.Fields {
		m.columns[f.Column] = f

		switch {
		case (f.Type == timeType || f.Type == unixTimeType) && f.Short == "CreatedAt":
			m.Created = f
		case (f.Type == timeType || f.Type == unixTimeType) && f.Short == "UpdatedAt":
			m.Updated = f
		case f.Type == deletedAtType:
			m.SoftDelete = f
		}

		if f.Version {
			m.Version = f
		}
	}

	for _, f := range m.Fields {
		if f.Name == "ID" || strings.HasSuffix(f.Name, ".ID") {
			m.SetKey(f)

			break
		}
	}

	if soft := m.SoftDelete; soft != nil {
		m.Filters = append(
			m.Filters,
			Filter{SoftDeleteFilter, func(context.Context) (qb.Cond, error) {
				return qb.IsNull{Left: m.Col(soft)}, nil
			}},
		)
	}

	return m
}

func walk(m *Model, t reflect.Type, offset uintptr, prefix, columnPrefix string) error {
	for sf := range t.Fields() {
		tag := parseTag(sf.Tag.Get("ant"))
		if !sf.IsExported() || tag.skip {
			continue
		}

		off := offset + sf.Offset

		name := prefix + sf.Name

		switch {
		case sf.Anonymous && sf.Type.Kind() == reflect.Struct && !IsValue(sf.Type):
			if err := walk(m, sf.Type, off, name+".", columnPrefix); err != nil {
				return err
			}

			continue
		case tag.embedded:
			if sf.Type.Kind() != reflect.Struct {
				return fmt.Errorf(
					"ant: embedded field %s.%s must be a struct value, got %s",
					m.Type.Name(),
					name,
					sf.Type,
				)
			}

			if err := walk(m, sf.Type, off, name+".", columnPrefix+tag.prefix); err != nil {
				return err
			}

			continue
		}

		if target, kind, ok := relation(sf.Type); ok {
			m.Relations = append(m.Relations, &Relation{
				Name:   name,
				Kind:   kind,
				Target: target,
				Type:   sf.Type,
				Offset: off,
				Owner:  m,
			})

			continue
		}

		column := tag.column
		if column == "" {
			column = ColumnName(sf.Name)
		}

		m.Fields = append(m.Fields, &Field{
			Name:      name,
			Short:     sf.Name,
			Column:    columnPrefix + column,
			Type:      sf.Type,
			Offset:    off,
			Named:     tag.column != "",
			Prefix:    columnPrefix,
			Unique:    tag.unique,
			Computed:  tag.computed,
			ReadOnly:  tag.readOnly,
			WriteOnly: tag.writeOnly,
			Default:   tag.defaulted,
			Immutable: tag.immutable,
			Token:     tag.token || tag.version,
			Version:   tag.version,
			Storage:   Storage{Default: tag.defaultSQL},
			pointer:   pointerTo(sf.Type),
		})
	}

	return nil
}

var (
	timeType      = reflect.TypeFor[time.Time]()
	deletedAtType = reflect.TypeFor[DeletedAt]()
	unixTimeType  = reflect.TypeFor[UnixTime]()
	scannerType   = reflect.TypeFor[sql.Scanner]()
	valuerType    = reflect.TypeFor[driver.Valuer]()
)

// relation reports whether t is a relation field: a slice of structs or a
// pointer to a struct. Value types such as time.Time are columns.
func relation(t reflect.Type) (reflect.Type, RelationKind, bool) {
	switch t.Kind() {
	case reflect.Slice:
		if isModel(t.Elem()) {
			return t.Elem(), Many, true
		}
	case reflect.Pointer:
		if isModel(t.Elem()) {
			return t.Elem(), One, true
		}
	}

	return nil, 0, false
}

func isModel(t reflect.Type) bool { return !IsValue(t) }

// IsValue reports whether t holds a single column's value rather than a row:
// anything that is not a struct, plus time.Time and types that convert
// themselves, such as sql.Null[T].
func IsValue(t reflect.Type) bool {
	return t.Kind() != reflect.Struct || t == timeType ||
		t.Implements(valuerType) || reflect.PointerTo(t).Implements(scannerType)
}

// IsInteger reports whether t is an integer type.
func IsInteger(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}

	return false
}

type tag struct {
	column     string
	prefix     string
	defaultSQL string
	unique     bool
	skip       bool
	computed   bool
	readOnly   bool
	writeOnly  bool
	defaulted  bool
	immutable  bool
	embedded   bool
	token      bool
	version    bool
}

// tagParts splits a tag on commas, except inside parentheses or quotes, so a
// default can be an expression such as coalesce(a, 0).
func tagParts(s string) []string {
	var parts []string

	depth, start := 0, 0
	quoted := false

	for i, r := range s {
		switch {
		case r == '\'':
			quoted = !quoted
		case quoted:
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}

	return append(parts, s[start:])
}

func parseTag(s string) tag {
	var out tag

	for _, part := range tagParts(s) {
		switch key, value, _ := strings.Cut(strings.TrimSpace(part), "="); key {
		case "-":
			out.skip = true
		case "unique":
			out.unique = true
		case "computed":
			out.computed = true
		case "readonly":
			out.readOnly = true
		case "writeonly":
			out.writeOnly = true
		case "default":
			out.defaulted = true
			out.defaultSQL = value
		case "immutable":
			out.immutable = true
		case "embedded":
			out.embedded = true
		case "version":
			out.version = true
		case "token":
			out.token = true
		case "prefix":
			out.prefix = value
		case "column":
			out.column = value
		}
	}

	return out
}

// ColumnName converts a Go field name to snake_case, keeping initialisms
// together: UserID becomes user_id.
func ColumnName(name string) string {
	var b strings.Builder

	runes := []rune(name)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prevLower := unicode.IsLower(runes[i-1])

			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || nextLower {
				b.WriteByte('_')
			}
		}

		b.WriteRune(unicode.ToLower(r))
	}

	return b.String()
}

// TableName converts a type name to its conventional table name.
func TableName(name string) string {
	return plural(ColumnName(name))
}

func plural(s string) string {
	switch {
	case strings.HasSuffix(s, "s"),
		strings.HasSuffix(s, "x"),
		strings.HasSuffix(s, "ch"),
		strings.HasSuffix(s, "sh"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsAny(s[len(s)-2:len(s)-1], "aeiou"):
		return s[:len(s)-1] + "ies"
	}

	return s + "s"
}
