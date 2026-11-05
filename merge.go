package ant

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"unsafe"

	"github.com/skudra-go/ant/internal/meta"
	"github.com/skudra-go/ant/qb"
)

// Merger builds a MERGE. Start one with Merge.
type Merger[T any] struct {
	table *Table[T]
	stmt  qb.Merge
	err   error
}

// Merge combines source rows into T's table, deciding per row what to do, which
// Upsert cannot: one statement can update, insert, and delete. Clauses run in the
// order added, and the first that fits a row wins. PostgreSQL has it from version
// 15.
//
//	ant.Merge(&db.Stock, db.Staged.Query(), ant.EqCol(&Stock.SKU, &Staged.SKU)).
//		WhenMatchedIf(ant.Eq(&Staged.Count, 0)).Delete().
//		WhenMatched(ant.SetExpr(&Stock.Count, ant.Col(&Staged.Count))).
//		WhenNotMatched(ant.Into(&Stock.SKU, &Staged.SKU), ant.Into(&Stock.Count, &Staged.Count)).
//		Exec(ctx)
func Merge[T, S any](t *Table[T], source *Query[S], on ...Cond) *Merger[T] {
	m := &Merger[T]{table: t}

	model, err := t.model()
	if err != nil {
		m.err = err

		return m
	}

	if err := source.problems(); err != nil {
		m.err = err

		return m
	}

	if source.model() == model {
		m.err = fmt.Errorf("ant: Merge needs a source other than %s", model.Type.Name())

		return m
	}

	m.stmt.Table = model.Table

	m.stmt.Using = qb.Source{
		Sub:   source.statement(source.selection()),
		Alias: source.model().Table.Name,
	}

	merged := And(on...)
	if merged.err != nil {
		m.err = merged.err

		return m
	}

	m.stmt.On = []qb.Cond{merged.node}

	return m
}

func (m *Merger[T]) add(a qb.MergeAction) *Merger[T] {
	n := *m
	n.stmt.Actions = append(append([]qb.MergeAction(nil), m.stmt.Actions...), a)

	return &n
}

func assignments(set []Assignment) ([]qb.Assignment, error) {
	var err error

	out := make([]qb.Assignment, 0, len(set))

	for _, a := range set {
		err = errors.Join(err, a.err)
		out = append(out, a.node)
	}

	return out, err
}

// WhenMatched updates target rows that have a source row.
func (m *Merger[T]) WhenMatched(set ...Assignment) *Merger[T] {
	return m.matched(nil, set)
}

// WhenMatchedIf updates matched rows that also meet a condition.
func (m *Merger[T]) WhenMatchedIf(c Cond, set ...Assignment) *Merger[T] {
	return m.matched(&c, set)
}

func (m *Merger[T]) matched(c *Cond, set []Assignment) *Merger[T] {
	nodes, err := assignments(set)
	action := qb.MergeAction{Matched: true, Set: nodes}

	if c != nil {
		err = errors.Join(err, c.err)
		action.If = []qb.Cond{c.node}
	}

	n := m.add(action)
	n.err = errors.Join(n.err, err)

	return n
}

// Delete turns the last clause, which must have no assignments, into a delete.
func (m *Merger[T]) Delete() *Merger[T] {
	n := m.add(qb.MergeAction{})
	last := len(n.stmt.Actions) - 1

	n.stmt.Actions = n.stmt.Actions[:last]

	if last == 0 || !n.stmt.Actions[last-1].Matched || len(n.stmt.Actions[last-1].Set) > 0 {
		n.err = errors.Join(
			n.err,
			errors.New("ant: Delete follows WhenMatched with no assignments"),
		)

		return n
	}

	n.stmt.Actions[last-1].Delete = true

	return n
}

