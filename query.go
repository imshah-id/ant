package ant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"
	"unsafe"

	"github.com/skudra-go/ant/internal/meta"
	"github.com/skudra-go/ant/qb"
)

// Query is a query for rows of T. Queries are immutable: every builder method
// returns a new query, so one can safely be the base of several others.
// Terminal methods run it.
type Query[T any] struct {
	builder
	noTracking bool
	debug      bool
	resolved   bool          // one instance per row, even with no tracking
	inSlice    bool          // results become copies in a relation, tracked there
	selected   []*meta.Field // chosen columns, or nil for all
	omitted    []*meta.Field
	computed   []qb.Selected
	includes   []include
	inJoin     []joinedInclude // relations read in the main statement
}

func newQuery[T any](s *state) *Query[T] {
	return &Query[T]{builder: builder{state: s, sources: []source{sourceOf[T]()}}}
}

// next returns a copy of q that shares no slices with it.
func (q *Query[T]) next() *Query[T] {
	c := *q
	c.builder = q.builder.cloned()
	c.selected = slices.Clone(q.selected)
	c.omitted = slices.Clone(q.omitted)
	c.computed = slices.Clone(q.computed)
	c.includes = slices.Clone(q.includes)
	c.inJoin = slices.Clone(q.inJoin)

	return &c
}

func (q *Query[T]) model() *meta.Model { return q.sources[0].model }

// Where adds conditions, all of which must match.
func (q *Query[T]) Where(c ...Cond) *Query[T] {
	n := q.next()
	n.addWhere(c)

	return n
}

// When applies f only if ok is true, for conditions decided at runtime.
func (q *Query[T]) When(ok bool, f func(*Query[T]) *Query[T]) *Query[T] {
	if !ok {
		return q
	}

	return f(q)
}

// Unfiltered allows Update and Delete to affect every row. Without it, they
// return ErrNoCondition unless a condition can exclude rows, which an empty
// ant.And or ant.NotIn cannot.
func (q *Query[T]) Unfiltered() *Query[T] {
	n := q.next()
	n.unfiltered = true

	return n
}

// OrderBy orders results by field, ascending, replacing any earlier ordering.
func (q *Query[T]) OrderBy[V Ordered](field *V) *Query[T] {
	return q.Order(Col(field).Asc())
}

// OrderByDesc orders results by field, descending, replacing any earlier
// ordering.
func (q *Query[T]) OrderByDesc[V Ordered](field *V) *Query[T] {
	return q.Order(Col(field).Desc())
}

// Order orders results by terms from Asc and Desc, replacing any earlier
// ordering.
func (q *Query[T]) Order(terms ...Sort) *Query[T] {
	n := q.next()
	n.order = nil
	n.addOrder(terms)

	return n
}

// ThenBy adds an ascending ordering after OrderBy.
func (q *Query[T]) ThenBy[V Ordered](field *V) *Query[T] { return then(q, Col(field).Asc()) }

// ThenByDesc adds a descending ordering after OrderBy.
func (q *Query[T]) ThenByDesc[V Ordered](field *V) *Query[T] { return then(q, Col(field).Desc()) }

func then[T any](q *Query[T], term Sort) *Query[T] {
	n := q.next()
	if len(n.order) == 0 {
		n.fail(ErrThenWithoutOrder)
	}

	n.addOrder([]Sort{term})

	return n
}

// Limit caps the number of rows returned.
func (q *Query[T]) Limit(n int) *Query[T] {
	c := q.next()
	c.limit = &n

	return c
}

// Offset skips the first n rows. Without an ordering, which rows are skipped
// is up to the database.
func (q *Query[T]) Offset(n int) *Query[T] {
	c := q.next()
	c.offset = &n

	return c
}

// Tag adds a comment to the SQL, so a query can be found in database logs.
func (q *Query[T]) Tag(name string) *Query[T] {
	n := q.next()
	n.comment = name

	return n
}

// Debug logs this query's SQL and values when it runs, at info level, through
// slog's default logger. It is for a moment of curiosity at a desk: unlike
// WithLogger, it shows values, so do not leave it in code.
func (q *Query[T]) Debug() *Query[T] {
	n := q.next()
	n.debug = true

	return n
}

// Hint adds an optimizer hint, written /*+ hint */ before the statement, in the
// form extensions such as pg_hint_plan read. A database that does not know it sees
// a comment. Prefer fixing the query, since a hint outlives the reason for it.
func (q *Query[T]) Hint(hint string) *Query[T] {
	n := q.next()
	n.hint = hint

	return n
}

// ForUpdate locks the selected rows until the transaction ends.
func (q *Query[T]) ForUpdate() *Query[T] { return q.locked(qb.Lock{Mode: "UPDATE"}) }

