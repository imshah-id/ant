package ant

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"reflect"

	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/internal/meta"
)

// RawQuery is a raw SQL statement. Its text reaches the database as written,
// so pass values as arguments, never by building the text from them.
type RawQuery[T any] struct {
	handle Handle
	query  string
	args   []any
	err    error
}

// Raw prepares a raw SQL statement. Pass arguments in one of three forms: the
// dialect's placeholders such as $1, sql.Named arguments written @name, or
// portable ? marks. A slice inside IN (@ids) or IN (?) expands to its elements.
// See dialect.Bind for the exact rules.
//
// T is what each row scans into. A struct takes columns by name, ignoring
// columns with no field, and need not be a model. A value type such as int
// takes a single column. T is unused by Exec, so ant.Raw[any] suits it.
func Raw[T any](h Handle, query string, args ...any) *RawQuery[T] {
	if t := reflect.TypeFor[T](); !meta.IsValue(t) {
		configure[T]()
	}

	return &RawQuery[T]{handle: h, query: query, args: args}
}

// RawFile is Raw with the statement read from a file in fsys, such as an
// embed.FS. A file that cannot be read is an error at the terminal call.
func RawFile[T any](h Handle, fsys fs.FS, name string, args ...any) *RawQuery[T] {
	text, err := fs.ReadFile(fsys, name)
	r := Raw[T](h, string(text), args...)

	if err != nil {
		r.err = fmt.Errorf("ant: read raw query: %w", err)
	}

	return r
}

// prepare returns the open context and the final statement text and arguments.
func (r *RawQuery[T]) prepare() (*state, string, []any, error) {
	if r.err != nil {
		return nil, "", nil, r.err
	}

	if r.handle == nil || r.handle.context().state == nil {
		return nil, "", nil, ErrNotOpen
	}

	s := r.handle.context().state
	text, args, err := dialect.Bind(s.dialect, r.query, r.args)

	return s, text, args, err
}

func (r *RawQuery[T]) rows(ctx context.Context) (*sql.Rows, error) {
	s, text, args, err := r.prepare()
	if err != nil {
		return nil, err
	}

	// A row scans into the fields ant read of T, so a struct it could not read,
	// as one with an embedded pointer, is an error, not rows with fields left
	// empty.
	if t := reflect.TypeFor[T](); !meta.IsValue(t) {
		if err := meta.OfType(t).Err(); err != nil {
			return nil, err
		}
	}

	rows, err := s.exec.QueryContext(ctx, text, args...)
	if err != nil {
		return nil, err
	}

	if t := reflect.TypeFor[T](); !meta.IsValue(t) {
		if err := s.checkColumns(rows, meta.OfType(t)); err != nil {
			return nil, err
		}
	}

	return rows, nil
}

// All runs the statement and returns every row, as an empty slice if there are
// none.
func (r *RawQuery[T]) All(ctx context.Context) ([]T, error) {
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, err
	}

	return collect[T](rows)
}

// First runs the statement and returns the first row, or ErrNotFound.
func (r *RawQuery[T]) First(ctx context.Context) (*T, error) {
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, err
	}

	return first[T](rows)
}

// Exec runs the statement and returns the number of rows affected.
func (r *RawQuery[T]) Exec(ctx context.Context) (int64, error) {
	s, text, args, err := r.prepare()
	if err != nil {
		return 0, err
	}

	result, err := s.exec.ExecContext(ctx, text, args...)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}
