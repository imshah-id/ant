package postgres

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/skudra-go/ant"
)

// JSONText reads a value inside a JSON column as text, following path. A
// missing value is NULL.
func JSONText[V any](field *V, path ...string) ant.Expr[string] {
	return ant.SQL[string]("? #>> ?::text[]", ant.Col(field), pathLiteral(path))
}

// pathLiteral writes a JSON path as an array literal, quoting each part, so a key
// holding a comma or brace stays one key.
func pathLiteral(path []string) string {
	quoted := make([]string, len(path))
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`)

	for i, part := range path {
		quoted[i] = `"` + escape.Replace(part) + `"`
	}

	return "{" + strings.Join(quoted, ",") + "}"
}

// JSONSet returns a jsonb column with the value at path replaced, or added if the
// path's parent exists, for updating part of a document in one statement:
//
//	ant.SetExpr(&U.Meta, postgres.JSONSet(&U.Meta, 2, "profile", "level"))
func JSONSet[V any](field *V, value any, path ...string) ant.Expr[V] {
	text, err := json.Marshal(value)
	if err != nil {
		return ant.FailedExpr[V](err)
	}

	return ant.SQL[V](
		"jsonb_set(?, ?::text[], ?::jsonb)",
		ant.Col(field),
		pathLiteral(path),
		string(text),
	)
}

// JSONContains matches rows whose jsonb column contains value.
func JSONContains[V any](field *V, value any) ant.Cond {
	text, err := json.Marshal(value)
	if err != nil {
		return ant.FailedCond(err)
	}

	return ant.SQLCond("? @> ?::jsonb", ant.Col(field), string(text))
}

// JSONHasKey matches rows whose jsonb column has key at the top level.
func JSONHasKey[V any](field *V, key string) ant.Cond {
	return ant.SQLCond("jsonb_exists(?, ?)", ant.Col(field), key)
}

// JSONLength returns the number of elements in a jsonb array column.
func JSONLength[V any](field *V) ant.Expr[int] {
	return ant.SQL[int]("jsonb_array_length(?)", ant.Col(field))
}

// Match matches rows whose text column meets a full-text search, in websearch
// syntax, using the given text search configuration, such as "english".
func Match[V ~string](field *V, config, query string) ant.Cond {
	return ant.SQLCond(
		"to_tsvector(?::regconfig, ?) @@ websearch_to_tsquery(?::regconfig, ?)",
		config,
		ant.Col(field),
		config,
		query,
	)
}

// Point is a location in PostgreSQL's built-in point type, which needs no
// extension. For geography, with projections and real distances, use PostGIS through
// .Type("geometry(Point,4326)") and ant.SQL.
type Point struct {
	X, Y float64
}

// ColumnType implements schema.Typed.
func (Point) ColumnType(string) string { return "point" }

// Value implements driver.Valuer.
func (p Point) Value() (driver.Value, error) { return fmt.Sprintf("(%g,%g)", p.X, p.Y), nil }

// Scan implements sql.Scanner.
func (p *Point) Scan(src any) error {
	var text string

	switch src := src.(type) {
	case nil:
		*p = Point{}

		return nil
	case string:
		text = src
	case []byte:
		text = string(src)
	default:
		return fmt.Errorf("postgres: cannot scan %T into a point", src)
	}

	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 5 || trimmed[0] != '(' || trimmed[len(trimmed)-1] != ')' {
		return fmt.Errorf("postgres: cannot scan %q into a point", text)
	}

	parts := strings.Split(trimmed[1:len(trimmed)-1], ",")
	if len(parts) != 2 {
		return fmt.Errorf("postgres: cannot scan %q into a point", text)
	}

	x, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return fmt.Errorf("postgres: cannot scan %q into a point: %w", text, err)
	}

	y, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return fmt.Errorf("postgres: cannot scan %q into a point: %w", text, err)
	}

	p.X = x
	p.Y = y

	return nil
}

// Distance returns how far a point column is from p, in the column's units.
func Distance(field *Point, p Point) ant.Expr[float64] {
	return ant.SQL[float64]("? <-> ?::point", ant.Col(field), fmt.Sprintf("(%g,%g)", p.X, p.Y))
}

// Near matches rows whose point column is within radius of p.
func Near(field *Point, p Point, radius float64) ant.Cond {
	return ant.SQLCond(
		"? <@ circle(?::point, ?::double precision)",
		ant.Col(field),
		fmt.Sprintf("(%g,%g)", p.X, p.Y),
		radius,
	)
}

// Vector is a column of the pgvector extension, for similarity search. Run CREATE
// EXTENSION vector in a migration first, and give the size with
// b.Column(&D.Embedding).Type("vector(1536)").
type Vector []float32

// ColumnType implements schema.Typed.
func (Vector) ColumnType(string) string { return "vector" }

// Value implements driver.Valuer, in pgvector's text format.
func (v Vector) Value() (driver.Value, error) {
	if v == nil {
		return nil, nil
	}

	return v.text(), nil
}

func (v Vector) text() string {
	var b strings.Builder
	b.WriteByte('[')

	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(strconv.FormatFloat(float64(x), 'f', -1, 32))
	}

	b.WriteByte(']')

	return b.String()
}

// Scan implements sql.Scanner.
func (v *Vector) Scan(src any) error {
	var text string

	switch src := src.(type) {
	case nil:
		*v = nil

		return nil
	case string:
		text = src
	case []byte:
		text = string(src)
	default:
		return fmt.Errorf("postgres: cannot scan %T into a vector", src)
	}

	text = strings.Trim(text, "[]")
	out := Vector{}

	if text != "" {
		for part := range strings.SplitSeq(text, ",") {
			x, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
			if err != nil {
				return err
			}

			out = append(out, float32(x))
		}
	}

	*v = out

	return nil
}

// CosineDistance returns the distance from a vector column to v, for ordering by
// similarity: Order(postgres.CosineDistance(&D.Embedding, query).Asc()).Limit(10).
func CosineDistance(field *Vector, v Vector) ant.Expr[float64] {
	return ant.SQL[float64]("? <=> ?::vector", ant.Col(field), v.text())
}

// L2Distance returns the Euclidean distance from a vector column to v.
func L2Distance(field *Vector, v Vector) ant.Expr[float64] {
	return ant.SQL[float64]("? <-> ?::vector", ant.Col(field), v.text())
}

// Array stores a slice in a PostgreSQL array column of text, numbers, or booleans.
type Array[V string | int | int32 | int64 | float64 | bool] []V

// ColumnType implements schema.Typed.
func (a Array[V]) ColumnType(string) string {
	switch any(*new(V)).(type) {
	case string:
		return "text[]"
	case int32:
		return "integer[]"
	case float64:
		return "double precision[]"
	case bool:
		return "boolean[]"
	}

	return "bigint[]"
}

// Value implements driver.Valuer, in PostgreSQL's array text format.
func (a Array[V]) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}

	var b strings.Builder
	b.WriteByte('{')

	for i, v := range a {
		if i > 0 {
			b.WriteByte(',')
		}

		if text, ok := any(v).(string); ok {
			b.WriteByte('"')
			b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text))
			b.WriteByte('"')
		} else {
			fmt.Fprint(&b, v)
		}
	}

	b.WriteByte('}')

	return b.String(), nil
}

// Scan implements sql.Scanner.
func (a *Array[V]) Scan(src any) error {
	var text string

	switch src := src.(type) {
	case nil:
		*a = nil

		return nil
	case string:
		text = src
	case []byte:
		text = string(src)
	default:
		return fmt.Errorf("postgres: cannot scan %T into an array", src)
	}

	items, err := parseArray(text)
	if err != nil {
		return err
	}

	out := make(Array[V], len(items))

	for i, item := range items {
		if err := parseElement(item, &out[i]); err != nil {
			return err
		}
	}

	*a = out

	return nil
}

func parseElement[V string | int | int32 | int64 | float64 | bool](text string, dst *V) error {
	var err error

	switch p := any(dst).(type) {
	case *string:
		*p = text
	case *int:
		*p, err = strconv.Atoi(text)
	case *int32:
		var n int64

		n, err = strconv.ParseInt(text, 10, 32)
		*p = int32(n)
	case *int64:
		*p, err = strconv.ParseInt(text, 10, 64)
	case *float64:
		*p, err = strconv.ParseFloat(text, 64)
	case *bool:
		*p = text == "t" || text == "true"
	}

	return err
}

// parseArray splits a one-dimensional array literal into its elements.
func parseArray(text string) ([]string, error) {
	if len(text) < 2 || text[0] != '{' || text[len(text)-1] != '}' {
		return nil, fmt.Errorf("postgres: bad array %q", text)
	}

	body := text[1 : len(text)-1]
	if body == "" {
		return []string{}, nil
	}

	var (
		out     []string
		current strings.Builder
		quoted  bool
	)

	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\\' && quoted && i+1 < len(body):
			i++
			current.WriteByte(body[i])
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, current.String())
			current.Reset()
		case c == '{' && !quoted:
			return nil, fmt.Errorf("postgres: nested arrays are not supported: %q", text)
		default:
			current.WriteByte(c)
		}
	}

	return append(out, current.String()), nil
}
