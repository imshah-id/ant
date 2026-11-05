package ant

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"unsafe"

	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/internal/exec"
	"github.com/skudra-go/ant/internal/meta"
	"github.com/skudra-go/ant/internal/track"
	"github.com/skudra-go/ant/qb"
)

// FieldRef names a column of any type, from F.
type FieldRef struct {
	field *meta.Field
	model *meta.Model
	err   error
}

// F names a column, for APIs that take a list of columns of different types.
func F[V any](field *V) FieldRef {
	f, model, err := fieldOf(field)

	return FieldRef{f, model, err}
}

// Conflict says what to do when an insert hits a unique constraint. Start one
// with OnConflict.
type Conflict struct {
	columns []string
	update  []string
	set     []qb.Assignment
	nothing bool
	err     error
}

// OnConflict names the column of the unique constraint. Add more with And.
func OnConflict[V any](field *V) *Conflict { return (&Conflict{}).add(F(field)) }

func (c *Conflict) add(ref FieldRef) *Conflict {
	n := *c
	n.columns = slices.Clone(c.columns)

	if ref.err != nil {
		n.err = errors.Join(n.err, ref.err)

		return &n
	}

	n.columns = append(n.columns, ref.field.Column)

	return &n
}

// And adds a column for a constraint on several columns.
func (c *Conflict) And(ref FieldRef) *Conflict { return c.add(ref) }

// DoNothing keeps the existing row.
func (c *Conflict) DoNothing() *Conflict {
	n := *c
	n.nothing = true

	return &n
}

// DoUpdate overwrites only the named columns of the existing row. By default,
// every column except the key and the constraint's own is overwritten.
func (c *Conflict) DoUpdate(fields ...FieldRef) *Conflict {
	n := *c
	n.update = slices.Clone(c.update)

	for _, ref := range fields {
		if ref.err != nil {
			n.err = errors.Join(n.err, ref.err)

			continue
		}

		n.update = append(n.update, ref.field.Column)
	}

	return &n
}

// Set assigns values on conflict, as well as any named in DoUpdate.
func (c *Conflict) Set(assignments ...Assignment) *Conflict {
	n := *c
	n.set = slices.Clone(c.set)

	for _, a := range assignments {
		if a.err != nil {
			n.err = errors.Join(n.err, a.err)

			continue
		}

		n.set = append(n.set, a.node)
	}

	return &n
}

// clause returns the ON CONFLICT clause for a model. An update moves the version
// on, as every write does, so a copy loaded before it cannot save over it. The
// version is ant's to keep, so naming it is an error.
func (c *Conflict) clause(model *meta.Model) (*qb.Conflict, error) {
	out := &qb.Conflict{Columns: c.columns}

	if c.nothing {
		return out, nil
	}

	if err := managedVersion(model, c.update, c.set); err != nil {
		return nil, err
	}

	names := c.update
	if len(names) == 0 && len(c.set) == 0 {
		for _, f := range model.Updatable() {
			if !slices.Contains(c.columns, f.Column) && f != model.Version {
				names = append(names, f.Column)
			}
		}
	}

	for _, name := range names {
		out.Set = append(
			out.Set,
			qb.Assignment{Column: name, Value: qb.Column{Alias: "excluded", Name: name}},
		)
	}

	out.Set = append(out.Set, c.set...)

	if v := model.Version; v != nil {
		out.Set = append(out.Set, nextVersion(model))
	}

	return out, nil
}

// managedVersion refuses a write that names the version column, which ant moves
// on by itself.
func managedVersion(model *meta.Model, columns []string, set []qb.Assignment) error {
	v := model.Version
	if v == nil {
		return nil
	}

	if slices.Contains(columns, v.Column) ||
		slices.ContainsFunc(set, func(a qb.Assignment) bool { return a.Column == v.Column }) {
		return fmt.Errorf(
			"ant: %s.%s is a version, which ant moves on by itself; leave it out",
			model.Type.Name(),
			v.Name,
		)
	}

	return nil
}

// one is a literal 1, which takes no parameter: InsertMany fills a statement
// with as many rows as the parameter limit allows.
var one = qb.Fragment{SQL: "1"}