// ForShare locks the selected rows against changes until the transaction ends,
// while allowing other readers.
func (q *Query[T]) ForShare() *Query[T] { return q.locked(qb.Lock{Mode: "SHARE"}) }

// NoWait makes a lock fail at once instead of waiting for another transaction.
func (q *Query[T]) NoWait() *Query[T] {
	lock := q.currentLock()
	lock.NoWait = true

	return q.locked(lock)
}

// SkipLocked leaves out rows that another transaction has locked, which suits
// job queues.
func (q *Query[T]) SkipLocked() *Query[T] {
	lock := q.currentLock()
	lock.SkipLocked = true

	return q.locked(lock)
}

func (q *Query[T]) currentLock() qb.Lock {
	if q.lock != nil {
		return *q.lock
	}

	return qb.Lock{Mode: "UPDATE"}
}

func (q *Query[T]) locked(lock qb.Lock) *Query[T] {
	n := q.next()
	n.lock = &lock

	return n
}

// ClearOrder removes any ordering.
func (q *Query[T]) ClearOrder() *Query[T] {
	n := q.next()
	n.order = nil

	return n
}

// OrderByRandom shuffles the results.
func (q *Query[T]) OrderByRandom() *Query[T] {
	return q.Order(Expr[float64]{node: qb.Func{Name: "RANDOM"}}.Asc())
}

// Latest orders by a time column, newest first.
func (q *Query[T]) Latest(field *time.Time) *Query[T] { return q.OrderByDesc(field) }

// Oldest orders by a time column, oldest first.
func (q *Query[T]) Oldest(field *time.Time) *Query[T] { return q.OrderBy(field) }

// WithDeleted includes soft-deleted rows.
func (q *Query[T]) WithDeleted() *Query[T] { return q.IgnoreFilters(meta.SoftDeleteFilter) }

// OnlyDeleted returns only soft-deleted rows.
func (q *Query[T]) OnlyDeleted() *Query[T] {
	n := q.WithDeleted()
	if soft := n.model().SoftDelete; soft != nil {
		n.where = append(n.where, qb.IsNull{Left: n.model().Col(soft), Negate: true})
		n.filtered = true
	}

	return n
}

// IgnoreFilters switches off the named global filters for this query, or all of
// them with no names. Related queries from Include keep theirs.
func (q *Query[T]) IgnoreFilters(names ...string) *Query[T] {
	n := q.next()
	n.ignored = append(n.ignored, names...)
	n.ignoreAll = n.ignoreAll || len(names) == 0

	return n
}

// Scope applies reusable query logic:
//
//	func Adults(q *ant.Query[User]) *ant.Query[User] {
//		return q.Where(ant.Gte(&U.Age, 18))
//	}
//
//	db.Users.Query().Scope(Adults).All(ctx)
func (q *Query[T]) Scope(scopes ...func(*Query[T]) *Query[T]) *Query[T] {
	out := q

	for _, scope := range scopes {
		out = scope(out)
	}

	return out
}

// Distinct removes duplicate rows.
func (q *Query[T]) Distinct() *Query[T] {
	n := q.next()
	n.distinct = true

	return n
}

// NoTracking returns entities the change tracker does not see, including those
// from Include.
func (q *Query[T]) NoTracking() *Query[T] {
	n := q.next()
	n.noTracking = true

	return n
}

// NoTrackingResolved is NoTracking, except that one row is one instance in the
// results, as it is with tracking. It suits a read-only query that joins or
// includes the same row many times, and costs less than tracking it.
func (q *Query[T]) NoTrackingResolved() *Query[T] {
	n := q.NoTracking()
	n.resolved = true

	return n
}

// Column limits the query to the named columns, and can be repeated. Other
// fields keep their zero value, so the results are never tracked.
func (q *Query[T]) Column[V any](field *V) *Query[T] {
	n := q.next()

	f, model, err := fieldOf(field)
	if err != nil {
		n.fail(err)

		return n
	}

	n.refs = append(n.refs, model)
	n.selected = append(n.selected, f)

	return n
}

// Omit leaves a column out of the query. Its field keeps its zero value, so the
// results are never tracked.
func (q *Query[T]) Omit[V any](field *V) *Query[T] {
	n := q.next()

	f, model, err := fieldOf(field)
	if err != nil {
		n.fail(err)

		return n
	}

	n.refs = append(n.refs, model)
	n.omitted = append(n.omitted, f)

	return n
}

