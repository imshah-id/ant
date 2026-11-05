package dialect_test

import (
	"database/sql"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/dialect/postgres"
	"github.com/skudra-go/ant/dialect/sqlite"
	"github.com/skudra-go/ant/dialect/sqlserver"
	"github.com/skudra-go/ant/qb"
)

// deleted is built on sql.Null[time.Time], as ant.DeletedAt is.
type deleted struct{ sql.Null[time.Time] }

// pointerDeleted has a pointer receiver, so a nil one must stay NULL.
type pointerDeleted struct{ at time.Time }

func (s *pointerDeleted) Value() (driver.Value, error) { return s.at, nil }

// looped embeds a pointer to itself, which database/sql accepts.
type looped struct{ *looped }

func (looped) Value() (driver.Value, error) { return "loop", nil }

// overridden is built on sql.NullTime but stores text, counting its calls.
type overridden struct {
	sql.NullTime
	calls *int
}

func (o overridden) Value() (driver.Value, error) {
	*o.calls++

	return "text", nil
}

// counted is a Valuer that is not a time, such as JSON or an encrypted value. Its
// Value must run once, in database/sql, not also to look for a time.
type counted struct{ calls *int }

func (c counted) Value() (driver.Value, error) {
	*c.calls++

	return "x", nil
}

// A database without zones gets every time in UTC, whatever holds it, from
// queries and from raw SQL alike. Other databases get the time as it was.
func TestTimesBindInUTC(t *testing.T) {
	east := time.FixedZone("UTC+3", 3*60*60)
	at := time.Date(2026, 1, 2, 15, 4, 5, 0, east)
	utc := at.UTC()

	var nilTime *time.Time

	var nilDeleted *pointerDeleted

	pointed := &at
	nullTime := &sql.NullTime{Time: at, Valid: true}

	var nilPointer *time.Time

	calls := 0
	other := counted{&calls}

	cases := []struct {
		name string
		in   any
		want any
	}{
		{"time.Time", at, utc},
		{"*time.Time", &at, utc},
		{"nil *time.Time", nilTime, nilTime},
		{"sql.Null", sql.Null[time.Time]{V: at, Valid: true}, utc},
		{"invalid sql.Null is NULL", sql.Null[time.Time]{V: at}, nil},
		{"sql.NullTime", sql.NullTime{Time: at, Valid: true}, utc},
		{"a type built on sql.Null", deleted{sql.Null[time.Time]{V: at, Valid: true}}, utc},
		{"a nil pointer Valuer", nilDeleted, nilDeleted},
		{"a secret keeps its cover", qb.Secret{V: at}, qb.Secret{V: utc}},
		{"a secret behind a pointer too", &qb.Secret{V: at}, &qb.Secret{V: utc}},
		{"another Valuer, untouched", other, other},
		{"**time.Time", &pointed, utc},
		{"**sql.NullTime", &nullTime, utc},
		{"a pointer to a nil *time.Time", &nilPointer, &nilPointer},
		{"**qb.Secret", ptrTo(&qb.Secret{V: at}), &qb.Secret{V: utc}},
		{"not a time", 42, 42},
	}

	t.Cleanup(func() {
		if calls != 0 {
			t.Errorf("another Valuer's Value ran %d times while binding", calls)
		}
	})

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			for _, d := range []dialect.Dialect{sqlite.Dialect, sqlserver.Dialect} {
				_, args, err := d.Render(&qb.Select{
					Columns: []qb.Selected{{Value: qb.Param{Value: tt.in}}},
				})
				if err != nil {
					t.Fatal(err)
				}

				if !same(args[0], tt.want) {
					t.Errorf("%s rendered %#v, want %#v", d.Name(), args[0], tt.want)
				}

				_, args, err = dialect.Bind(d, "SELECT ?", []any{tt.in})
				if err != nil {
					t.Fatal(err)
				}

				if !same(args[0], tt.want) {
					t.Errorf("%s bound %#v, want %#v", d.Name(), args[0], tt.want)
				}

				_, args, err = dialect.Bind(d, "SELECT @at", []any{sql.Named("at", tt.in)})
				if err != nil {
					t.Fatal(err)
				}

				if !same(args[0], tt.want) {
					t.Errorf("%s bound by name %#v, want %#v", d.Name(), args[0], tt.want)
				}
			}

			_, args, err := postgres.Dialect.Render(&qb.Select{
				Columns: []qb.Selected{{Value: qb.Param{Value: tt.in}}},
			})
			if err != nil {
				t.Fatal(err)
			}

			if !same(args[0], tt.in) {
				t.Errorf("postgres changed %#v to %#v", tt.in, args[0])
			}
		})
	}
}