// nextVersion is the assignment that moves the stored version on by one.
func nextVersion(model *meta.Model) qb.Assignment {
	v := model.Version

	return qb.Assignment{
		Column: v.Column,
		Value: qb.Binary{
			Left:  qb.Column{Table: model.Table, Name: v.Column},
			Op:    "+",
			Right: one,
		},
	}
}

// insertStatement returns a one-row INSERT for e. Generated zero keys are left
// out for the database to fill in.
func insertStatement(model *meta.Model, e unsafe.Pointer) *qb.Insert {
	stmt := &qb.Insert{Table: model.Table}

	var row []qb.Expr

	for _, f := range model.Writable() {
		if f.FromDatabase() && f.Value(e).IsZero() {
			continue
		}

		stmt.Columns = append(stmt.Columns, f.Column)
		row = append(row, fieldParam(f, f.Value(e).Interface()))
	}

	if len(row) > 0 {
		stmt.Rows = [][]qb.Expr{row}
	}

	return stmt
}

func storedColumns(model *meta.Model) []qb.Selected {
	fields := model.Stored()

	out := make([]qb.Selected, len(fields))

	for i, f := range fields {
		out[i] = qb.Selected{Value: qb.Column{Name: f.Column}}
	}

	return out
}

// Upsert inserts e, or follows conflict if a unique constraint already holds the
// row. With an update, e receives the stored row, including its key. With
// DoNothing, it stays as it was and Upsert reports false.
func (t *Table[T]) Upsert(ctx context.Context, e *T, conflict *Conflict) (bool, error) {
	model, err := t.model()

	switch {
	case err != nil:
		return false, err
	case e == nil:
		return false, ErrNilEntity
	case conflict == nil:
		return false, errors.New("ant: Upsert needs a Conflict")
	case conflict.err != nil:
		return false, conflict.err
	}

	if model.ReadOnly {
		return false, fmt.Errorf("%w: %s", ErrReadOnly, model.Type.Name())
	}

	clause, err := conflict.clause(model)
	if err != nil {
		return false, err
	}

	// A copy that carries a version updates only the row it was loaded from.
	checked := t.versionCheck(model, unsafe.Pointer(e), clause)

	n := &node{model: model, entity: unsafe.Pointer(e)}
	newUnit(t.state).stamp(n)

	stmt := insertStatement(model, unsafe.Pointer(e))
	stmt.Conflict = clause

	rows, err := t.upsertRows(ctx, model, stmt, unsafe.Pointer(e))
	if err == nil {
		err = readInto(rows, e)
	}

	if err != nil {
		for _, f := range n.stamped {
			f.Value(n.entity).SetZero()
		}

		switch {
		case errors.Is(err, ErrNotFound) && checked:
			key, _ := model.KeyOf(unsafe.Pointer(e))

			return false, &ConcurrencyError{Model: model.Type.Name(), Key: key}
		case errors.Is(err, ErrNotFound):
			return false, nil
		}

		return false, err
	}

	if t.state.settings.tracking && model.Key() != nil {
		entry := t.state.tracker.Track(model, unsafe.Pointer(e), track.Unchanged)
		undo := entry.Saved(track.Unchanged)
		t.state.tracker.Rekey(entry)

		if t.state.tx != nil {
			t.state.tx.undo = append(t.state.tx.undo, undo)
		}
	}

	return true, nil
}

// versionCheck limits the conflict's update to the version e carries, if any, and
// reports whether it did. A zero version is a new entity, which has none to
// check. ON DUPLICATE KEY takes no condition, so there the update goes ahead.
func (t *Table[T]) versionCheck(model *meta.Model, e unsafe.Pointer, c *qb.Conflict) bool {
	v := model.Version

	switch {
	case v == nil, len(c.Set) == 0, v.Value(e).IsZero():
		return false
	case !dialect.Supports(t.state.dialect, dialect.ConflictWhere):
		return false
	}

	c.Where = append(c.Where, qb.Compare{
		Left:  qb.Column{Table: model.Table, Name: v.Column},
		Op:    qb.OpEq,
		Right: qb.Param{Value: v.Value(e).Interface()},
	})

	return true
}