// With fills a field tagged ant:"computed" from an expression, such as a count
// of related rows.
func (q *Query[T]) With[V any](field *V, value Expr[V]) *Query[T] {
	n := q.next()

	f, model, err := fieldOf(field)

	switch {
	case err != nil:
		n.fail(err)
	case value.err != nil:
		n.fail(value.err)
	default:
		n.refs = append(append(n.refs, model), value.refs...)
		n.computed = append(n.computed, qb.Selected{Value: value.node, Alias: f.Column})
	}

	return n
}

// tracked reports whether results go to the change tracker. A partial row
// cannot be tracked, because its missing fields would look like real values, and
// a derived row may not match any stored row.
func (q *Query[T]) tracked() bool {
	return !q.noTracking && q.state.settings.tracking && q.from == nil &&
		len(q.selected) == 0 && len(q.omitted) == 0 && len(q.computed) == 0
}

func (q *Query[T]) columns() []*meta.Field {
	fields := q.selected
	if len(fields) == 0 {
		fields = q.model().Stored()
	}

	return slices.DeleteFunc(slices.Clone(fields), func(f *meta.Field) bool {
		return slices.Contains(q.omitted, f)
	})
}

func (q *Query[T]) selection() []qb.Selected {
	root := q.sources[0]
	fields := q.columns()

	out := make([]qb.Selected, 0, len(fields)+len(q.computed))

	for _, f := range fields {
		out = append(out, qb.Selected{Value: root.col(f)})
	}

	return append(out, q.computed...)
}

func (q *Query[T]) readyToRead() error {
	if err := q.ready(); err != nil {
		return err
	}

	if len(q.columns())+len(q.computed) == 0 {
		return ErrNoColumns
	}

	return nil
}

// fetch runs stmt and returns entities with their includes loaded.
func (q *Query[T]) fetch(ctx context.Context, stmt *qb.Select) ([]*T, error) {
	q.addJoined(stmt)

	if q.debug {
		q.state.debugStatement(ctx, stmt)
	}

	rows, err := q.state.query(ctx, stmt)
	if err != nil {
		return nil, err
	}

	if len(q.inJoin) == 0 && len(q.computed) == 0 {
		if err := q.state.checkColumns(rows, q.model()); err != nil {
			return nil, err
		}
	}

	var items []*T

	if len(q.inJoin) > 0 {
		items, err = q.collectJoined(rows)
	} else {
		items, err = collectEntities[T](rows)
	}

	if err != nil {
		return nil, err
	}

	if q.resolved {
		items = resolveIdentity(q.model(), items)
	}

	if _, hooked := any(new(T)).(AfterLoader); hooked {
		for _, item := range items {
			if err := any(item).(AfterLoader).AfterLoad(ctx); err != nil {
				return nil, err
			}
		}
	}

	if q.tracked() && !q.inSlice {
		model := q.model()

		for i, item := range items {
			items[i] = (*T)(q.state.tracker.Load(model, unsafe.Pointer(item)))
		}
	}

	if err := q.load(ctx, items); err != nil {
		return nil, err
	}

	if _, hooked := any(new(T)).(AfterIncluder); hooked && ctx.Value(skipHooks{}) == nil {
		for _, item := range items {
			if err := any(item).(AfterIncluder).AfterInclude(ctx); err != nil {
				return nil, err
			}
		}
	}

	return items, nil
}

func (q *Query[T]) load(ctx context.Context, items []*T) error {
	if len(q.includes) == 0 || len(items) == 0 {
		return nil
	}

	parents := make([]unsafe.Pointer, len(items))

	for i, item := range items {
		parents[i] = unsafe.Pointer(item)
	}

	for _, inc := range q.includes {
		if err := inc.load(ctx, parents, q.state != nil && q.tracked()); err != nil {
			return err
		}
	}

	return nil
}

// All returns every matching row, as an empty slice if there are none.
func (q *Query[T]) All(ctx context.Context) ([]*T, error) {
	if err := q.readyToRead(); err != nil {
		return nil, err
	}

	return q.fetch(ctx, q.statement(q.selection()))
}

// atMost returns the smaller of limit and n, treating nil as no limit.
func atMost(limit *int, n int) *int {
	if limit != nil && *limit < n {
		return limit
	}

	return &n
}

// First returns the first matching row, or ErrNotFound. Without an ordering,
// which row is first is up to the database.
func (q *Query[T]) First(ctx context.Context) (*T, error) {
	if err := q.readyToRead(); err != nil {
		return nil, err
	}

	stmt := q.statement(q.selection())
	stmt.Limit = atMost(q.limit, 1)

	found, err := q.fetch(ctx, stmt)

	switch {
	case err != nil:
		return nil, err
	case len(found) == 0:
		return nil, ErrNotFound
	}

	return found[0], nil
}

