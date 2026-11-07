package dialect_test

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
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

// pointedNull is built on sql.Null of a *time.Time.
type pointedNull struct{ sql.Null[*time.Time] }

// twice reaches one time through two embedded fields, and gives it through the
// sql.Null, whose Value it gets.
type twice struct {
	*time.Time
	sql.Null[any]
}

// hiddenNull is built on an sql.Null of an interface under an unexported name,
// which reflect may read but not hand out.
type (
	nullAny    = sql.Null[any]
	hiddenNull struct{ nullAny }
)

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

	var nilNumber *int

	// A pointer may reach the time through an interface, and go on after it.
	var heldTime, heldNull, heldNothing, heldPointer any = at, sql.NullTime{Time: at, Valid: true}, nil, &pointed

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
		{"sql.Null of a *time.Time", sql.Null[*time.Time]{V: &at, Valid: true}, utc},
		{"sql.Null of a **time.Time", sql.Null[**time.Time]{V: &pointed, Valid: true}, utc},
		{"invalid sql.Null of a *time.Time is NULL", sql.Null[*time.Time]{V: &at}, nil},
		{"a pointer to sql.Null of a *time.Time", &sql.Null[*time.Time]{V: &at, Valid: true}, utc},
		{"sql.Null of an interface holding a time", sql.Null[any]{V: at, Valid: true}, utc},
		{"sql.Null of an interface holding a *time.Time", sql.Null[any]{V: &at, Valid: true}, utc},
		{
			"sql.Null of an interface holding sql.NullTime",
			sql.Null[any]{V: sql.NullTime{Time: at, Valid: true}, Valid: true},
			utc,
		},
		{
			"sql.Null of an interface holding no time",
			sql.Null[any]{V: 42, Valid: true},
			sql.Null[any]{V: 42, Valid: true},
		},
		{
			"sql.Null of an interface holding another Valuer",
			sql.Null[any]{V: other, Valid: true},
			sql.Null[any]{V: other, Valid: true},
		},
		{"invalid sql.Null of an interface", sql.Null[any]{V: at}, sql.Null[any]{V: at}},
		{
			"a time two embedded fields point to",
			twice{&at, sql.Null[any]{V: &at, Valid: true}},
			utc,
		},
		{
			"an sql.Null of an interface holding another",
			sql.Null[any]{V: sql.Null[any]{V: at, Valid: true}, Valid: true},
			utc,
		},
		{
			"sql.Null of sql.NullTime",
			sql.Null[sql.NullTime]{V: sql.NullTime{Time: at, Valid: true}, Valid: true},
			utc,
		},
		{
			"sql.Null of a *sql.NullTime",
			sql.Null[*sql.NullTime]{V: &sql.NullTime{Time: at, Valid: true}, Valid: true},
			utc,
		},
		{
			"sql.Null of a type built on sql.Null",
			sql.Null[deleted]{V: deleted{sql.Null[time.Time]{V: at, Valid: true}}, Valid: true},
			utc,
		},
		{
			"sql.Null of another Valuer",
			sql.Null[counted]{V: other, Valid: true},
			sql.Null[counted]{V: other, Valid: true},
		},
		{
			"a type built on sql.Null of a *time.Time",
			pointedNull{sql.Null[*time.Time]{V: &at, Valid: true}},
			utc,
		},
		{
			"a type built on sql.Null of an interface, unexported",
			hiddenNull{nullAny{V: at, Valid: true}},
			utc,
		},
		{"a type built on sql.Null", deleted{sql.Null[time.Time]{V: at, Valid: true}}, utc},
		{"a nil pointer Valuer", nilDeleted, nilDeleted},
		{"a secret keeps its cover", qb.Secret{V: at}, qb.Secret{V: utc}},
		{"a secret behind a pointer too", &qb.Secret{V: at}, &qb.Secret{V: utc}},
		{"another Valuer, untouched", other, other},
		{"**time.Time", &pointed, utc},
		{"**sql.NullTime", &nullTime, utc},
		{"a pointer to a nil *time.Time", &nilPointer, &nilPointer},
		{"**qb.Secret", ptrTo(&qb.Secret{V: at}), &qb.Secret{V: utc}},
		{"a pointer to a nil *int", &nilNumber, &nilNumber},
		{"a pointer to an interface holding a time", &heldTime, utc},
		{"a pointer to an interface holding sql.NullTime", &heldNull, utc},
		{"a pointer to an empty interface", &heldNothing, &heldNothing},
		{"a pointer to an interface holding a **time.Time", &heldPointer, utc},
		{"nil *int", nilNumber, nilNumber},
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