// same compares bound values, times by instant and location.
func same(a, b any) bool {
	if x, ok := a.(time.Time); ok {
		y, ok := b.(time.Time)

		return ok && x.Equal(y) && x.Location() == y.Location()
	}

	if x, ok := a.(qb.Secret); ok {
		y, ok := b.(qb.Secret)

		return ok && same(x.V, y.V)
	}

	if x, ok := a.(*qb.Secret); ok {
		y, ok := b.(*qb.Secret)

		return ok && same(x.V, y.V)
	}

	return a == b
}

// A list in a raw IN expands to one value per element, and each is put in UTC.
func TestRawListsBindInUTC(t *testing.T) {
	east := time.FixedZone("UTC+3", 3*60*60)
	at := time.Date(2026, 1, 2, 15, 4, 5, 0, east)

	for _, args := range [][]any{
		{[]time.Time{at, at}},
		{sql.Named("when", []*time.Time{&at, &at})},
	} {
		query := "SELECT 1 WHERE t IN (?)"

		if _, ok := args[0].(sql.NamedArg); ok {
			query = "SELECT 1 WHERE t IN (@when)"
		}

		_, bound, err := dialect.Bind(sqlserver.Dialect, query, args)
		if err != nil {
			t.Fatal(err)
		}

		if len(bound) != 2 {
			t.Fatalf("bound %d values", len(bound))
		}

		for _, b := range bound {
			if v, ok := b.(time.Time); !ok || v.Location() != time.UTC {
				t.Errorf("an element bound as %#v", b)
			}
		}
	}
}

// A type that embeds itself does not hang binding, and a type built on a time
// that overrides Value is asked once, and its value goes on.
func TestOddValuers(t *testing.T) {
	calls := 0

	for _, d := range []dialect.Dialect{sqlite.Dialect, sqlserver.Dialect} {
		_, args, err := d.Render(&qb.Select{
			Columns: []qb.Selected{
				{Value: qb.Param{Value: looped{}}},
				{Value: qb.Param{Value: overridden{calls: &calls}}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		if _, ok := args[0].(looped); !ok {
			t.Errorf("%s: a looped Valuer became %#v", d.Name(), args[0])
		}

		if args[1] != "text" {
			t.Errorf("%s: an overridden Valuer became %#v", d.Name(), args[1])
		}
	}

	if calls != 2 {
		t.Errorf("Value ran %d times for two renders", calls)
	}
}

func ptrTo[V any](v V) *V { return &v }

// A Valuer built on a time, behind a pointer to a pointer, is asked once, and
// what it gives, an error too, goes on.
func TestPointedValuersAreAskedOnce(t *testing.T) {
	calls := 0
	inner := &overridden{calls: &calls}

	for _, d := range []dialect.Dialect{sqlite.Dialect, sqlserver.Dialect} {
		_, args, err := d.Render(
			&qb.Select{Columns: []qb.Selected{{Value: qb.Param{Value: &inner}}}},
		)
		if err != nil {
			t.Fatal(err)
		}

		if args[0] != "text" {
			t.Errorf("%s: bound %#v", d.Name(), args[0])
		}
	}

	if calls != 2 {
		t.Errorf("Value ran %d times for two renders", calls)
	}
}