// Single returns the only matching row. It returns ErrNotFound if there are
// none, and ErrMultipleRows if there are several.
func (q *Query[T]) Single(ctx context.Context) (*T, error) {
	if err := q.readyToRead(); err != nil {
		return nil, err
	}

	stmt := q.statement(q.selection())
	stmt.Limit = atMost(q.limit, 2)

	found, err := q.fetch(ctx, stmt)

	switch {
	case err != nil:
		return nil, err
	case len(found) == 0:
		return nil, ErrNotFound
	case len(found) > 1:
		return nil, ErrMultipleRows
	}

	return found[0], nil
}

// ToSQL returns the SELECT this query would run. Global filters that need a
// context see an empty one.
func (q *Query[T]) ToSQL() (string, []any, error) { return q.ToSQLContext(context.Background()) }

// ToSQLContext is ToSQL with global filters resolved against ctx.
func (q *Query[T]) ToSQLContext(ctx context.Context) (string, []any, error) {
	if err := q.err; err != nil {
		return "", nil, err
	}

	if q.state == nil {
		return "", nil, ErrNotOpen
	}

	stmt := q.statement(q.selection())
	q.addJoined(stmt)

	return q.state.dialect.Render(qb.Contextual{Context: ctx, Statement: stmt})
}

// UpdateSQL returns the statement Update would run, for sending it some other
// way, such as in a pipelined batch.
func (q *Query[T]) UpdateSQL(ctx context.Context, set ...Assignment) (string, []any, error) {
	stmt, n, err := q.update(set)
	if err != nil {
		return "", nil, err
	}

	return n.state.dialect.Render(qb.Contextual{Context: ctx, Statement: stmt})
}

// DeleteSQL returns the statement Delete would run.
func (q *Query[T]) DeleteSQL(ctx context.Context) (string, []any, error) {
	stmt, err := q.delete(false)
	if err != nil {
		return "", nil, err
	}

	return q.state.dialect.Render(qb.Contextual{Context: ctx, Statement: stmt})
}

// Explain returns the database's plan for this query.
func (q *Query[T]) Explain(ctx context.Context) (string, error) {
	if err := q.readyToRead(); err != nil {
		return "", err
	}

	text, args, err := q.state.dialect.Render(
		qb.Contextual{Context: ctx, Statement: q.statement(q.selection())},
	)
	if err != nil {
		return "", err
	}

	rows, err := q.state.exec.QueryContext(ctx, "EXPLAIN "+text, args...)
	if err != nil {
		return "", err
	}

	lines, err := collect[string](rows)
	if err != nil {
		return "", err
	}

	return strings.Join(lines, "\n"), nil
}

// Find returns the entity with the given primary key, within this query's
// conditions, or ErrNotFound.
func (q *Query[T]) Find[K comparable](ctx context.Context, key K) (*T, error) {
	field := q.model().Key()
	if field == nil {
		return nil, ErrNoKey
	}

	if err := checkKey[K](field); err != nil {
		return nil, err
	}

	if err := q.ready(); err != nil {
		return nil, err
	}

	n := q.next()
	n.where = append(n.where, keyEquals(q.model(), field, key)...)

	return n.First(ctx)
}

// Each streams matching rows, stopping after the first error. Values are
// copies, so streamed rows are not tracked.
func (q *Query[T]) Each(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T

		if len(q.includes) > 0 {
			yield(zero, ErrIncludeInEach)

			return
		}

		if err := q.readyToRead(); err != nil {
			yield(zero, err)

			return
		}

		rows, err := q.state.query(ctx, q.statement(q.selection()))
		if err != nil {
			yield(zero, err)

			return
		}

		stream(rows, yield)
	}
}

// bulkTarget returns the conditions a bulk write uses, global filters included.
// SQL has no LIMIT on UPDATE and DELETE, so a limited query selects its rows by
// key. So does a query on a derived source, such as From, a set operation, or
// Recursive, because UPDATE and DELETE name only the table, and the rows the
// source picks would otherwise be lost.
func (q *Query[T]) bulkTarget() ([]qb.Cond, error) {
	if q.limit == nil && q.offset == nil && q.from == nil && len(q.with) == 0 {
		return q.conditions(), nil
	}

	model := q.model()

	key := model.Key()
	if key == nil {
		return nil, ErrNoKey
	}

	// A source without the key cannot say which rows it came from, and some
	// databases would resolve the key to the table being written instead.
	if q.from != nil && !q.fromKey {
		if q.from.Raw != nil {
			return nil, errors.New(
				"ant: Update and Delete cannot use FromRaw, whose columns ant cannot see",
			)
		}

		return nil, fmt.Errorf(
			"ant: Update and Delete on a derived source need it to select %s.%s",
			model.Type.Name(),
			key.Short,
		)
	}

	column := q.sources[0].col(key)
	sub := q.statement([]qb.Selected{{Value: column}})

	// Order matters only to paging, and SQL Server refuses it in a subquery
	// without OFFSET.
	if q.limit == nil && q.offset == nil {
		sub.Order = nil
	}

	return []qb.Cond{qb.In{Left: column, Sub: sub}}, nil
}