// upsertRows runs an upsert and returns the stored row, or ErrNotFound if it did
// nothing. ON DUPLICATE KEY returns the old row even when it changed nothing, so
// there, as with no RETURNING at all, the row count decides, and the row is read
// back by the conflict's columns.
func (t *Table[T]) upsertRows(
	ctx context.Context,
	model *meta.Model,
	stmt *qb.Insert,
	e unsafe.Pointer,
) (*sql.Rows, error) {
	if t.state.dialect.SupportsReturning() && !dialect.SyntaxOf(t.state.dialect).DuplicateKey {
		stmt.Returning = storedColumns(model)

		return t.state.query(ctx, stmt)
	}

	if len(stmt.Conflict.Columns) == 0 {
		return nil, fmt.Errorf(
			"%w: an upsert with no conflict columns on %s",
			ErrUnsupported,
			t.state.dialect.Name(),
		)
	}

	changed, err := t.state.run(ctx, stmt)

	switch {
	case err != nil:
		return nil, err
	// No row changed: the conflict did nothing, or its condition, such as a
	// copy's version, turned the update down.
	case changed == 0 && (len(stmt.Conflict.Set) == 0 || len(stmt.Conflict.Where) > 0):
		return nil, ErrNotFound
	}

	read := &qb.Select{Columns: storedColumns(model), From: qb.Source{Table: model.Table}}

	for _, name := range stmt.Conflict.Columns {
		f := model.Column(name)
		read.Where = append(
			read.Where,
			qb.Compare{
				Left:  qb.Column{Name: name},
				Op:    qb.OpEq,
				Right: fieldParam(f, f.Value(e).Interface()),
			},
		)
	}

	return t.state.query(ctx, read)
}

// insertOptions configures InsertMany.
type insertOptions struct {
	batch    int
	conflict *Conflict
}

// InsertOption configures InsertMany.
type InsertOption func(*insertOptions)

// BatchSize sets the most rows in one statement. The default is 1000.
func BatchSize(n int) InsertOption { return func(o *insertOptions) { o.batch = n } }

// WithConflict applies a Conflict to every row of InsertMany.
func WithConflict(c *Conflict) InsertOption { return func(o *insertOptions) { o.conflict = c } }

// InsertMany writes many rows quickly, in multi-row statements inside one
// transaction, and returns how many were inserted. Unlike Insert, it ignores
// relations and does not track the rows. Generated keys are copied back, except
// with WithConflict, where rows may be skipped.
func (t *Table[T]) InsertMany(
	ctx context.Context,
	entities []*T,
	options ...InsertOption,
) (int64, error) {
	model, err := t.model()
	if err != nil {
		return 0, err
	}

	o := insertOptions{batch: 1000}

	for _, option := range options {
		option(&o)
	}

	if o.batch < 1 {
		return 0, fmt.Errorf("ant: batch size %d", o.batch)
	}

	if o.conflict != nil && o.conflict.err != nil {
		return 0, o.conflict.err
	}

	if slices.Contains(entities, nil) {
		return 0, ErrNilEntity
	}

	if len(entities) == 0 {
		return 0, nil
	}

	if model.ReadOnly {
		return 0, fmt.Errorf("%w: %s", ErrReadOnly, model.Type.Name())
	}

	if o.conflict != nil {
		if _, err := o.conflict.clause(model); err != nil {
			return 0, err
		}
	}

	stamper := newUnit(t.state)

	for _, e := range entities {
		stamper.stamp(&node{model: model, entity: unsafe.Pointer(e)})
	}

	var (
		total    int64
		assigned []*T
	)

	key := model.Key()

	err = t.state.transact(ctx, true, func(to exec.Executor) error {
		for _, group := range keyGroups(model, entities) {
			generated := key != nil && key.Generated && key.Value(unsafe.Pointer(group[0])).IsZero()
			columns := insertColumns(model, generated)

			size := min(
				o.batch,
				dialect.SyntaxOf(t.state.dialect).ParameterLimit()/max(len(columns), 1),
			)

			if generated && o.conflict == nil && !t.state.dialect.SupportsReturning() {
				size = 1 // LastInsertId reports one key
			}

			for batch := range slices.Chunk(group, size) {
				n, err := t.insertBatch(ctx, to, model, batch, columns, o.conflict, generated)
				if err != nil {
					return err
				}

				total += n

				if generated && o.conflict == nil {
					assigned = append(assigned, batch...)
				}
			}
		}

		return nil
	})
	if err != nil {
		for _, e := range assigned {
			key.Value(unsafe.Pointer(e)).SetZero()
		}

		return 0, err
	}

	return total, nil
}

