package ant

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iter"
	"unsafe"

	"github.com/skudra-go/ant/internal/meta"
	"github.com/skudra-go/ant/internal/scan"
	"github.com/skudra-go/ant/qb"
)

// Selectable is a query that Project can read from.
type Selectable interface {
	base() *builder
}

func (q *Query[T]) base() *builder { return &q.builder }

// Projection is a query that returns values of R. Results are not tracked.
type Projection[R any] struct {
	builder
	columns []qb.Selected
	reader  func(*sql.Rows) (func() (R, error), error)
}

// Project selects only the mapped expressions, and scans them into R. Use it
// instead of Select to avoid fetching whole rows:
//
//	var S = ant.Fields[Summary]()
//
//	ant.Project[Summary](db.Posts.Join(&P.User),
//		ant.Into(&S.Author, &U.Name),
//		ant.Into(&S.Title, &P.Title))
func Project[R any](q Selectable, mappings ...Mapping) *Projection[R] {
	p := &Projection[R]{builder: q.base().cloned()}
	model := configure[R]()

	// R scans by its fields, so a struct ant could not read fails the projection
	// once, rather than each of its mappings.
	if err := model.Err(); err != nil {
		p.fail(err)

		mappings = nil
	}

	for _, m := range mappings {
		switch {
		case m.err != nil:
			p.fail(m.err)
		case !model.Has(m.dst):
			p.fail(fmt.Errorf("%w: Into needs a probe of %s", ErrWrongModel, model.Type.Name()))
		default:
			p.refs = append(p.refs, m.refs...)
			p.columns = append(p.columns, qb.Selected{Value: m.node, Alias: m.dst.Column})
		}
	}

	p.reader = func(rows *sql.Rows) (func() (R, error), error) {
		into, err := scan.For[R](rows)
		if err != nil {
			return nil, err
		}

		return func() (R, error) {
			var v R

			return v, into(&v)
		}, nil
	}

	return p
}

// projectRows reads every source of b into fresh values and passes them to f.
func projectRows[R any](b builder, f func([]unsafe.Pointer) R) *Projection[R] {
	p := &Projection[R]{builder: b.cloned(), columns: b.allColumns()}
	p.reader = func(rows *sql.Rows) (func() (R, error), error) {
		row := newRowReader(p.sources, rows)

		return func() (R, error) {
			var zero R

			values, err := row.read()
			if err != nil {
				return zero, err
			}

			return f(values), nil
		}, nil
	}

	return p
}

// rowReader scans a joined row into one value per source.
type rowReader struct {
	rows    *sql.Rows
	sources []source
	fields  [][]*meta.Field
	dest    []any
	values  []unsafe.Pointer
	seen    []bool
}

func newRowReader(sources []source, rows *sql.Rows) *rowReader {
	r := &rowReader{
		rows:    rows,
		sources: sources,
		fields:  make([][]*meta.Field, len(sources)),
		values:  make([]unsafe.Pointer, len(sources)),
		seen:    make([]bool, len(sources)),
	}

	columns := 0

	for i, s := range sources {
		r.fields[i] = s.model.Stored()
		columns += len(r.fields[i])
	}

	r.dest = make([]any, columns)

	return r
}

// nullableColumn scans into dst unless the column is NULL, and records whether
// any column of its source held a value.
type nullableColumn struct {
	dst  any
	seen *bool
}

// Scan implements sql.Scanner.
func (c *nullableColumn) Scan(src any) error {
	if src == nil {
		return nil
	}

	*c.seen = true

	return sql.ConvertAssign(driver.ScanContext{}, c.dst, src)
}

// read returns one pointer per source, nil for an unmatched outer join side.
func (r *rowReader) read() ([]unsafe.Pointer, error) {
	column := 0

	for i, s := range r.sources {
		r.values[i] = s.alloc()

		r.seen[i] = false

		for _, f := range r.fields[i] {
			dst := f.Addr(r.values[i])

			if s.nullable {
				dst = &nullableColumn{dst: dst, seen: &r.seen[i]}
			}

			r.dest[column] = dst
			column++
		}
	}

	if err := r.rows.Scan(r.dest...); err != nil {
		return nil, err
	}

	for i, s := range r.sources {
		if s.nullable && !r.seen[i] {
			r.values[i] = nil
		}
	}

	return r.values, nil
}

func (p *Projection[R]) open(ctx context.Context) (*sql.Rows, func() (R, error), error) {
	err := p.ready()
	if err == nil && len(p.columns) == 0 {
		err = ErrNoColumns
	}

	if err != nil {
		return nil, nil, err
	}

	if p.toMany > 1 {
		p.state.warnCartesian(ctx, p.toMany)
	}

	rows, err := p.state.query(ctx, p.statement(p.columns))
	if err != nil {
		return nil, nil, err
	}

	read, err := p.reader(rows)
	if err != nil {
		return nil, nil, errors.Join(err, rows.Close())
	}

	return rows, read, nil
}

// All returns every projected row, as an empty slice if there are none.
func (p *Projection[R]) All(ctx context.Context) (out []R, err error) {
	rows, read, err := p.open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	out = []R{}

	for rows.Next() {
		v, err := read()
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}

	return out, rows.Err()
}

// First returns the first projected row, or ErrNotFound.
func (p *Projection[R]) First(ctx context.Context) (*R, error) {
	n := *p
	n.limit = atMost(p.limit, 1)

	found, err := n.All(ctx)

	switch {
	case err != nil:
		return nil, err
	case len(found) == 0:
		return nil, ErrNotFound
	}

	return &found[0], nil
}

// Each streams projected rows, stopping after the first error.
func (p *Projection[R]) Each(ctx context.Context) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R

		rows, read, err := p.open(ctx)
		if err != nil {
			yield(zero, err)

			return
		}
		defer rows.Close()

		for rows.Next() {
			v, err := read()
			if err != nil {
				yield(zero, err)

				return
			}

			if !yield(v, nil) {
				return
			}
		}

		if err := rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}