var errRefused = errors.New("refused")

// refused is built on sql.NullTime but fails, counting its calls.
type refused struct {
	sql.NullTime
	calls *int
}

func (r refused) Value() (driver.Value, error) {
	*r.calls++

	return nil, errRefused
}

// A Valuer built on a time, behind any number of pointers or an interface, is
// asked once on its way to the driver, and what it gives, an error too, goes on.
func TestPointedValuersAreAskedOnce(t *testing.T) {
	calls := 0
	text := &overridden{calls: &calls}
	failing := &refused{calls: &calls}

	var textHeld, textPointerHeld, failingHeld any = *text, text, *failing

	// The interface may hold more pointers on the way to the Valuer.
	var textChain, failingChain any = &text, &failing

	cases := []struct {
		in   any
		want any
		err  error
	}{
		{&text, "text", nil},
		{ptrTo(&text), "text", nil},
		{&failing, nil, errRefused},
		{ptrTo(&failing), nil, errRefused},
		{&textHeld, "text", nil},
		{ptrTo(&textHeld), "text", nil},
		{&textPointerHeld, "text", nil},
		{&failingHeld, nil, errRefused},
		{&textChain, "text", nil},
		{ptrTo(&textChain), "text", nil},
		{&failingChain, nil, errRefused},
	}

	for _, d := range []dialect.Dialect{sqlite.Dialect, sqlserver.Dialect} {
		for _, tt := range cases {
			calls = 0

			_, rendered, err := d.Render(
				&qb.Select{Columns: []qb.Selected{{Value: qb.Param{Value: tt.in}}}},
			)
			if err != nil {
				t.Fatal(err)
			}

			_, bound, err := dialect.Bind(d, "SELECT ?", []any{tt.in})
			if err != nil {
				t.Fatal(err)
			}

			if calls != 2 {
				t.Errorf("%s: %T was asked %d times to bind twice", d.Name(), tt.in, calls)
			}

			for _, arg := range []any{rendered[0], bound[0]} {
				calls = 0

				// database/sql converts the argument as the driver gets it.
				got, err := driver.DefaultParameterConverter.ConvertValue(arg)

				if got != tt.want || !errors.Is(err, tt.err) || calls != 0 {
					t.Errorf(
						"%s: %T went on as %#v, %v, asked %d more times",
						d.Name(),
						tt.in,
						got,
						err,
						calls,
					)
				}
			}
		}
	}
}

// changing is built on sql.Null of an interface holding a time, and its Value
// puts text there in its place, counting its calls.
type changing struct {
	sql.Null[any]
	calls *int
}

func (c *changing) Value() (driver.Value, error) {
	*c.calls++
	c.V = "text"

	return "text", nil
}

// A Valuer whose Value changes what it holds is still asked once: whether ant
// asks it is settled before it does.
func TestChangingValuerIsAskedOnce(t *testing.T) {
	at := time.Date(2026, 1, 2, 15, 4, 5, 0, time.FixedZone("UTC+3", 3*60*60))

	for _, d := range []dialect.Dialect{sqlite.Dialect, sqlserver.Dialect} {
		binds := map[string]func(v any) (any, error){
			"rendered": func(v any) (any, error) {
				_, args, err := d.Render(
					&qb.Select{Columns: []qb.Selected{{Value: qb.Param{Value: v}}}},
				)

				return args[0], err
			},
			"bound": func(v any) (any, error) {
				_, args, err := dialect.Bind(d, "SELECT ?", []any{v})

				return args[0], err
			},
		}

		for how, bind := range binds {
			calls := 0
			c := &changing{Null: sql.Null[any]{V: at, Valid: true}, calls: &calls}

			arg, err := bind(&c)
			if err != nil {
				t.Fatal(err)
			}

			got, err := driver.DefaultParameterConverter.ConvertValue(arg)
			if got != "text" || err != nil || calls != 1 {
				t.Errorf(
					"%s, %s: went on as %#v, %v, asked %d times",
					d.Name(),
					how,
					got,
					err,
					calls,
				)
			}
		}
	}
}

// loop is a pointer type that points to its own type, and can point to itself.
type loop *loop

// selfish is built on an sql.Null of an interface that can hold the selfish
// value itself, and gives a value of its own, so database/sql takes it.
type selfish struct{ sql.Null[any] }

func (selfish) Value() (driver.Value, error) { return "selfish", nil }