// WhenNotMatched inserts source rows with no target row. Each mapping names a
// target column and the source value for it.
func (m *Merger[T]) WhenNotMatched(insert ...Mapping) *Merger[T] {
	action := qb.MergeAction{}

	var err error

	for _, mapping := range insert {
		if mapping.err != nil {
			err = errors.Join(err, mapping.err)

			continue
		}

		action.Columns = append(action.Columns, mapping.dst.Column)
		action.Values = append(action.Values, mapping.node)
	}

	n := m.add(action)
	n.err = errors.Join(n.err, err)

	return n
}

// Exec runs the statement and returns the number of rows changed.
func (m *Merger[T]) Exec(ctx context.Context) (int64, error) {
	if m.err != nil {
		return 0, m.err
	}

	if len(m.stmt.Actions) == 0 {
		return 0, errors.New("ant: Merge has no WHEN clause")
	}

	stmt := m.stmt

	model, err := m.table.model()
	if err != nil {
		return 0, err
	}

	if stmt.Actions, err = versioned(model, stmt.Actions); err != nil {
		return 0, err
	}

	return m.table.state.run(ctx, &stmt)
}

// versioned keeps a model's version as other writes do: an update moves it on,
// so a copy loaded before cannot save over it, and an insert starts it at 1
// unless the source gives one.
func versioned(model *meta.Model, actions []qb.MergeAction) ([]qb.MergeAction, error) {
	v := model.Version
	if v == nil {
		return actions, nil
	}

	out := slices.Clone(actions)

	for i, a := range out {
		switch {
		case a.Matched && len(a.Set) > 0:
			if err := managedVersion(model, nil, a.Set); err != nil {
				return nil, err
			}

			a.Set = append(slices.Clone(a.Set), nextVersion(model))
		case !a.Matched && len(a.Columns) > 0 && !slices.Contains(a.Columns, v.Column):
			a.Columns = append(slices.Clone(a.Columns), v.Column)
			a.Values = append(slices.Clone(a.Values), one)
		}

		out[i] = a
	}

	return out, nil
}

// BulkRows is entities prepared for a bulk loader outside ant, such as COPY.
type BulkRows struct {
	Schema  string
	Table   string
	Columns []string
	Rows    [][]any
}

// PrepareBulk fills what Insert fills in Go, such as timestamps and generated
// keys, and returns the values to load. Keys from the database are left to it, and
// do not come back. Hooks do not run.
func PrepareBulk[T any](h Handle, entities []*T) (*BulkRows, error) {
	s, model, err := handleModel[T](h)
	if err != nil {
		return nil, err
	}

	if len(entities) == 0 {
		return nil, ErrNoRows
	}

	if model.ReadOnly {
		return nil, fmt.Errorf("%w: %s", ErrReadOnly, model.Type.Name())
	}

	u := newUnit(s)

	nodes := make([]*node, len(entities))

	for i, e := range entities {
		if e == nil {
			return nil, ErrNilEntity
		}

		nodes[i] = &node{model: model, entity: unsafe.Pointer(e)}
		u.stamp(nodes[i])
	}

	var fields []*meta.Field

	for _, f := range model.Writable() {
		if !f.FromDatabase() {
			fields = append(fields, f)

			continue
		}

		zero := 0

		for _, n := range nodes {
			if f.Value(n.entity).IsZero() {
				zero++
			}
		}

		switch zero {
		case len(nodes):
		case 0:
			fields = append(fields, f)
		default:
			return nil, fmt.Errorf("ant: bulk load needs %s set on every row or none", f.Name)
		}
	}

	out := &BulkRows{Schema: model.Table.Schema, Table: model.Table.Name}

	for _, f := range fields {
		out.Columns = append(out.Columns, f.Column)
	}

	for _, n := range nodes {
		row := make([]any, len(fields))

		for i, f := range fields {
			value := f.Value(n.entity).Interface()
			if valuer, ok := value.(driver.Valuer); ok {
				if value, err = valuer.Value(); err != nil {
					return nil, err
				}
			}

			row[i] = value
		}

		out.Rows = append(out.Rows, row)
	}

	return out, nil
}
