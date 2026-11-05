package ant

import (
	"context"
	"fmt"
	"iter"
	"reflect"
	"unsafe"

	"github.com/skudra-go/ant/internal/meta"
	"github.com/skudra-go/ant/internal/track"
	"github.com/skudra-go/ant/qb"
)

// Table is a typed entry point to one model. Declare Table fields on a struct
// that embeds Context, and Open populates them.
type Table[T any] struct {
	state *state
}

func (t *Table[T]) bind(s *state) error {
	t.state = s

	return configure[T]().Err()
}

func (t *Table[T]) query() *Query[T] { return newQuery[T](t.state) }

// WithDeleted starts a query that includes soft-deleted rows.
func (t *Table[T]) WithDeleted() *Query[T] { return t.query().WithDeleted() }

// OnlyDeleted starts a query for soft-deleted rows.
func (t *Table[T]) OnlyDeleted() *Query[T] { return t.query().OnlyDeleted() }

// IgnoreFilters starts a query with the named global filters off.
func (t *Table[T]) IgnoreFilters(
	names ...string,
) *Query[T] {
	return t.query().IgnoreFilters(names...)
}

// Scope starts a query from reusable logic. See Query.Scope.
func (t *Table[T]) Scope(scopes ...func(*Query[T]) *Query[T]) *Query[T] {
	return t.query().Scope(scopes...)
}

// Query starts a query with nothing added yet.
func (t *Table[T]) Query() *Query[T] { return t.query() }

// model returns T's metadata, or the error that stops T from being used.
func (t *Table[T]) model() (*meta.Model, error) {
	model := configure[T]()

	if t.state == nil {
		return nil, ErrNotOpen
	}

	return model, model.Err()
}

// keyed returns T's metadata and primary key.
func (t *Table[T]) keyed() (*meta.Model, *meta.Field, error) {
	model, err := t.model()
	if err != nil {
		return nil, nil, err
	}

	key := model.Key()
	if key == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoKey, model.Type.Name())
	}

	return model, key, nil
}

// checkKey requires K to be the key's type, or an integer for an integer key,
// so an untyped constant works with an int64 key.
func checkKey[K any](key *meta.Field) error {
	k := reflect.TypeFor[K]()
	if k == key.Type || k.Kind() == key.Type.Kind() && k.ConvertibleTo(key.Type) ||
		meta.IsInteger(k) && meta.IsInteger(key.Type) {
		return nil
	}

	return fmt.Errorf("%w: %s for %s", ErrKeyType, k, key.Type)
}

func keyEquals(model *meta.Model, key *meta.Field, v any) []qb.Cond {
	return []qb.Cond{qb.Compare{Left: model.Col(key), Op: qb.OpEq, Right: qb.Param{Value: v}}}
}

// Insert writes entities and the new entities related to them, parents first,
// in one transaction, so either all are saved or none are. Generated keys and
// foreign keys are filled in. A zero value in a generated key means the database
// assigns it, and every other field is written as it is, zero or not. A related
// entity that already has a generated key is linked, not inserted.
func (t *Table[T]) Insert(ctx context.Context, entities ...*T) error {
	return t.insert(ctx, entities, true)
}

// InsertOnly writes entities and ignores their relation fields.
func (t *Table[T]) InsertOnly(ctx context.Context, entities ...*T) error {
	return t.insert(ctx, entities, false)
}

func (t *Table[T]) insert(ctx context.Context, entities []*T, related bool) error {
	model, err := t.model()
	if err != nil {
		return err
	}

	ctx, release, err := t.state.claim(ctx, t.state.touchesTracker())
	if err != nil {
		return err
	}

	defer release()

	u := newUnit(t.state)

	for _, e := range entities {
		if e == nil {
			return ErrNilEntity
		}

		if err := u.insert(model, unsafe.Pointer(e), related); err != nil {
			return err
		}
	}

	return u.run(ctx)
}

// Find returns the entity with the given primary key, or ErrNotFound. A tracked
// entity is returned without a query, with any unsaved changes.
func (t *Table[T]) Find[K comparable](ctx context.Context, key K) (*T, error) {
	model, field, err := t.keyed()
	if err != nil {
		return nil, err
	}

	if err := checkKey[K](field); err != nil {
		return nil, err
	}

	ctx, release, err := t.state.claim(ctx, t.state.settings.tracking)
	if err != nil {
		return nil, err
	}

	defer release()

	if t.state.settings.tracking && len(model.Filters) == 0 {
		if v, ok := meta.KeyValue(key); ok {
			if e := t.state.tracker.Find(model, v); e != nil && e.State != track.Deleted {
				return (*T)(e.Entity), nil
			}
		}
	}

	return t.query().Find(ctx, key)
}