// keyGroups splits entities into those that need a generated key and the rest,
// which need different column lists.
func keyGroups[T any](model *meta.Model, entities []*T) [][]*T {
	key := model.Key()
	if key == nil || !key.Generated {
		return [][]*T{entities}
	}

	var generated, supplied []*T

	for _, e := range entities {
		if key.Value(unsafe.Pointer(e)).IsZero() {
			generated = append(generated, e)
		} else {
			supplied = append(supplied, e)
		}
	}

	return slices.DeleteFunc([][]*T{generated, supplied}, func(g []*T) bool { return len(g) == 0 })
}

func insertColumns(model *meta.Model, generated bool) []*meta.Field {
	return slices.DeleteFunc(
		model.Writable(),
		func(f *meta.Field) bool { return generated && f.Generated },
	)
}

func (t *Table[T]) insertBatch(
	ctx context.Context,
	to exec.Executor,
	model *meta.Model,
	batch []*T,
	columns []*meta.Field,
	conflict *Conflict,
	generated bool,
) (int64, error) {
	stmt := &qb.Insert{Table: model.Table}

	for _, f := range columns {
		stmt.Columns = append(stmt.Columns, f.Column)
	}

	for _, e := range batch {
		row := make([]qb.Expr, len(columns))

		for i, f := range columns {
			if value := f.Value(unsafe.Pointer(e)); f.Default && value.IsZero() {
				row[i] = qb.Default{}
			} else {
				row[i] = fieldParam(f, value.Interface())
			}
		}

		stmt.Rows = append(stmt.Rows, row)
	}

	if conflict != nil {
		clause, err := conflict.clause(model)
		if err != nil {
			return 0, err
		}

		stmt.Conflict = clause

		return t.state.runOn(ctx, to, stmt)
	}

	if !generated {
		return t.state.runOn(ctx, to, stmt)
	}

	key := model.Key()

	if !t.state.dialect.SupportsReturning() {
		text, args, err := t.state.dialect.Render(stmt)
		if err != nil {
			return 0, err
		}

		result, err := to.ExecContext(ctx, text, args...)
		if err != nil {
			return 0, err
		}

		id, err := result.LastInsertId()
		if err != nil {
			return 0, err
		}

		return 1, assignKey(key, unsafe.Pointer(batch[0]), id)
	}

	stmt.Returning = []qb.Selected{{Value: qb.Column{Name: key.Column}}}

	text, args, err := t.state.dialect.Render(stmt)
	if err != nil {
		return 0, err
	}

	rows, err := to.QueryContext(ctx, text, args...)
	if err != nil {
		return 0, err
	}

	keys, err := collect[int64](rows)
	if err != nil {
		return 0, err
	}

	if len(keys) != len(batch) {
		return 0, fmt.Errorf("ant: insert returned %d keys for %d rows", len(keys), len(batch))
	}
	// One statement draws its sequence values in row order, while RETURNING
	// promises no order, so ascending keys match rows in order.
	slices.SortFunc(keys, func(a, b int64) int { return cmp.Compare(a, b) })

	for i, e := range batch {
		if err := sql.ConvertAssign(
			driver.ScanContext{},
			key.Addr(unsafe.Pointer(e)),
			keys[i],
		); err != nil {
			return 0, err
		}
	}

	return int64(len(batch)), nil
}

// InsertFrom inserts the rows of a projection, and returns how many. The
// projection's mappings name the columns:
//
//	db.Archive.InsertFrom(ctx, ant.Project[Archive](db.Posts.Where(old),
//		ant.Into(&A.Title, &P.Title)))
func (t *Table[T]) InsertFrom(ctx context.Context, rows *Projection[T]) (int64, error) {
	model, err := t.model()
	if err != nil {
		return 0, err
	}

	if err := rows.ready(); err != nil {
		return 0, err
	}

	if len(rows.columns) == 0 {
		return 0, ErrNoColumns
	}

	stmt := &qb.Insert{Table: model.Table, From: rows.statement(rows.columns)}

	for _, c := range rows.columns {
		stmt.Columns = append(stmt.Columns, c.Alias)
	}

	return t.state.run(ctx, stmt)
}