// sealed is built on a qb.Secret, which it hands its value through.
type sealed struct{ qb.Secret }

// A pointer that points back at itself, directly or through an interface, binds
// an error rather than leaving binding to follow it for ever, and a time held in
// a secret inside a Valuer is put in UTC and stays secret.
func TestCyclesAndNestedSecrets(t *testing.T) {
	east := time.FixedZone("UTC+3", 3*60*60)
	at := time.Date(2026, 1, 2, 15, 4, 5, 0, east)

	var self loop

	self = &self

	var held any

	held = &held

	nullSelf := &sql.Null[any]{Valid: true}
	nullSelf.V = nullSelf

	valuerSelf := &selfish{}
	valuerSelf.V, valuerSelf.Valid = valuerSelf, true

	for _, d := range []dialect.Dialect{sqlite.Dialect, sqlserver.Dialect} {
		binds := map[string]func(v any) (any, error){
			"rendered": func(v any) (any, error) {
				_, args, err := d.Render(
					&qb.Select{Columns: []qb.Selected{{Value: qb.Param{Value: v}}}},
				)

				return args[0], err
			},
			"bound": func(v any) (any, error) {
				_, args, err := dialect.Bind(d, "SELECT ?", []any{v})

				return args[0], err
			},
		}

		for how, bind := range binds {
			// The chains ant follows bind an error. An sql.Null holding one is
			// left to database/sql, as ant does not follow it into the Null.
			for name, cyclic := range map[string]struct {
				in    any
				fails bool
			}{
				"a pointer to itself":                  {self, true},
				"an interface holding its own address": {&held, true},
				"an sql.Null of a self pointing type":  {sql.Null[loop]{V: self, Valid: true}, false},
				"an sql.Null holding itself":           {nullSelf, false},
				"a Valuer on an sql.Null holding it":   {valuerSelf, false},
			} {
				done := make(chan error, 1)

				go func() {
					arg, err := bind(cyclic.in)
					if err == nil && cyclic.fails {
						_, err = driver.DefaultParameterConverter.ConvertValue(arg)
						if err == nil {
							err = errors.New("no error")
						}
					}

					done <- err
				}()

				select {
				case err := <-done:
					if cyclic.fails &&
						(err == nil || !strings.Contains(err.Error(), "points back at itself")) {
						t.Errorf(
							"%s, %s: %s gave %v, want that it points back at itself",
							d.Name(),
							how,
							name,
							err,
						)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s, %s: binding %s did not end", d.Name(), how, name)
				}
			}

			for name, in := range map[string]any{
				"an sql.Null holding a secret": sql.Null[any]{V: qb.Secret{V: at}, Valid: true},
				"a type built on a secret":     sealed{qb.Secret{V: at}},
			} {
				arg, err := bind(in)
				if err != nil {
					t.Fatal(err)
				}

				if !same(arg, qb.Secret{V: at.UTC()}) {
					t.Errorf(
						"%s, %s: %s went on as %#v, want a secret in UTC",
						d.Name(),
						how,
						name,
						arg,
					)
				}
			}
		}
	}
}

// fan's two embedded fields both lead to the next fan, so a walk that followed
// each of them in full would double at every link.
type (
	fan struct {
		*fanLeft
		*fanRight
	}
	fanLeft  struct{ *fan }
	fanRight struct{ *fan }
)

func (fan) Value() (driver.Value, error) { return "fan", nil }

// A value whose parts keep sharing what follows them is walked in bounded time,
// and goes on as it is.
func TestSharingChainsBindInBoundedTime(t *testing.T) {
	links := make([]*fan, 64)

	for i := range links {
		links[i] = &fan{}
	}

	for i := range len(links) - 1 {
		links[i].fanLeft, links[i].fanRight = &fanLeft{links[i+1]}, &fanRight{links[i+1]}
	}

	for _, d := range []dialect.Dialect{sqlite.Dialect, sqlserver.Dialect} {
		done := make(chan any, 1)

		go func() {
			_, args, err := dialect.Bind(d, "SELECT ?", []any{links[0]})
			if err != nil {
				done <- err

				return
			}

			done <- args[0]
		}()

		select {
		case got := <-done:
			if v, err := driver.DefaultParameterConverter.ConvertValue(
				got,
			); v != "fan" ||
				err != nil {
				t.Errorf("%s: went on as %#v, %v", d.Name(), v, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: binding a chain of shared parts did not end", d.Name())
		}
	}
}