// FindKey returns the entity with a composite primary key, given its parts in
// key order, or ErrNotFound.
func (t *Table[T]) FindKey(ctx context.Context, parts ...any) (*T, error) {
	model, err := t.model()
	if err != nil {
		return nil, err
	}

	q := t.query()

	if len(model.Keys) == 0 || len(parts) != len(model.Keys) {
		return nil, fmt.Errorf(
			"%w: %d parts for %d key columns",
			ErrKeyType,
			len(parts),
			len(model.Keys),
		)
	}

	for i, f := range model.Keys {
		q.where = append(q.where, keyEquals(model, f, parts[i])...)
	}

	return q.First(ctx)
}

// DeleteByID removes the entity with the given primary key, and stops tracking
// it, or returns ErrNotFound.
func (t *Table[T]) DeleteByID[K comparable](ctx context.Context, key K) error {
	model, field, err := t.keyed()
	if err != nil {
		return err
	}

	if err := checkKey[K](field); err != nil {
		return err
	}

	ctx, release, err := t.state.claim(ctx, t.state.touchesTracker())
	if err != nil {
		return err
	}

	defer release()

	q := t.query()
	q.where = keyEquals(model, field, key)
	q.filtered = true

	n, err := q.Delete(ctx)
	if err != nil {
		return err
	}

	if n == 0 {
		return ErrNotFound
	}

	if v, ok := meta.KeyValue(key); ok {
		undo := t.state.tracker.ForgetRow(model, v)

		if t.state.tx != nil {
			t.state.onRollback(undo)
		}
	}

	return nil
}

// pruneBatch caps the rows one Prune statement removes.
const pruneBatch = 1000

// Prune removes rows that meet conds for good, soft-deleted or not, in batches, so
// that clearing out old rows does not hold one long transaction. It returns how
// many went, and stops at the first error.
func (t *Table[T]) Prune(ctx context.Context, conds ...Cond) (int64, error) {
	q := t.WithDeleted().Where(conds...).Limit(pruneBatch)

	var total int64

	for {
		n, err := q.ForceDelete(ctx)

		total += n

		if err != nil || n < pruneBatch {
			return total, err
		}
	}
}

// Reload overwrites the columns of e with the stored row, found by e's primary
// key, or returns ErrNotFound. Relations are left alone.
func (t *Table[T]) Reload(ctx context.Context, e *T) error {
	model, err := t.model()
	if err != nil {
		return err
	}

	if len(model.Keys) == 0 {
		return fmt.Errorf("%w: %s", ErrNoKey, model.Type.Name())
	}

	if e == nil {
		return ErrNilEntity
	}

	ctx, release, err := t.state.claim(ctx, t.state.touchesTracker())
	if err != nil {
		return err
	}

	defer release()

	q := t.query().WithDeleted()
	q.where = keyConditions(model, unsafe.Pointer(e))

	rows, err := t.state.query(ctx, q.statement(q.selection()))
	if err != nil {
		return err
	}

	entry := t.state.tracker.Get(unsafe.Pointer(e))

	// The row is read into a value of its own and copied over, so that a
	// Scanner that fills the map, slice, or pointee e holds leaves before as it
	// was.
	var read T
	if err := readInto(rows, &read); err != nil {
		return err
	}

	before := *e

	for _, f := range q.columns() {
		f.Value(unsafe.Pointer(e)).Set(f.Value(unsafe.Pointer(&read)))
	}

	if entry != nil {
		// In a transaction, a rollback takes the read back with the rows it
		// read: the values, and the state and snapshot.
		undo := t.state.tracker.Reloaded(entry, unsafe.Pointer(&before))

		if t.state.tx != nil {
			t.state.onRollback(undo)
		}
	}

	return nil
}

// Where starts a query with conditions.
func (t *Table[T]) Where(c ...Cond) *Query[T] { return t.query().Where(c...) }

// Unfiltered starts a query whose Update or Delete affects every row.
func (t *Table[T]) Unfiltered() *Query[T] { return t.query().Unfiltered() }

// OrderBy starts a query ordered by field, ascending.
func (t *Table[T]) OrderBy[V Ordered](field *V) *Query[T] { return t.query().OrderBy(field) }

// OrderByDesc starts a query ordered by field, descending.
func (t *Table[T]) OrderByDesc[V Ordered](
	field *V,
) *Query[T] {
	return t.query().OrderByDesc(field)
}

// Order starts a query ordered by terms from Asc and Desc.
func (t *Table[T]) Order(terms ...Sort) *Query[T] { return t.query().Order(terms...) }

// GroupByExpr starts a query grouped by an expression.
func (t *Table[T]) GroupByExpr[V any](e Expr[V]) *Grouped { return t.query().GroupByExpr(e) }