// FirstOrCreate returns the first row that meets conds, or inserts e and returns
// it. If another writer inserts a matching row first and a unique constraint
// stops this insert, it returns that row. The bool reports whether e was created.
func (t *Table[T]) FirstOrCreate(ctx context.Context, e *T, conds ...Cond) (*T, bool, error) {
	if e == nil {
		return nil, false, ErrNilEntity
	}

	q := t.query().Where(conds...)

	found, err := q.First(ctx)
	if err == nil {
		return found, false, nil
	}

	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}

	created := error(nil)

	if t.state.tx != nil {
		created = t.state.savepoint(ctx, func() error { return t.Insert(ctx, e) })
	} else {
		created = t.Insert(ctx, e)
	}

	if created == nil {
		return e, true, nil
	}

	if !IsUniqueViolation(created) {
		return nil, false, created
	}

	found, err = q.First(ctx)
	if err != nil {
		return nil, false, created
	}

	return found, false, nil
}

// FirstOrNew returns the first row that meets conds, or e, not saved. The bool
// reports whether a row was found.
func (t *Table[T]) FirstOrNew(ctx context.Context, e *T, conds ...Cond) (*T, bool, error) {
	found, err := t.query().Where(conds...).First(ctx)

	switch {
	case err == nil:
		return found, true, nil
	case errors.Is(err, ErrNotFound):
		return e, false, nil
	}

	return nil, false, err
}

// UpdateMany writes the named columns of each entity to its own row, in one
// transaction, and returns how many rows changed. Use it for values that differ
// per row. Tracked entities keep their snapshots, so prefer SaveChanges for them.
func (t *Table[T]) UpdateMany(
	ctx context.Context,
	entities []*T,
	fields ...FieldRef,
) (int64, error) {
	model, field, err := t.keyed()
	if err != nil {
		return 0, err
	}

	if len(fields) == 0 {
		return 0, ErrNoAssignment
	}

	for _, ref := range fields {
		switch {
		case ref.err != nil:
			return 0, ref.err
		case ref.model != model:
			return 0, ErrWrongModel
		}
	}

	if slices.Contains(entities, nil) {
		return 0, ErrNilEntity
	}

	var total int64

	err = t.state.transact(ctx, len(entities) > 1, func(to exec.Executor) error {
		for _, e := range entities {
			entity := unsafe.Pointer(e)
			stmt := &qb.Update{
				Table: model.Table,
				Where: keyEquals(model, field, field.Value(entity).Interface()),
			}

			for _, ref := range fields {
				stmt.Set = append(stmt.Set, qb.Assignment{
					Column: ref.field.Column,
					Value:  qb.Param{Value: ref.field.Value(entity).Interface()},
				})
			}

			n, err := t.state.runOn(ctx, to, stmt)
			if err != nil {
				return err
			}

			total += n
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	return total, nil
}

// Truncate removes every row, faster than Delete, and cannot be used on a table
// that others refer to unless they are empty too. It stops tracking nothing, so
// call ClearTracked if the context holds these rows.
func (t *Table[T]) Truncate(ctx context.Context) error {
	model, err := t.model()
	if err != nil {
		return err
	}

	name := t.state.dialect.Quote(model.Table.Name)

	if model.Table.Schema != "" {
		name = t.state.dialect.Quote(model.Table.Schema) + "." + name
	}

	_, err = t.state.exec.ExecContext(ctx, "TRUNCATE TABLE "+name)

	return err
}

// Clone returns a copy of entity with its key and relations cleared, ready to be
// inserted as a new row.
func Clone[T any](entity *T) *T {
	model := configure[T]()
	out := new(T)
	*out = *entity

	if key := model.Key(); key != nil && key.Generated {
		key.Value(unsafe.Pointer(out)).SetZero()
	}

	for _, r := range model.Relations {
		reflectZero(r, unsafe.Pointer(out))
	}

	return out
}