// keyIndex returns the position of the model's stored key in q's selection, or
// -1 if q's rows do not carry it, so that a bulk write on a source made from q
// can find the rows they came from. A set operation matches columns by
// position, so its sides must agree on it. A computed column under the key's
// name would make it ambiguous.
func (q *Query[T]) keyIndex() int {
	key := q.model().Key()

	switch {
	case key == nil, q.from != nil && !q.fromKey:
		return -1
	case slices.ContainsFunc(q.computed, func(c qb.Selected) bool { return c.Alias == key.Column }):
		return -1
	}

	return slices.Index(q.columns(), key)
}

// update returns the statement for Update.
func (q *Query[T]) update(a []Assignment) (*qb.Update, *Query[T], error) {
	n := q.next()

	if len(a) == 0 {
		n.fail(ErrNoAssignment)
	}

	set := make([]qb.Assignment, 0, len(a))

	for _, assignment := range a {
		if assignment.err != nil {
			n.fail(assignment.err)

			continue
		}

		n.refs = append(append(n.refs, assignment.model), assignment.refs...)
		set = append(set, assignment.node)
	}

	if err := n.writable(); err != nil {
		return nil, nil, err
	}

	where, err := n.bulkTarget()
	if err != nil {
		return nil, nil, err
	}

	model := n.model()
	if model.ReadOnly {
		return nil, nil, fmt.Errorf("%w: %s", ErrReadOnly, model.Type.Name())
	}

	if updated := model.Updated; updated != nil &&
		!slices.ContainsFunc(set, func(a qb.Assignment) bool {
			return a.Column == updated.Column
		}) {
		set = append(
			set,
			qb.Assignment{
				Column: updated.Column,
				Value: qb.Param{
					Value: timeValue(updated, n.state.settings.timestamp()).Interface(),
				},
			},
		)
	}

	if v := model.Version; v != nil {
		// Holders of a loaded copy must see this as a conflict.
		set = append(
			set,
			qb.Assignment{
				Column: v.Column,
				Value: qb.Binary{
					Left:  qb.Column{Name: v.Column},
					Op:    "+",
					Right: qb.Param{Value: 1},
				},
			},
		)
	}

	return &qb.Update{Table: model.Table, Set: set, Where: where}, n, nil
}

// Update applies assignments to every matching row and returns the number of
// rows changed. Zero values are written like any other value. With Limit or
// Offset, it changes only the rows the query selects.
func (q *Query[T]) Update(ctx context.Context, a ...Assignment) (int64, error) {
	stmt, n, err := q.update(a)
	if err != nil {
		return 0, err
	}

	return n.state.run(ctx, stmt)
}

// UpdateReturning is Update, returning the changed rows. They are not tracked.
func (q *Query[T]) UpdateReturning(ctx context.Context, a ...Assignment) ([]*T, error) {
	stmt, n, err := q.update(a)
	if err != nil {
		return nil, err
	}

	stmt.Returning = n.returning()

	return n.readRows(ctx, stmt)
}

// delete returns the statement for Delete: an UPDATE for a soft-deleted model.
func (q *Query[T]) delete(force bool) (qb.Statement, error) {
	if err := q.writable(); err != nil {
		return nil, err
	}

	model := q.model()
	if model.ReadOnly {
		return nil, fmt.Errorf("%w: %s", ErrReadOnly, model.Type.Name())
	}

	where, err := q.bulkTarget()
	if err != nil {
		return nil, err
	}

	if soft := model.SoftDelete; soft != nil && !force {
		deleted := meta.DeletedAt{
			Null: sql.Null[time.Time]{V: q.state.settings.timestamp(), Valid: true},
		}

		return &qb.Update{
			Table: model.Table,
			Where: where,
			Set:   []qb.Assignment{{Column: soft.Column, Value: qb.Param{Value: deleted}}},
		}, nil
	}

	return &qb.Delete{Table: model.Table, Where: where}, nil
}

// Delete removes every matching row and returns the number removed. A model with
// a DeletedAt field is soft-deleted. With Limit or Offset, it removes only the
// rows the query selects. Tracked instances of those rows stay tracked, so prefer
// Remove with SaveChanges for loaded entities.
func (q *Query[T]) Delete(ctx context.Context) (int64, error) {
	stmt, err := q.delete(false)
	if err != nil {
		return 0, err
	}

	return q.state.run(ctx, stmt)
}