// Limit starts a query capped at n rows.
func (t *Table[T]) Limit(n int) *Query[T] { return t.query().Limit(n) }

// Offset starts a query that skips n rows.
func (t *Table[T]) Offset(n int) *Query[T] { return t.query().Offset(n) }

// Include starts a query that loads a one-to-many relation. See Query.Include.
func (t *Table[T]) Include[R any](rel *[]R, shape ...func(*Query[R]) *Query[R]) *Query[T] {
	return t.query().Include(rel, shape...)
}

// IncludeOne starts a query that loads a many-to-one or one-to-one relation.
func (t *Table[T]) IncludeOne[R any](rel **R, shape ...func(*Query[R]) *Query[R]) *Query[T] {
	return t.query().IncludeOne(rel, shape...)
}

// IncludeJoined starts a query that reads a relation in the same statement. See
// Query.IncludeJoined.
func (t *Table[T]) IncludeJoined[R any](rel **R) *Query[T] { return t.query().IncludeJoined(rel) }

// Join starts a query joined through a relation. See Query.Join.
func (t *Table[T]) Join[J any](rel **J) *Join2[T, J] { return t.query().Join(rel) }

// JoinMany starts a query joined through a one-to-many relation.
func (t *Table[T]) JoinMany[J any](rel *[]J) *Join2[T, J] { return t.query().JoinMany(rel) }

// LeftJoin starts a query left-joined through a relation.
func (t *Table[T]) LeftJoin[J any](rel **J) *Join2[T, J] { return t.query().LeftJoin(rel) }

// LeftJoinMany starts a query left-joined through a one-to-many relation.
func (t *Table[T]) LeftJoinMany[J any](rel *[]J) *Join2[T, J] { return t.query().LeftJoinMany(rel) }

// JoinOn starts a query joined on conditions.
func (t *Table[T]) JoinOn[J any](on ...Cond) *Join2[T, J] {
	return joinWith[T, J](t.query(), qb.InnerJoin, on)
}

// LeftJoinOn starts a query left-joined on conditions.
func (t *Table[T]) LeftJoinOn[J any](on ...Cond) *Join2[T, J] {
	return joinWith[T, J](t.query(), qb.LeftJoin, on)
}

// RightJoinOn starts a query right-joined on conditions.
func (t *Table[T]) RightJoinOn[J any](on ...Cond) *Join2[T, J] {
	return joinWith[T, J](t.query(), qb.RightJoin, on)
}

// FullJoinOn starts a query fully joined on conditions.
func (t *Table[T]) FullJoinOn[J any](on ...Cond) *Join2[T, J] {
	return joinWith[T, J](t.query(), qb.FullJoin, on)
}

// CrossJoin starts a query that pairs every row with every row of J.
func (t *Table[T]) CrossJoin[J any]() *Join2[T, J] {
	return joinWith[T, J](t.query(), qb.CrossJoin, nil)
}

// NoTracking starts a query whose results are not tracked.
func (t *Table[T]) NoTracking() *Query[T] { return t.query().NoTracking() }

// Count returns the number of rows.
func (t *Table[T]) Count(ctx context.Context) (int64, error) { return t.query().Count(ctx) }

// Exists reports whether the table has any rows.
func (t *Table[T]) Exists(ctx context.Context) (bool, error) { return t.query().Exists(ctx) }

// GroupBy starts a query grouped by a column. See Query.GroupBy.
func (t *Table[T]) GroupBy[V any](field *V) *Grouped { return t.query().GroupBy(field) }

// Distinct starts a query that removes duplicate rows.
func (t *Table[T]) Distinct() *Query[T] { return t.query().Distinct() }

// Column starts a query limited to the named column. See Query.Column.
func (t *Table[T]) Column[V any](field *V) *Query[T] { return t.query().Column(field) }

// Omit starts a query that leaves a column out. See Query.Omit.
func (t *Table[T]) Omit[V any](field *V) *Query[T] { return t.query().Omit(field) }

// With starts a query that fills a computed field. See Query.With.
func (t *Table[T]) With[V any](field *V, value Expr[V]) *Query[T] {
	return t.query().With(field, value)
}

// All returns every row.
func (t *Table[T]) All(ctx context.Context) ([]*T, error) { return t.query().All(ctx) }

// First returns the first row, or ErrNotFound.
func (t *Table[T]) First(ctx context.Context) (*T, error) { return t.query().First(ctx) }

// Single returns the only row. See Query.Single.
func (t *Table[T]) Single(ctx context.Context) (*T, error) { return t.query().Single(ctx) }

// Each streams every row.
func (t *Table[T]) Each(ctx context.Context) iter.Seq2[T, error] { return t.query().Each(ctx) }