// ForceDelete removes rows for real, even from a soft-deleted model. Use
// WithDeleted to reach rows that are already soft-deleted.
func (q *Query[T]) ForceDelete(ctx context.Context) (int64, error) {
	stmt, err := q.delete(true)
	if err != nil {
		return 0, err
	}

	return q.state.run(ctx, stmt)
}

// Restore undeletes matching soft-deleted rows and returns how many.
func (q *Query[T]) Restore(ctx context.Context) (int64, error) {
	soft := q.model().SoftDelete
	if soft == nil {
		return 0, ErrNotSoftDeleted
	}

	n := q.next()
	n.ignored = append(n.ignored, meta.SoftDeleteFilter)

	n.where = append(n.where, qb.IsNull{Left: q.model().Col(soft), Negate: true})

	if err := n.writable(); err != nil {
		return 0, err
	}

	where, err := n.bulkTarget()
	if err != nil {
		return 0, err
	}

	return n.state.run(ctx, &qb.Update{
		Table: n.model().Table,
		Where: where,
		Set:   []qb.Assignment{{Column: soft.Column, Value: qb.Param{Value: nil}}},
	})
}

// DeleteReturning is Delete, returning the removed rows.
func (q *Query[T]) DeleteReturning(ctx context.Context) ([]*T, error) {
	stmt, err := q.delete(false)
	if err != nil {
		return nil, err
	}

	switch s := stmt.(type) {
	case *qb.Delete:
		s.Returning = q.returning()
	case *qb.Update:
		s.Returning = q.returning()
	}

	return q.readRows(ctx, stmt)
}

func (q *Query[T]) returning() []qb.Selected {
	fields := q.model().Stored()

	out := make([]qb.Selected, len(fields))

	for i, f := range fields {
		out[i] = qb.Selected{Value: qb.Column{Name: f.Column}}
	}

	return out
}

func (q *Query[T]) readRows(ctx context.Context, stmt qb.Statement) ([]*T, error) {
	rows, err := q.state.query(ctx, stmt)
	if err != nil {
		return nil, err
	}

	return collectEntities[T](rows)
}

// writable returns every error that stops a bulk write from running.
func (b *builder) writable() error {
	err := b.ready()

	if !b.narrowed() && !b.unfiltered {
		err = errors.Join(err, ErrNoCondition)
	}

	return err
}

// From reads from sub instead of the table. Conditions, ordering, and paging
// apply to its results, and they are not tracked.
func (q *Query[T]) From(sub *Query[T]) *Query[T] {
	n := q.next()

	if err := sub.problems(); err != nil {
		n.fail(err)

		return n
	}

	n.from = &qb.Source{Sub: sub.statement(sub.selection()), Alias: n.model().Table.Name}
	n.fromKey = sub.keyIndex() >= 0
	n.fromFiltered = sub.narrowed()

	return n
}

// FromCTE names sub in a WITH clause, and reads from it.
func (q *Query[T]) FromCTE(name string, sub *Query[T]) *Query[T] {
	n := q.next()

	if err := sub.problems(); err != nil {
		n.fail(err)

		return n
	}

	n.with = append(n.with, qb.CTE{Name: name, Select: sub.statement(sub.selection())})
	n.from = &qb.Source{Name: name, Alias: n.model().Table.Name}
	n.fromKey = sub.keyIndex() >= 0
	n.fromFiltered = sub.narrowed()

	return n
}

// problems returns what stops q being used as a subquery.
func (q *Query[T]) problems() error {
	if q.err != nil {
		return q.err
	}

	if len(q.includes) > 0 {
		return ErrIncludeInSubquery
	}

	return nil
}

func (q *Query[T]) set(kind string, all bool, other *Query[T]) *Query[T] {
	n := q.next()

	if err := errors.Join(q.problems(), other.problems()); err != nil {
		n.fail(err)

		return n
	}

	combined := q.member()
	combined.Sets = append(combined.Sets, qb.SetOp{Kind: kind, All: all, Select: other.member()})

	out := newQuery[T](q.state)
	out.noTracking = q.noTracking
	out.from = &qb.Source{Sub: combined, Alias: q.model().Table.Name}
	out.fromKey = q.keyIndex() >= 0 && q.keyIndex() == other.keyIndex()
	out.fromFiltered = setFiltered(kind, q.narrowed(), other.narrowed())

	return out
}

// member returns q as one side of a set operation. Its ordering and paging are
// its own, but a SELECT's ORDER BY and LIMIT would apply to the whole set, and
// some databases refuse them on a member. So a paged member becomes a derived
// table, and an ordering with no paging, which cannot change which rows a set
// holds, is left out.
func (q *Query[T]) member() *qb.Select {
	stmt := q.statement(q.selection())

	if stmt.Limit == nil && stmt.Offset == nil {
		stmt.Order = nil

		return stmt
	}

	return &qb.Select{
		Columns: []qb.Selected{{Value: qb.Star{}}},
		From:    qb.Source{Sub: stmt, Alias: q.model().Table.Name},
	}
}

// setFiltered reports whether a set operation can leave rows out, so that a
// bulk write on it passes the ErrNoCondition guard. A union has every row
// unless both sides are filtered, and an intersection has none outside a
// filtered side. EXCEPT counts only its first side, because a second side that
// matches nothing would leave every row.
func setFiltered(kind string, left, right bool) bool {
	switch kind {
	case "UNION":
		return left && right
	case "INTERSECT":
		return left || right
	default:
		return left
	}
}

// Union returns the rows of q or other, without duplicates. Each side's own
// ordering and paging pick its rows first, and conditions, ordering, and paging
// added afterward apply to the combined rows.
func (q *Query[T]) Union(other *Query[T]) *Query[T] { return q.set("UNION", false, other) }

// UnionAll is Union, keeping duplicates.
func (q *Query[T]) UnionAll(other *Query[T]) *Query[T] { return q.set("UNION", true, other) }

// Intersect returns the rows in both q and other.
func (q *Query[T]) Intersect(other *Query[T]) *Query[T] { return q.set("INTERSECT", false, other) }

// Except returns the rows of q that are not in other.
func (q *Query[T]) Except(other *Query[T]) *Query[T] { return q.set("EXCEPT", false, other) }

// Recursive extends q with rows reached by repeatedly following a one-to-many
// relation of the same model, such as a folder and everything under it.
func (q *Query[T]) Recursive(rel *[]T) *Query[T] { return recursive(q, rel) }

// RecursiveOne extends q with rows reached by repeatedly following a many-to-one
// relation of the same model, such as a folder and all its ancestors.
func (q *Query[T]) RecursiveOne(rel **T) *Query[T] { return recursive(q, rel) }

// recursiveName is the WITH name of a recursive query.
const recursiveName = "ant_tree"

func recursive[T, V any](q *Query[T], rel *V) *Query[T] {
	out := newQuery[T](q.state)
	out.noTracking = q.noTracking

	r, link, err := linkOf[T](rel)
	if err != nil {
		out.fail(err)

		return out
	}

	if err := q.problems(); err != nil {
		out.fail(err)

		return out
	}

	model := q.model()
	if r.Owner != model {
		out.fail(ErrWrongModel)

		return out
	}

	step := &qb.Select{
		Columns: q.selection(),
		From:    qb.Source{Table: model.Table},
		Where:   q.filters(q.sources[0]),
		Joins: []qb.Join{{
			Source: qb.Source{Name: recursiveName},
			On: []qb.Cond{qb.Compare{
				Left:  model.Col(link.Foreign),
				Op:    qb.OpEq,
				Right: qb.Column{Alias: recursiveName, Name: link.Local.Column},
			}},
		}},
	}

	seed := q.member()
	seed.Sets = []qb.SetOp{{Kind: "UNION", Select: step}}
	out.with = []qb.CTE{{Name: recursiveName, Select: seed}}
	out.recursive = true
	out.from = &qb.Source{Name: recursiveName, Alias: model.Table.Name}
	out.fromKey = q.keyIndex() >= 0
	out.fromFiltered = q.narrowed()

	return out
}

// Join joins through a many-to-one or one-to-one relation. Unmatched rows are
// dropped.
func (q *Query[T]) Join[J any](
	rel **J,
) *Join2[T, J] {
	return joinThrough[T, J](q, qb.InnerJoin, rel)
}

// JoinMany joins through a one-to-many relation, with one result per related
// row.
func (q *Query[T]) JoinMany[J any](rel *[]J) *Join2[T, J] {
	return joinThrough[T, J](q, qb.InnerJoin, rel)
}

// LeftJoin joins through a many-to-one or one-to-one relation, keeping
// unmatched rows. Their joined value is nil in Select.
func (q *Query[T]) LeftJoin[J any](rel **J) *Join2[T, J] {
	return joinThrough[T, J](q, qb.LeftJoin, rel)
}

// LeftJoinMany joins through a one-to-many relation, keeping rows with no
// related row.
func (q *Query[T]) LeftJoinMany[J any](rel *[]J) *Join2[T, J] {
	return joinThrough[T, J](q, qb.LeftJoin, rel)
}

// JoinOn joins J on conditions rather than a relation.
func (q *Query[T]) JoinOn[J any](
	on ...Cond,
) *Join2[T, J] {
	return joinWith[T, J](q, qb.InnerJoin, on)
}

// LeftJoinOn joins J on conditions, keeping unmatched rows of T.
func (q *Query[T]) LeftJoinOn[J any](on ...Cond) *Join2[T, J] {
	return joinWith[T, J](q, qb.LeftJoin, on)
}

// JoinUsing joins J on a column with the same name in both tables.
func (q *Query[T]) JoinUsing[J, V any](field *V) *Join2[T, J] {
	n := q.next()

	f, _, err := fieldOf(field)
	if err != nil {
		n.fail(err)

		return &Join2[T, J]{builder: n.builder}
	}

	joinOn(&n.builder, qb.InnerJoin, sourceOf[J](), nil)
	n.joins[len(n.joins)-1].Using = []string{f.Column}

	return &Join2[T, J]{builder: n.builder}
}

// RightJoinOn joins J on conditions, keeping unmatched rows of J. Their T value
// is nil in Select.
func (q *Query[T]) RightJoinOn[J any](on ...Cond) *Join2[T, J] {
	return joinWith[T, J](q, qb.RightJoin, on)
}

// FullJoinOn joins J on conditions, keeping unmatched rows of both.
func (q *Query[T]) FullJoinOn[J any](on ...Cond) *Join2[T, J] {
	return joinWith[T, J](q, qb.FullJoin, on)
}

// CrossJoin pairs every row of T with every row of J.
func (q *Query[T]) CrossJoin[J any]() *Join2[T, J] { return joinWith[T, J](q, qb.CrossJoin, nil) }

// JoinSub joins the results of another query. Probes of J refer to its columns,
// so sub can filter, group, or limit first. Its global filters already applied.
func JoinSub[T, J any](q *Query[T], sub *Query[J], on ...Cond) *Join2[T, J] {
	return joinQuery(q, qb.InnerJoin, sub, false, on)
}

// LeftJoinSub is JoinSub, keeping rows of T with no match.
func LeftJoinSub[T, J any](q *Query[T], sub *Query[J], on ...Cond) *Join2[T, J] {
	return joinQuery(q, qb.LeftJoin, sub, false, on)
}

// JoinLateral joins a query that runs once per row of T, so its conditions can
// refer to T's columns. With Limit, this reads the top rows per parent, which a
// plain join cannot express:
//
//	ant.JoinLateral(db.Users.Query(), db.Posts.
//		Where(ant.EqCol(&P.UserID, &U.ID)).
//		OrderByDesc(&P.CreatedAt).Limit(3))
func JoinLateral[T, J any](q *Query[T], sub *Query[J], on ...Cond) *Join2[T, J] {
	return joinQuery(q, qb.InnerJoin, sub, true, on)
}

// LeftJoinLateral is JoinLateral, keeping rows of T with no match.
func LeftJoinLateral[T, J any](q *Query[T], sub *Query[J], on ...Cond) *Join2[T, J] {
	return joinQuery(q, qb.LeftJoin, sub, true, on)
}

func joinQuery[T, J any](
	q *Query[T],
	kind qb.JoinKind,
	sub *Query[J],
	lateral bool,
	conds []Cond,
) *Join2[T, J] {
	n := q.next()

	if err := sub.problems(); err != nil {
		n.fail(err)
		n.addSource(sourceOf[J](), kind)

		return &Join2[T, J]{builder: n.builder}
	}

	s := n.addSource(sourceOf[J](), kind)

	on := make([]qb.Cond, 0, len(conds))

	for _, c := range conds {
		if c.err != nil {
			n.fail(c.err)

			continue
		}

		n.refs = append(n.refs, c.refs...)
		on = append(on, c.node)
	}

	alias := s.alias
	if alias == "" {
		alias = s.model.Table.Name
	}

	n.refs = append(n.refs, sub.outerRefs()...)
	n.joined = append(n.joined, source{}) // the subquery applied its own filters

	n.joins = append(n.joins, qb.Join{
		Kind:    kind,
		Source:  qb.Source{Sub: sub.statement(sub.selection()), Alias: alias},
		On:      on,
		Lateral: lateral,
	})

	return &Join2[T, J]{builder: n.builder}
}

func joinThrough[T, J, V any](q *Query[T], kind qb.JoinKind, rel *V) *Join2[T, J] {
	n := q.next()

	r, link, err := linkOf[J](rel)
	if err != nil {
		n.fail(err)
		n.addSource(sourceOf[J](), kind)
	} else {
		joinRelation(&n.builder, kind, sourceOf[J](), r, link)
	}

	return &Join2[T, J]{builder: n.builder}
}

func joinWith[T, J any](q *Query[T], kind qb.JoinKind, on []Cond) *Join2[T, J] {
	n := q.next()
	joinOn(&n.builder, kind, sourceOf[J](), on)

	return &Join2[T, J]{builder: n.builder}
}
