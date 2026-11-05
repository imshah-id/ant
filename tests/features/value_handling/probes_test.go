package value_handling

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/dialect/postgres"
	"github.com/skudra-go/ant/ids"
	"github.com/skudra-go/ant/secure"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// ---------------------------------------------------------------------------
// Generic scalar harness: one table per Go type V, with a NOT NULL / nullable
// column v, and a second table with a nullable pointer column p.
// ---------------------------------------------------------------------------

type prRow[V any] struct {
	ID int64
	V  V
}

func (*prRow[V]) Configure(b *ant.ModelBuilder[prRow[V]]) { b.Table(prTable[V]("row")) }

type prNRow[V any] struct {
	ID int64
	P  *V
}

func (*prNRow[V]) Configure(b *ant.ModelBuilder[prNRow[V]]) { b.Table(prTable[V]("nrow")) }

type prDB[V any] struct {
	ant.Context
	Rows  ant.Table[prRow[V]]
	NRows ant.Table[prNRow[V]]
}

func prTable[V any](kind string) string {
	full := reflect.TypeFor[V]().String()
	h := fnv.New32a()
	h.Write([]byte(full))

	clean := strings.Map(
		func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				return r
			case r >= 'A' && r <= 'Z':
				return r + 32
			}

			return '_'
		},
		full,
	)

	if len(clean) > 30 {
		clean = clean[:30]
	}

	return fmt.Sprintf("pr_%s_%s_%08x", kind, clean, h.Sum32())
}

// spec describes one Go type and the column it is stored in.
type spec[V any] struct {
	name     string
	sql      string // column type
	vals     []V    // distinct, ascending in the database's order
	nullable bool   // the column allows NULL (some vals may be stored as NULL)
	isNull   func(V) bool
	same     func(a, b V) bool // default reflect.DeepEqual
	norm     func(V) V         // documented conversion applied on write
	eq       func(*V, V) ant.Cond
	in       func(*V, ...V) ant.Cond
	gt       func(*V, V) ant.Cond
	noPtr    bool // skip the pointer-column phase
	noEq     bool // the database type has no = operator
}

func (s spec[V]) ddl() string {
	null := " NOT NULL"

	if s.nullable {
		null = ""
	}

	return fmt.Sprintf(
		`CREATE TABLE %s (id BIGSERIAL PRIMARY KEY, v %s%s);
		 CREATE TABLE %s (id BIGSERIAL PRIMARY KEY, p %s)`,
		prTable[V]("row"),
		s.sql,
		null,
		prTable[V]("nrow"),
		s.sql,
	)
}

func (s spec[V]) equal(a, b V) bool {
	if s.same != nil {
		return s.same(a, b)
	}

	return reflect.DeepEqual(a, b)
}

func (s spec[V]) want(v V) V {
	if s.norm != nil {
		return s.norm(v)
	}

	return v
}

func (s spec[V]) null(v V) bool { return s.isNull != nil && s.isNull(v) }

// step runs fn as a subtest, turning a panic into a failure, and reports whether
// it passed.
func step(t *testing.T, name string, fn func(t *testing.T)) bool {
	t.Helper()

	return t.Run(name, func(t *testing.T) {
		knownHere(t)

		defer func() {
			if r := recover(); r != nil {
				t.Errorf("PANIC: %v\n%s", r, debug.Stack())
			}
		}()

		fn(t)
	})
}

// probe runs every phase for one type.
func probe[V any](t *testing.T, s spec[V]) {
	t.Helper()

	t.Run(s.name, func(t *testing.T) {
		knownHere(t)

		fixture.RunWith(t, s.ddl(), func(t *testing.T, db *prDB[V]) { s.crud(t, db) })

		if !s.noPtr {
			fixture.RunWith(t, s.ddl(), func(t *testing.T, db *prDB[V]) { s.pointers(t, db) })
		}

		fixture.RunTrackedWith(t, s.ddl(), func(t *testing.T, db *prDB[V]) { s.tracked(t, db) })

		if !s.noPtr {
			fixture.RunTrackedWith(t, s.ddl(), func(t *testing.T, db *prDB[V]) {
				s.pointersTracked(t, db)
			})
		}
	})
}

func idsOf[V any](rows []*prRow[V]) []int64 {
	out := make([]int64, len(rows))

	for i, r := range rows {
		out[i] = r.ID
	}

	return out
}

func (s spec[V]) crud(t *testing.T, db *prDB[V]) {
	ctx := t.Context()
	f := ant.Fields[prRow[V]]()
	n := len(s.vals)
	ids := make([]int64, n)

	find := func(t *testing.T, id int64) V {
		t.Helper()

		got, err := db.Rows.NoTracking().Find(ctx, id)
		require.NoError(t, err, "Find %d", id)

		return got.V
	}

	idsWhere := func(t *testing.T, c ...ant.Cond) []int64 {
		t.Helper()

		rows, err := db.Rows.NoTracking().Where(c...).OrderBy(&f.ID).All(ctx)
		require.NoError(t, err)

		return idsOf(rows)
	}

	if !step(t, "insert", func(t *testing.T) {
		knownHere(t)

		for i, v := range s.vals {
			e := &prRow[V]{V: v}
			require.NoError(t, db.Rows.Insert(ctx, e), "insert %#v", v)
			require.NotZero(t, e.ID)

			ids[i] = e.ID

			if !s.equal(e.V, v) {
				t.Errorf("insert changed the entity: wrote %#v, entity now %#v", v, e.V)
			}

			if got := find(t, e.ID); !s.equal(got, s.want(v)) {
				t.Errorf("insert %#v: read back %#v, want %#v", v, got, s.want(v))
			}
		}
	}) {
		return
	}

	step(t, "all", func(t *testing.T) {
		knownHere(t)

		rows, err := db.Rows.NoTracking().OrderBy(&f.ID).All(ctx)
		require.NoError(t, err)
		require.Len(t, rows, n)

		for i, r := range rows {
			if !s.equal(r.V, s.want(s.vals[i])) {
				t.Errorf("All[%d]: got %#v, want %#v", i, r.V, s.want(s.vals[i]))
			}
		}
	})

	if s.eq != nil {
		step(t, "eq", func(t *testing.T) {
			knownHere(t)

			for i, v := range s.vals {
				got := idsWhere(t, s.eq(&f.V, v))
				if !slices.Equal(got, []int64{ids[i]}) {
					t.Errorf("Eq(%#v): rows %v, want [%d] (val index %d)", v, got, ids[i], i)
				}
			}
		})
	}

	if s.in != nil {
		step(t, "in", func(t *testing.T) {
			knownHere(t)

			var nonNull []int64

			for i, v := range s.vals {
				if !s.null(v) {
					nonNull = append(nonNull, ids[i])
				}
			}

			if got := idsWhere(t, s.in(&f.V, s.vals...)); !slices.Equal(got, nonNull) {
				t.Errorf(
					"In(all %d values): rows %v, want %v (NULL never matches IN)",
					n,
					got,
					nonNull,
				)
			}

			if got := idsWhere(t, s.in(&f.V)); len(got) != 0 {
				t.Errorf("In(): rows %v, want none", got)
			}

			if got := idsWhere(t, ant.Not(s.in(&f.V))); len(got) != n {
				t.Errorf("Not(In()): %d rows, want %d", len(got), n)
			}

			if n >= 3 {
				a, b := s.vals[0], s.vals[n-1]
				want := []int64{}

				for _, v := range []V{a, b} {
					for i, w := range s.vals {
						if !s.null(v) && s.equal(v, w) {
							want = append(want, ids[i])
						}
					}
				}

				slices.Sort(want)

				if got := idsWhere(t, s.in(&f.V, a, b)); !slices.Equal(got, want) {
					t.Errorf("In(first,last): rows %v, want %v", got, want)
				}
			}
		})
	}

	if s.gt != nil {
		step(t, "gt", func(t *testing.T) {
			knownHere(t)

			for i, v := range s.vals {
				if s.null(v) {
					continue
				}

				var want []int64

				for j := i + 1; j < n; j++ {
					if !s.null(s.vals[j]) {
						want = append(want, ids[j])
					}
				}

				if got := idsWhere(t, s.gt(&f.V, v)); !slices.Equal(got, want) {
					t.Errorf("Gt(%#v): rows %v, want %v", v, got, want)
				}
			}
		})
	}

	step(t, "update", func(t *testing.T) {
		knownHere(t)

		for i, id := range ids {
			v := s.vals[(i+1)%n]
			cnt, err := db.Rows.Where(ant.Eq(&f.ID, id)).Update(ctx, ant.Set(&f.V, v))
			require.NoError(t, err, "Set %#v", v)
			require.EqualValues(t, 1, cnt)

			if got := find(t, id); !s.equal(got, s.want(v)) {
				t.Errorf("Update Set(%#v): read back %#v, want %#v", v, got, s.want(v))
			}
		}

		for _, v := range s.vals {
			cnt, err := db.Rows.Where(ant.IsNotNull(&f.ID)).Update(ctx, ant.Set(&f.V, v))
			require.NoError(t, err, "Set %#v", v)
			require.EqualValues(t, n, cnt)

			rows, err := db.Rows.NoTracking().All(ctx)
			require.NoError(t, err)

			for _, r := range rows {
				if !s.equal(r.V, s.want(v)) {
					t.Errorf("bulk Set(%#v): row %d reads %#v, want %#v", v, r.ID, r.V, s.want(v))
				}
			}

			if s.eq != nil {
				if got := idsWhere(t, s.eq(&f.V, v)); len(got) != n {
					t.Errorf("after bulk Set(%#v): Eq finds %d rows, want %d", v, len(got), n)
				}
			}
		}

		v := s.vals[n-1]
		got, err := db.Rows.Where(ant.Eq(&f.ID, ids[0])).UpdateReturning(ctx, ant.Set(&f.V, v))
		require.NoError(t, err)
		require.Len(t, got, 1)

		if !s.equal(got[0].V, s.want(v)) {
			t.Errorf("UpdateReturning(%#v): got %#v, want %#v", v, got[0].V, s.want(v))
		}
	})

	step(t, "insertmany", func(t *testing.T) {
		knownHere(t)

		for _, batch := range []int{1000, 2} {
			rows := make([]*prRow[V], n)

			for i, v := range s.vals {
				rows[i] = &prRow[V]{V: v}
			}

			cnt, err := db.Rows.InsertMany(ctx, rows, ant.BatchSize(batch))
			require.NoError(t, err)
			require.EqualValues(t, n, cnt)

			for i, r := range rows {
				require.NotZero(t, r.ID, "InsertMany did not copy back a key (batch %d)", batch)

				if got := find(t, r.ID); !s.equal(got, s.want(s.vals[i])) {
					t.Errorf(
						"InsertMany(batch %d) %#v: read back %#v, want %#v",
						batch,
						s.vals[i],
						got,
						s.want(s.vals[i]),
					)
				}
			}
		}
	})

	step(t, "upsert", func(t *testing.T) {
		knownHere(t)

		for i := range s.vals {
			id := int64(1_000_000 + i)
			first, second := s.vals[i], s.vals[(i+1)%n]

			e := &prRow[V]{ID: id, V: first}
			ok, err := db.Rows.Upsert(ctx, e, ant.OnConflict(&f.ID))
			require.NoError(t, err, "upsert insert %#v", first)
			require.True(t, ok)

			if !s.equal(e.V, s.want(first)) {
				t.Errorf("Upsert insert %#v: entity holds %#v, want %#v", first, e.V, s.want(first))
			}

			if got := find(t, id); !s.equal(got, s.want(first)) {
				t.Errorf("Upsert insert %#v: read back %#v", first, got)
			}

			e2 := &prRow[V]{ID: id, V: second}
			ok, err = db.Rows.Upsert(ctx, e2, ant.OnConflict(&f.ID))
			require.NoError(t, err, "upsert update %#v", second)
			require.True(t, ok)

			if !s.equal(e2.V, s.want(second)) {
				t.Errorf(
					"Upsert update %#v: entity holds %#v, want %#v",
					second,
					e2.V,
					s.want(second),
				)
			}

			if got := find(t, id); !s.equal(got, s.want(second)) {
				t.Errorf("Upsert update %#v: read back %#v, want %#v", second, got, s.want(second))
			}

			e3 := &prRow[V]{ID: id, V: first}
			ok, err = db.Rows.Upsert(ctx, e3, ant.OnConflict(&f.ID).DoNothing())
			require.NoError(t, err)
			require.False(t, ok, "DoNothing on an existing row")

			if got := find(t, id); !s.equal(got, s.want(second)) {
				t.Errorf("Upsert DoNothing changed the row to %#v", got)
			}
		}

		// InsertMany with a conflict clause overwrites by key.
		rows := make([]*prRow[V], n)

		for i := range s.vals {
			rows[i] = &prRow[V]{ID: int64(1_000_000 + i), V: s.vals[n-1-i]}
		}

		_, err := db.Rows.InsertMany(ctx, rows, ant.WithConflict(ant.OnConflict(&f.ID)))
		require.NoError(t, err)

		for i, r := range rows {
			if got := find(t, r.ID); !s.equal(got, s.want(s.vals[n-1-i])) {
				t.Errorf("InsertMany+conflict %#v: read back %#v", s.vals[n-1-i], got)
			}
		}

		// Conflict.Set and DoUpdate with an explicit column.
		for i := range s.vals {
			id := int64(1_000_000 + i)
			third := s.vals[(i+2)%n]
			e := &prRow[V]{ID: id, V: s.vals[i]}
			ok, err := db.Rows.Upsert(ctx, e, ant.OnConflict(&f.ID).Set(ant.Set(&f.V, third)))
			require.NoError(t, err)
			require.True(t, ok)

			if got := find(t, id); !s.equal(got, s.want(third)) {
				t.Errorf("Upsert Conflict.Set(%#v): read back %#v", third, got)
			}

			e = &prRow[V]{ID: id, V: s.vals[i]}
			ok, err = db.Rows.Upsert(ctx, e, ant.OnConflict(&f.ID).DoUpdate(ant.F(&f.V)))
			require.NoError(t, err)
			require.True(t, ok)

			if got := find(t, id); !s.equal(got, s.want(s.vals[i])) {
				t.Errorf("Upsert DoUpdate(V) %#v: read back %#v", s.vals[i], got)
			}
		}
	})

	step(t, "updatemany reload deletereturning", func(t *testing.T) {
		knownHere(t)

		rows := make([]*prRow[V], n)

		for i, v := range s.vals {
			rows[i] = &prRow[V]{V: v}
		}

		require.NoError(t, db.Rows.Insert(ctx, rows...))

		for i, r := range rows {
			r.V = s.vals[(i+1)%n]
		}

		cnt, err := db.Rows.UpdateMany(ctx, rows, ant.F(&f.V))
		require.NoError(t, err)
		require.EqualValues(t, n, cnt)

		for i, r := range rows {
			if got := find(t, r.ID); !s.equal(got, s.want(s.vals[(i+1)%n])) {
				t.Errorf("UpdateMany %#v: read back %#v", s.vals[(i+1)%n], got)
			}
		}

		// Reload overwrites the entity with the stored (converted) value.
		for i, r := range rows {
			_, err := db.Rows.Where(ant.Eq(&f.ID, r.ID)).Update(ctx, ant.Set(&f.V, s.vals[i]))
			require.NoError(t, err)
			require.NoError(t, db.Rows.Reload(ctx, r))

			if !s.equal(r.V, s.want(s.vals[i])) {
				t.Errorf("Reload: entity holds %#v, want %#v", r.V, s.want(s.vals[i]))
			}
		}

		ids := make([]int64, n)

		for i, r := range rows {
			ids[i] = r.ID
		}

		deleted, err := db.Rows.Where(ant.In(&f.ID, ids...)).DeleteReturning(ctx)
		require.NoError(t, err)
		require.Len(t, deleted, n)

		slices.SortFunc(deleted, func(a, b *prRow[V]) int { return cmp.Compare(a.ID, b.ID) })

		for i, r := range deleted {
			if !s.equal(r.V, s.want(s.vals[i])) {
				t.Errorf("DeleteReturning: row %d holds %#v, want %#v", i, r.V, s.want(s.vals[i]))
			}
		}
	})
}

func (s spec[V]) tracked(t *testing.T, db *prDB[V]) {
	ctx := t.Context()
	f := ant.Fields[prRow[V]]()
	n := len(s.vals)

	step(t, "savechanges", func(t *testing.T) {
		knownHere(t)

		e := &prRow[V]{V: s.vals[0]}
		require.NoError(t, db.Rows.Insert(ctx, e))

		if db.HasChanges() {
			t.Errorf("HasChanges after Insert of %#v: %s", s.vals[0], db.DebugTracked())
		}

		for i := 1; i <= n; i++ {
			v := s.vals[i%n]
			e.V = v

			if n > 1 && !db.HasChanges() {
				t.Errorf("HasChanges is false after changing the field to %#v", v)
			}

			require.NoError(t, db.SaveChanges(ctx), "SaveChanges %#v", v)

			if db.HasChanges() {
				t.Errorf("HasChanges after SaveChanges of %#v: %s", v, db.DebugTracked())
			}

			got, err := db.Rows.NoTracking().Find(ctx, e.ID)
			require.NoError(t, err)

			if !s.equal(got.V, s.want(v)) {
				t.Errorf("SaveChanges %#v: read back %#v, want %#v", v, got.V, s.want(v))
			}
		}
	})

	step(t, "tracked load is clean", func(t *testing.T) {
		knownHere(t)

		for _, v := range s.vals {
			require.NoError(t, db.Rows.Insert(ctx, &prRow[V]{V: v}))
		}

		db.ClearTracked()

		rows, err := db.Rows.OrderBy(&f.ID).All(ctx)
		require.NoError(t, err)

		for _, r := range rows {
			if !db.HasChanges() {
				continue
			}

			t.Errorf(
				"HasChanges right after loading (row %d = %#v): %s",
				r.ID,
				r.V,
				db.DebugTracked(),
			)

			break
		}

		require.NoError(t, db.SaveChanges(ctx))
	})
}

func (s spec[V]) pointers(t *testing.T, db *prDB[V]) {
	ctx := t.Context()
	f := ant.Fields[prNRow[V]]()
	n := len(s.vals)

	idsWhere := func(t *testing.T, c ...ant.Cond) []int64 {
		t.Helper()

		rows, err := db.NRows.NoTracking().Where(c...).OrderBy(&f.ID).All(ctx)
		require.NoError(t, err)

		out := make([]int64, len(rows))

		for i, r := range rows {
			out[i] = r.ID
		}

		return out
	}

	read := func(t *testing.T, id int64) *V {
		t.Helper()

		got, err := db.NRows.NoTracking().Find(ctx, id)
		require.NoError(t, err)

		return got.P
	}

	var (
		nilID int64
		ids   = make([]int64, n)
	)

	if !step(t, "insert", func(t *testing.T) {
		knownHere(t)

		e := &prNRow[V]{}
		require.NoError(t, db.NRows.Insert(ctx, e))
		nilID = e.ID

		if p := read(t, nilID); p != nil {
			t.Errorf("nil pointer read back as %#v, want nil", *p)
		}

		for i := range s.vals {
			v := s.vals[i]
			e := &prNRow[V]{P: &v}
			require.NoError(t, db.NRows.Insert(ctx, e), "insert &%#v", v)
			ids[i] = e.ID

			p := read(t, e.ID)
			if p == nil {
				if !s.null(v) {
					t.Errorf("pointer to %#v read back as nil", v)
				}

				continue
			}

			if !s.equal(*p, s.want(v)) {
				t.Errorf("pointer to %#v read back as %#v, want %#v", v, *p, s.want(v))
			}
		}
	}) {
		return
	}

	step(t, "conditions", func(t *testing.T) {
		knownHere(t)

		nulls := []int64{nilID}

		for i, v := range s.vals {
			if s.null(v) {
				nulls = append(nulls, ids[i])
			}
		}

		slices.Sort(nulls)

		if got := idsWhere(t, ant.IsNull(&f.P)); !slices.Equal(got, nulls) {
			t.Errorf("IsNull(P): rows %v, want %v", got, nulls)
		}

		if got := idsWhere(t, ant.IsNotNull(&f.P)); len(got) != n+1-len(nulls) {
			t.Errorf("IsNotNull(P): %d rows, want %d", len(got), n+1-len(nulls))
		}

		if got := idsWhere(t, ant.Eq(&f.P, (*V)(nil))); !slices.Equal(got, nulls) {
			t.Errorf("Eq(P, nil): rows %v, want %v", got, nulls)
		}

		if got := idsWhere(t, ant.Ne(&f.P, (*V)(nil))); len(got) != n+1-len(nulls) {
			t.Errorf("Ne(P, nil): %d rows, want %d", len(got), n+1-len(nulls))
		}

		for i := range s.vals {
			v := s.vals[i]
			if s.null(v) || s.noEq {
				continue
			}

			if got := idsWhere(t, ant.Eq(&f.P, &v)); !slices.Equal(got, []int64{ids[i]}) {
				t.Errorf("Eq(P, &%#v): rows %v, want [%d]", v, got, ids[i])
			}
		}

		if n >= 2 && !s.null(s.vals[0]) && !s.null(s.vals[n-1]) && !s.noEq {
			a, b := s.vals[0], s.vals[n-1]
			if got := idsWhere(
				t,
				ant.In(&f.P, &a, &b),
			); !slices.Equal(
				got,
				[]int64{ids[0], ids[n-1]},
			) {
				t.Errorf("In(P, &first, &last): rows %v, want [%d %d]", got, ids[0], ids[n-1])
			}

			if got := idsWhere(t, ant.In(&f.P, &a, nil)); !slices.Equal(got, []int64{ids[0]}) {
				t.Logf(
					"NOTE In(P, &first, nil): rows %v (NULL row %d, first %d)",
					got,
					nilID,
					ids[0],
				)
			}
		}

		if got := idsWhere(t, ant.In(&f.P)); len(got) != 0 {
			t.Errorf("In(P) empty: rows %v", got)
		}
	})

	step(t, "update", func(t *testing.T) {
		knownHere(t)

		// A value to NULL, and NULL to a value.
		cnt, err := db.NRows.Where(ant.Eq(&f.ID, ids[0])).Update(ctx, ant.Set(&f.P, (*V)(nil)))
		require.NoError(t, err)
		require.EqualValues(t, 1, cnt)

		if p := read(t, ids[0]); p != nil {
			t.Errorf("Set(P, nil): read back %#v, want nil", *p)
		}

		v := s.vals[n-1]
		cnt, err = db.NRows.Where(ant.Eq(&f.ID, nilID)).Update(ctx, ant.Set(&f.P, &v))
		require.NoError(t, err)
		require.EqualValues(t, 1, cnt)

		if p := read(t, nilID); p == nil || !s.equal(*p, s.want(v)) {
			t.Errorf("Set(P, &%#v): read back %v", v, p)
		}
	})

	step(t, "insertmany", func(t *testing.T) {
		knownHere(t)

		rows := []*prNRow[V]{{}}

		for i := range s.vals {
			rows = append(rows, &prNRow[V]{P: &s.vals[i]}, &prNRow[V]{})
		}

		cnt, err := db.NRows.InsertMany(ctx, rows, ant.BatchSize(3))
		require.NoError(t, err)
		require.EqualValues(t, len(rows), cnt)

		for _, r := range rows {
			p := read(t, r.ID)

			switch {
			case r.P == nil && p != nil:
				t.Errorf("InsertMany: nil pointer read back as %#v", *p)
			case r.P != nil && p == nil && !s.null(*r.P):
				t.Errorf("InsertMany: pointer to %#v read back as nil", *r.P)
			case r.P != nil && p == nil:
			case r.P != nil && !s.equal(*p, s.want(*r.P)):
				t.Errorf("InsertMany: pointer to %#v read back as %#v", *r.P, *p)
			}
		}
	})

	step(t, "upsert", func(t *testing.T) {
		knownHere(t)

		v := s.vals[n-1]
		e := &prNRow[V]{ID: 5_000_000, P: &v}
		ok, err := db.NRows.Upsert(ctx, e, ant.OnConflict(&f.ID))
		require.NoError(t, err)
		require.True(t, ok)

		e2 := &prNRow[V]{ID: 5_000_000}
		ok, err = db.NRows.Upsert(ctx, e2, ant.OnConflict(&f.ID))
		require.NoError(t, err)
		require.True(t, ok)

		if e2.P != nil {
			t.Errorf("Upsert with a nil pointer: entity holds %#v", *e2.P)
		}

		if p := read(t, 5_000_000); p != nil {
			t.Errorf("Upsert with a nil pointer over a value: read back %#v, want nil", *p)
		}
	})
}

// ---------------------------------------------------------------------------
// helpers for building specs
// ---------------------------------------------------------------------------

func ord[V cmp.Ordered](name, sqlType string, vals ...V) spec[V] {
	return spec[V]{
		name: name,
		sql:  sqlType,
		vals: vals,
		eq:   ant.Eq[V],
		in:   ant.In[V],
		gt:   ant.Gt[V],
	}
}

func floatSame[V ~float32 | ~float64](a, b V) bool {
	return a == b || (a != a && b != b)
}

func timeSame(a, b time.Time) bool { return a.Equal(b) }

// ---------------------------------------------------------------------------
// the scalar matrix
// ---------------------------------------------------------------------------

func TestProbeScalars(t *testing.T) {
	knownHere(t)

	probe(t, spec[bool]{
		name: "bool",
		sql:  "BOOLEAN",
		vals: []bool{false, true},
		eq:   ant.Eq[bool],
		in:   ant.In[bool],
	})

	probe(t, ord("int8", "SMALLINT", int8(math.MinInt8), -1, 0, 1, math.MaxInt8))
	probe(t, ord("int16", "SMALLINT", int16(math.MinInt16), -1, 0, 1, math.MaxInt16))
	probe(t, ord("int32", "INTEGER", int32(math.MinInt32), -1, 0, 1, math.MaxInt32))
	probe(t, ord("int64", "BIGINT", int64(math.MinInt64), -1, 0, 1, math.MaxInt64))
	probe(t, ord("int", "BIGINT", math.MinInt, -1, 0, 1, math.MaxInt))
	probe(t, ord("uint8", "SMALLINT", uint8(0), 1, 127, 128, math.MaxUint8))
	probe(t, ord("uint16", "INTEGER", uint16(0), 1, 32768, math.MaxUint16))
	probe(t, ord("uint32", "BIGINT", uint32(0), 1, math.MaxInt32+1, math.MaxUint32))

	probe(t, ord(
		"uint64 numeric",
		"NUMERIC(20,0)",
		uint64(0),
		1,
		math.MaxInt64,
		math.MaxInt64+1,
		math.MaxUint64,
	))

	probe(t, ord(
		"uint numeric",
		"NUMERIC(20,0)",
		uint(0),
		1,
		math.MaxInt64,
		math.MaxInt64+1,
		math.MaxUint64,
	))

	s := ord(
		"float64",
		"DOUBLE PRECISION",
		math.Inf(-1),
		-math.MaxFloat64,
		-1.5,
		-math.SmallestNonzeroFloat64,
		0,
		math.SmallestNonzeroFloat64,
		0.1,
		1.5,
		math.MaxFloat64,
		math.Inf(1),
		math.NaN(),
	)
	s.same = floatSame[float64]
	probe(t, s)

	s32 := ord(
		"float32",
		"REAL",
		float32(math.Inf(-1)),
		-math.MaxFloat32,
		-1.5,
		-math.SmallestNonzeroFloat32,
		0,
		math.SmallestNonzeroFloat32,
		0.1,
		1.5,
		math.MaxFloat32,
		float32(math.Inf(1)),
		float32(math.NaN()),
	)
	s32.same = floatSame[float32]
	probe(t, s32)

	str := ord(
		"string",
		`TEXT COLLATE "C"`,
		"",
		" ",
		"a",
		"A",
		"a b",
		"NULL",
		"null",
		"'",
		`"`,
		`\`,
		`\\n`,
		"%_",
		"\n\t\r",
		"\u65e5\u672c\u8a9e",
		"emoji \U0001F600\U0001F389\U0001F468\u200d\U0001F469\u200d\U0001F467",
		"e\u0301",
		"\u00e9",
		"\u00a0",
		"\ufeffbom",
		"\u202eRTL",
		strings.Repeat("x", 1<<20),
		strings.Repeat("ab", 70_000)+"\U0001F600",
	)
	slices.Sort(str.vals)
	probe(t, str)

	probe(t, spec[time.Time]{
		name: "time.Time timestamptz",
		sql:  "TIMESTAMPTZ",
		vals: []time.Time{
			time.Time{},
			time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(1969, 12, 31, 23, 59, 59, 999_999_000, time.UTC),
			time.Unix(0, 0),
			time.Date(2024, 2, 29, 12, 0, 0, 123_456_000, time.FixedZone("x", 5*3600+1800)),
			time.Date(2024, 2, 29, 12, 0, 1, 0, time.FixedZone("x", -8*3600)),
			time.Date(2038, 1, 19, 3, 14, 8, 0, time.UTC),
			time.Date(9999, 12, 31, 23, 59, 59, 999_999_000, time.UTC),
			time.Date(100000, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		same: timeSame,
		eq:   ant.Eq[time.Time],
		in:   ant.In[time.Time],
		gt:   ant.Gt[time.Time],
	})

	probe(t, spec[time.Time]{
		name: "time.Time with nanoseconds",
		sql:  "TIMESTAMPTZ",
		vals: []time.Time{
			time.Date(2024, 1, 1, 0, 0, 0, 123_456_000, time.UTC),
			time.Date(2024, 1, 1, 0, 0, 0, 123_457_000, time.UTC),
		},
		same: timeSame,
		norm: func(v time.Time) time.Time { return v.Truncate(time.Microsecond) },
		eq:   ant.Eq[time.Time],
		in:   ant.In[time.Time],
		gt:   ant.Gt[time.Time],
	})

	probe(t, spec[time.Time]{
		name: "time.Time nanoseconds truncate",
		sql:  "TIMESTAMPTZ",
		vals: []time.Time{
			time.Date(1969, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
			time.Date(2024, 1, 1, 0, 0, 0, 123_456_789, time.UTC),
			time.Date(2024, 1, 1, 0, 0, 0, 999_999_999, time.UTC),
		},
		same:  timeSame,
		norm:  func(v time.Time) time.Time { return v.Truncate(time.Microsecond) },
		eq:    ant.Eq[time.Time],
		in:    ant.In[time.Time],
		gt:    ant.Gt[time.Time],
		noPtr: true,
	})

	probe(t, spec[time.Time]{
		name: "time.Time timestamp without zone (UTC)",
		sql:  "TIMESTAMP",
		vals: []time.Time{
			time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2024, 2, 29, 12, 0, 0, 123_456_000, time.UTC),
			time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		},
		same: timeSame,
		eq:   ant.Eq[time.Time],
		in:   ant.In[time.Time],
		gt:   ant.Gt[time.Time],
	})

	probe(t, spec[time.Duration]{
		name: "time.Duration bigint",
		sql:  "BIGINT",
		vals: []time.Duration{math.MinInt64, -1, 0, 1, time.Hour, math.MaxInt64},
		eq:   ant.Eq[time.Duration],
		in:   ant.In[time.Duration],
		gt:   ant.Gt[time.Duration],
	})

	probe(t, spec[ant.UnixTime]{
		name: "ant.UnixTime",
		sql:  "BIGINT",
		vals: []ant.UnixTime{
			{Time: time.Unix(-1, 0).UTC()},
			{Time: time.Unix(1, 0).UTC()},
			{Time: time.Unix(1_700_000_000, 0).UTC()},
		},
		same: func(a, b ant.UnixTime) bool { return a.Time.Equal(b.Time) },
		eq:   ant.Eq[ant.UnixTime],
		in:   ant.In[ant.UnixTime],
	})

	// The Unix epoch itself is stored as 0, which reads back as the zero time.
	probe(t, spec[ant.UnixTime]{
		name:  "ant.UnixTime epoch",
		sql:   "BIGINT",
		vals:  []ant.UnixTime{{Time: time.Unix(0, 0).UTC()}},
		same:  func(a, b ant.UnixTime) bool { return a.Time.Equal(b.Time) },
		eq:    ant.Eq[ant.UnixTime],
		in:    ant.In[ant.UnixTime],
		noPtr: true,
	})
}

// ---------------------------------------------------------------------------
// sql.Null[T] and the legacy Null types
// ---------------------------------------------------------------------------

func nullSpec[V comparable](name, sqlType string, valid func(V) bool, vals ...V) spec[V] {
	return spec[V]{
		name:     name,
		sql:      sqlType,
		vals:     vals,
		nullable: true,
		isNull:   func(v V) bool { return !valid(v) },
		eq:       ant.Eq[V],
		in:       ant.In[V],
	}
}

func TestProbeNullTypes(t *testing.T) {
	knownHere(t)

	ts := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)

	probe(t, nullSpec(
		"sql.Null[int64]",
		"BIGINT",
		func(v sql.Null[int64]) bool { return v.Valid },
		sql.Null[int64]{},
		sql.Null[int64]{V: 0, Valid: true},
		sql.Null[int64]{V: math.MaxInt64, Valid: true},
	))

	probe(t, nullSpec(
		"sql.Null[int32]",
		"INTEGER",
		func(v sql.Null[int32]) bool { return v.Valid },
		sql.Null[int32]{},
		sql.Null[int32]{V: math.MinInt32, Valid: true},
		sql.Null[int32]{V: 0, Valid: true},
	))

	probe(t, nullSpec(
		"sql.Null[string]",
		"TEXT",
		func(v sql.Null[string]) bool { return v.Valid },
		sql.Null[string]{},
		sql.Null[string]{V: "", Valid: true},
		sql.Null[string]{V: "NULL", Valid: true},
		sql.Null[string]{V: "x", Valid: true},
	))

	probe(t, nullSpec(
		"sql.Null[bool]",
		"BOOLEAN",
		func(v sql.Null[bool]) bool { return v.Valid },
		sql.Null[bool]{},
		sql.Null[bool]{V: false, Valid: true},
		sql.Null[bool]{V: true, Valid: true},
	))

	probe(t, nullSpec(
		"sql.Null[float64]",
		"DOUBLE PRECISION",
		func(v sql.Null[float64]) bool { return v.Valid },
		sql.Null[float64]{},
		sql.Null[float64]{V: 0, Valid: true},
		sql.Null[float64]{V: math.Inf(1), Valid: true},
	))

	probe(t, nullSpec(
		"sql.Null[uint8]",
		"SMALLINT",
		func(v sql.Null[uint8]) bool { return v.Valid },
		sql.Null[uint8]{},
		sql.Null[uint8]{V: 0, Valid: true},
		sql.Null[uint8]{V: 255, Valid: true},
	))

	probe(t, nullSpec(
		"sql.Null[uint64]",
		"NUMERIC(20,0)",
		func(v sql.Null[uint64]) bool { return v.Valid },
		sql.Null[uint64]{},
		sql.Null[uint64]{V: 0, Valid: true},
		sql.Null[uint64]{V: math.MaxInt64, Valid: true},
	))
	// database/sql's own Null[T].Value refuses uint64 with the high bit set, though a
	// bare uint64 field is accepted above.
	probe(t, nullSpec(
		"sql.Null[uint64] high bit",
		"NUMERIC(20,0)",
		func(v sql.Null[uint64]) bool { return v.Valid },
		sql.Null[uint64]{V: math.MaxUint64, Valid: true},
	))

	tn := nullSpec(
		"sql.Null[time.Time]",
		"TIMESTAMPTZ",
		func(v sql.Null[time.Time]) bool { return v.Valid },
		sql.Null[time.Time]{},
		sql.Null[time.Time]{V: time.Time{}, Valid: true},
		sql.Null[time.Time]{V: ts, Valid: true},
	)
	tn.same = func(a, b sql.Null[time.Time]) bool { return a.Valid == b.Valid && a.V.Equal(b.V) }
	probe(t, tn)

	probe(t, nullSpec(
		"sql.NullString",
		"TEXT",
		func(v sql.NullString) bool { return v.Valid },
		sql.NullString{},
		sql.NullString{Valid: true},
		sql.NullString{String: "x", Valid: true},
	))

	probe(t, nullSpec(
		"sql.NullInt64",
		"BIGINT",
		func(v sql.NullInt64) bool { return v.Valid },
		sql.NullInt64{},
		sql.NullInt64{Valid: true},
		sql.NullInt64{Int64: -5, Valid: true},
	))

	probe(t, nullSpec(
		"sql.NullInt32",
		"INTEGER",
		func(v sql.NullInt32) bool { return v.Valid },
		sql.NullInt32{},
		sql.NullInt32{Valid: true},
		sql.NullInt32{Int32: 5, Valid: true},
	))

	probe(t, nullSpec(
		"sql.NullInt16",
		"SMALLINT",
		func(v sql.NullInt16) bool { return v.Valid },
		sql.NullInt16{},
		sql.NullInt16{Valid: true},
		sql.NullInt16{Int16: 5, Valid: true},
	))

	probe(t, nullSpec(
		"sql.NullByte",
		"SMALLINT",
		func(v sql.NullByte) bool { return v.Valid },
		sql.NullByte{},
		sql.NullByte{Valid: true},
		sql.NullByte{Byte: 255, Valid: true},
	))

	probe(t, nullSpec(
		"sql.NullBool",
		"BOOLEAN",
		func(v sql.NullBool) bool { return v.Valid },
		sql.NullBool{},
		sql.NullBool{Valid: true},
		sql.NullBool{Bool: true, Valid: true},
	))

	probe(t, nullSpec(
		"sql.NullFloat64",
		"DOUBLE PRECISION",
		func(v sql.NullFloat64) bool { return v.Valid },
		sql.NullFloat64{},
		sql.NullFloat64{Valid: true},
		sql.NullFloat64{Float64: 1.5, Valid: true},
	))

	nt := nullSpec(
		"sql.NullTime",
		"TIMESTAMPTZ",
		func(v sql.NullTime) bool { return v.Valid },
		sql.NullTime{},
		sql.NullTime{Valid: true},
		sql.NullTime{Time: ts, Valid: true},
	)
	nt.same = func(a, b sql.NullTime) bool { return a.Valid == b.Valid && a.Time.Equal(b.Time) }
	probe(t, nt)
}

// An invalid Null value that still holds data is stored as NULL.
func TestProbeInvalidNullWithData(t *testing.T) {
	knownHere(t)

	ddl := `CREATE TABLE ` + prTable[sql.Null[int64]](
		"row",
	) + ` (id BIGSERIAL PRIMARY KEY, v BIGINT)`
	fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[sql.Null[int64]]) {
		ctx := t.Context()
		f := ant.Fields[prRow[sql.Null[int64]]]()
		e := &prRow[sql.Null[int64]]{V: sql.Null[int64]{V: 7, Valid: false}}
		require.NoError(t, db.Rows.Insert(ctx, e))

		n, err := db.Rows.Where(ant.IsNull(&f.V)).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)

		n, err = db.Rows.Where(ant.Eq(&f.V, sql.Null[int64]{V: 99})).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, n, "Eq(invalid Null) is Eq(NULL)")

		n, err = db.Rows.Where(ant.Ne(&f.V, sql.Null[int64]{})).Count(ctx)
		require.NoError(t, err)
		require.Zero(t, n)

		n, err = db.Rows.Where(ant.Eq(&f.V, sql.Null[int64]{V: 7, Valid: true})).Count(ctx)
		require.NoError(t, err)
		require.Zero(t, n)
	})
}

// In and NotIn with NULL members: Eq(nil) and Ne(nil) turn into IS [NOT] NULL.
// In/NotIn do not, which is the SQL meaning.
func TestProbeInWithNulls(t *testing.T) {
	knownHere(t)

	ddl := `CREATE TABLE ` + prTable[int64]("nrow") + ` (id BIGSERIAL PRIMARY KEY, p BIGINT)`
	fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[int64]) {
		ctx := t.Context()
		f := ant.Fields[prNRow[int64]]()
		one, two := int64(1), int64(2)
		require.NoError(
			t,
			db.NRows.Insert(
				ctx,
				&prNRow[int64]{},
				&prNRow[int64]{P: &one},
				&prNRow[int64]{P: &two},
			),
		)

		count := func(c ant.Cond) int64 {
			n, err := db.NRows.Where(c).Count(ctx)
			require.NoError(t, err)

			return n
		}

		if n := count(ant.In(&f.P, nil, &one)); n != 2 {
			t.Errorf(
				"In(P, nil, &1) matches %d rows; Eq(P, nil) matches NULL, so want 2 (NULL row and 1)",
				n,
			)
		}

		if n := count(ant.NotIn(&f.P, nil, &one)); n != 1 {
			t.Errorf("NotIn(P, nil, &1) matches %d rows; want 1 (only 2) like Ne(nil)", n)
		}

		if n := count(ant.NotIn(&f.P, &one)); n != 1 {
			t.Errorf(
				"NotIn(P, &1) matches %d rows; plain SQL gives 1 (the NULL row is not 'not in')",
				n,
			)
		}
	})
}

// ---------------------------------------------------------------------------
// named types, custom Valuer / Scanner types
// ---------------------------------------------------------------------------

type (
	prStatus  string
	prLevel   int16
	prBits    uint8
	prBig     uint64
	prCelsius float32
	prFlag    bool
)

// prMoney: value-receiver Valuer, pointer-receiver Scanner, text NUMERIC.
type prMoney struct{ Cents int64 }

func (m prMoney) Value() (driver.Value, error) {
	sign, c := "", m.Cents
	if c < 0 {
		sign, c = "-", -c
	}

	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100), nil
}

func (m *prMoney) Scan(src any) error {
	text, ok := src.(string)
	if !ok {
		return fmt.Errorf("prMoney: unexpected %T", src)
	}

	neg := strings.HasPrefix(text, "-")
	whole, frac, _ := strings.Cut(strings.TrimPrefix(text, "-"), ".")

	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return err
	}

	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return err
	}

	m.Cents = w*100 + f

	if neg {
		m.Cents = -m.Cents
	}

	return nil
}

// prTemp: BOTH methods on the pointer receiver.
type prTemp struct{ Milli int64 }

func (t *prTemp) Value() (driver.Value, error) {
	if t == nil {
		return nil, nil
	}

	return t.Milli, nil
}

func (t *prTemp) Scan(src any) error {
	n, ok := src.(int64)
	if !ok {
		return fmt.Errorf("prTemp: unexpected %T", src)
	}

	t.Milli = n

	return nil
}

// prOptional: stores 0 as NULL, and scans NULL back as 0.
type prOptional struct{ N int64 }

func (o prOptional) Value() (driver.Value, error) {
	if o.N == 0 {
		return nil, nil
	}

	return o.N, nil
}

func (o *prOptional) Scan(src any) error {
	if src == nil {
		o.N = 0

		return nil
	}

	n, ok := src.(int64)
	if !ok {
		return fmt.Errorf("prOptional: unexpected %T", src)
	}

	o.N = n

	return nil
}

// prUpper: a string that is stored upper-cased (a documented conversion).
type prUpper string

func (u prUpper) Value() (driver.Value, error) { return strings.ToUpper(string(u)), nil }

func (u *prUpper) Scan(src any) error {
	switch s := src.(type) {
	case string:
		*u = prUpper(s)
	case []byte:
		*u = prUpper(s)
	default:
		return fmt.Errorf("prUpper: unexpected %T", src)
	}

	return nil
}

// prColor: an int enum stored by name.
type prColor int

var prColorNames = []string{"red", "green", "blue"}

func (c prColor) Value() (driver.Value, error) {
	if int(c) < 0 || int(c) >= len(prColorNames) {
		return nil, fmt.Errorf("prColor: %d out of range", int(c))
	}

	return prColorNames[c], nil
}

func (c *prColor) Scan(src any) error {
	var name string

	switch s := src.(type) {
	case string:
		name = s
	case []byte:
		name = string(s)
	default:
		return fmt.Errorf("prColor: unexpected %T", src)
	}

	i := slices.Index(prColorNames, name)
	if i < 0 {
		return fmt.Errorf("prColor: unknown %q", name)
	}

	*c = prColor(i)

	return nil
}

// prTags: a slice type that is its own JSON column.
type prTags []string

func (t prTags) Value() (driver.Value, error) {
	if t == nil {
		return nil, nil
	}

	b, err := json.Marshal([]string(t))

	return string(b), err
}

func (t *prTags) Scan(src any) error {
	var b []byte

	switch s := src.(type) {
	case nil:
		*t = nil

		return nil
	case string:
		b = []byte(s)
	case []byte:
		b = s
	default:
		return fmt.Errorf("prTags: unexpected %T", src)
	}

	return json.Unmarshal(b, (*[]string)(t))
}

// prLine / prLines: a slice of STRUCTS that is its own JSON column.
type prLine struct {
	SKU string
	Qty int
}

type prLines []prLine

func (l prLines) Value() (driver.Value, error) {
	if l == nil {
		return nil, nil
	}

	b, err := json.Marshal([]prLine(l))

	return string(b), err
}

func (l *prLines) Scan(src any) error {
	var b []byte

	switch s := src.(type) {
	case nil:
		*l = nil

		return nil
	case string:
		b = []byte(s)
	case []byte:
		b = s
	default:
		return fmt.Errorf("prLines: unexpected %T", src)
	}

	return json.Unmarshal(b, (*[]prLine)(l))
}

func TestProbeNamedTypes(t *testing.T) {
	knownHere(t)

	probe(t, ord("named string", "TEXT", prStatus(""), "active", "archived"))
	probe(t, ord("named int16", "SMALLINT", prLevel(-5), 0, 5))
	probe(t, ord("named uint8", "SMALLINT", prBits(0), 1, 255))
	probe(t, ord("named uint64", "NUMERIC(20,0)", prBig(0), 5, math.MaxInt64, math.MaxUint64))
	probe(t, ord("named float32", "REAL", prCelsius(-40), 0, 36.6))

	probe(t, spec[prFlag]{
		name: "named bool",
		sql:  "BOOLEAN",
		vals: []prFlag{false, true},
		eq:   ant.Eq[prFlag],
		in:   ant.In[prFlag],
	})
}

func TestProbeCustomValuers(t *testing.T) {
	knownHere(t)

	probe(t, spec[prMoney]{
		name: "Money (value Valuer, pointer Scanner)",
		sql:  "NUMERIC(12,2)",
		vals: []prMoney{{-100050}, {-1}, {0}, {1}, {99}, {123456789}},
		eq:   ant.Eq[prMoney],
		in:   ant.In[prMoney],
	})

	probe(t, spec[prTemp]{
		name: "Temp (pointer Valuer and Scanner, non-pointer field)",
		sql:  "BIGINT",
		vals: []prTemp{{-5}, {0}, {7}},
		eq:   ant.Eq[prTemp],
		in:   ant.In[prTemp],
	})

	probe(t, spec[prOptional]{
		name:     "Optional (Valuer returns nil for zero)",
		sql:      "BIGINT",
		nullable: true,
		vals:     []prOptional{{0}, {1}, {9}},
		isNull:   func(o prOptional) bool { return o.N == 0 },
		eq:       ant.Eq[prOptional],
		in:       ant.In[prOptional],
	})

	probe(t, spec[prUpper]{
		name: "Upper (stored converted)",
		sql:  "TEXT",
		vals: []prUpper{"A", "b", "Cd"},
		norm: func(u prUpper) prUpper { return prUpper(strings.ToUpper(string(u))) },
		eq:   ant.Eq[prUpper],
		in:   ant.In[prUpper],
	})

	probe(t, spec[prColor]{
		name: "Color (int enum stored by name)",
		sql:  "TEXT",
		vals: []prColor{0, 1, 2},
		eq:   ant.Eq[prColor],
		in:   ant.In[prColor],
	})

	probe(t, spec[prTags]{
		name:     "Tags (slice with Valuer, nil is NULL)",
		sql:      "JSONB",
		nullable: true,
		vals:     []prTags{nil, {}, {"a"}, {"a", "b\"\\"}},
		isNull:   func(t prTags) bool { return t == nil },
	})

	probe(t, spec[prLines]{
		name:     "Lines (slice of structs with Valuer)",
		sql:      "JSONB",
		nullable: true,
		vals:     []prLines{nil, {}, {{"a", 1}}, {{"a", 1}, {"b", 2}}},
		isNull:   func(l prLines) bool { return l == nil },
		noPtr:    true,
	})
}

// ---------------------------------------------------------------------------
// bytes
// ---------------------------------------------------------------------------

type prBlob []byte

func TestProbeBytes(t *testing.T) {
	knownHere(t)

	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}

	probe(t, spec[[]byte]{
		name:     "[]byte (nil is NULL, empty is empty)",
		sql:      "BYTEA",
		nullable: true,
		vals: [][]byte{
			nil, {}, {0}, {0, 0, 0}, {0xff, 0xfe, 'a'}, []byte("it's \\x00 \"q\""), big,
		},
		isNull: func(b []byte) bool { return b == nil },
	})

	probe(t, spec[prBlob]{
		name:  "named []byte",
		sql:   "BYTEA",
		vals:  []prBlob{{}, {1}, {0, 255}},
		noPtr: true,
	})

	probe(t, spec[prBlob]{
		name:     "named []byte with NULL",
		sql:      "BYTEA",
		nullable: true,
		vals:     []prBlob{nil, {}, {1}, {0, 255}},
		isNull:   func(b prBlob) bool { return b == nil },
		noPtr:    true,
	})

	probe(t, spec[json.RawMessage]{
		name: "json.RawMessage in JSONB",
		sql:  "JSONB",
		vals: []json.RawMessage{
			json.RawMessage(`{}`),
			json.RawMessage(`[1, 2]`),
			json.RawMessage(`"x"`),
			json.RawMessage(`{"a": [true, null]}`),
			json.RawMessage(`null`),
		},
		noPtr: true,
	})

	probe(t, spec[json.RawMessage]{
		name:     "json.RawMessage with NULL",
		sql:      "JSONB",
		nullable: true,
		vals: []json.RawMessage{
			nil, json.RawMessage(`{}`), json.RawMessage(`[1, 2]`),
		},
		isNull: func(b json.RawMessage) bool { return b == nil },
		noPtr:  true,
	})

	probe(t, spec[string]{
		name: "string raw JSON text in JSONB",
		sql:  "JSONB",
		vals: []string{`{}`, `[1, 2]`, `"x"`, `{"a": [true, null]}`, `null`, `1.5`},
		same: func(a, b string) bool {
			var x, y any

			_ = json.Unmarshal([]byte(a), &x)
			_ = json.Unmarshal([]byte(b), &y)

			return reflect.DeepEqual(x, y)
		},
		noPtr: true,
	})
}

// ---------------------------------------------------------------------------
// ant.JSON and ant.Gob
// ---------------------------------------------------------------------------

type prPair struct {
	A int
	B string
}

type prDeep struct {
	Name   string              `json:"name"`
	Tags   []string            `json:"tags"`
	Inner  *prPair             `json:"inner"`
	Attrs  map[string]any      `json:"attrs,omitempty"`
	Counts map[string]int      `json:"counts"`
	Times  []time.Time         `json:"times"`
	Raw    json.RawMessage     `json:"raw"`
	Any    any                 `json:"any"`
	Nested map[string][]prDeep `json:"nested,omitempty"`
}

func TestProbeJSON(t *testing.T) {
	knownHere(t)

	probe(t, spec[ant.JSON[prPair]]{
		name: "ant.JSON[comparable struct]",
		sql:  "JSONB",
		vals: []ant.JSON[prPair]{
			{},
			{V: prPair{A: 1}},
			{V: prPair{A: 1, B: "x"}},
			{V: prPair{A: -1, B: "\u65e5\u672c \"q\" \\ \n <&>"}},
		},
		eq: ant.Eq[ant.JSON[prPair]],
		in: ant.In[ant.JSON[prPair]],
	})

	probe(t, spec[ant.JSON[map[string]any]]{
		name: "ant.JSON[map[string]any]",
		sql:  "JSONB",
		vals: []ant.JSON[map[string]any]{
			{},
			{V: map[string]any{}},
			{V: map[string]any{"a": nil}},
			{V: map[string]any{"": 1.0, "k": "v"}},
			{
				V: map[string]any{
					"a": 1.5,
					"b": []any{1.0, "x", nil, true, map[string]any{}},
					"c": map[string]any{"d": map[string]any{"e": []any{}}},
				},
			},
		},
	})

	probe(t, spec[ant.JSON[[]int]]{
		name: "ant.JSON[[]int] (nil vs empty)",
		sql:  "JSONB",
		vals: []ant.JSON[[]int]{{}, {V: []int{}}, {V: []int{1, 2}}},
	})

	probe(t, spec[ant.JSON[*prPair]]{
		name: "ant.JSON[*struct]",
		sql:  "JSONB",
		vals: []ant.JSON[*prPair]{{}, {V: &prPair{}}, {V: &prPair{A: 3, B: "z"}}},
	})

	probe(t, spec[ant.JSON[any]]{
		name: "ant.JSON[any]",
		sql:  "JSONB",
		vals: []ant.JSON[any]{
			{}, {V: 1.0}, {V: "s"}, {V: true}, {V: []any{}}, {V: map[string]any{"k": []any{1.0}}},
		},
	})

	probe(t, spec[ant.JSON[time.Time]]{
		name: "ant.JSON[time.Time]",
		sql:  "JSONB",
		vals: []ant.JSON[time.Time]{
			{},
			{V: time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC)},
			{V: time.Date(2024, 1, 2, 3, 4, 5, 0, time.FixedZone("", 3600))},
		},
		same: func(a, b ant.JSON[time.Time]) bool { return a.V.Equal(b.V) },
	})

	probe(t, spec[ant.JSON[int64]]{
		name: "ant.JSON[int64] extremes",
		sql:  "JSONB",
		vals: []ant.JSON[int64]{{V: math.MinInt64}, {V: 0}, {V: math.MaxInt64}},
		eq:   ant.Eq[ant.JSON[int64]],
		in:   ant.In[ant.JSON[int64]],
	})

	probe(t, spec[ant.JSON[prDeep]]{
		name: "ant.JSON[deep struct]",
		sql:  "JSONB",
		vals: []ant.JSON[prDeep]{
			{},
			{V: prDeep{Name: "n", Tags: []string{}, Counts: map[string]int{}}},
			{
				V: prDeep{
					Name:   "full",
					Tags:   []string{"a", "b"},
					Inner:  &prPair{1, "x"},
					Attrs:  map[string]any{"x": 1.0, "y": []any{"z"}},
					Counts: map[string]int{"a": 1, "b": 2},
					Times:  []time.Time{time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
					Raw:    json.RawMessage(`{"r":1}`),
					Any:    "s",
					Nested: map[string][]prDeep{"k": {{Name: "child", Counts: map[string]int{}}}},
				},
			},
		},
		same: func(a, b ant.JSON[prDeep]) bool {
			x, _ := json.Marshal(a.V)
			y, _ := json.Marshal(b.V)

			var xv, yv any

			_ = json.Unmarshal(x, &xv)
			_ = json.Unmarshal(y, &yv)

			return reflect.DeepEqual(xv, yv)
		},
	})

	probe(t, spec[ant.Gob[map[string]int]]{
		name: "ant.Gob[map]",
		sql:  "BYTEA",
		vals: []ant.Gob[map[string]int]{
			{V: map[string]int{}}, {V: map[string]int{"a": 1}}, {V: map[string]int{"": -1}},
		},
	})

	probe(t, spec[ant.Gob[prPair]]{
		name: "ant.Gob[struct]",
		sql:  "BYTEA",
		vals: []ant.Gob[prPair]{{}, {V: prPair{A: 1}}, {V: prPair{A: 2, B: "q"}}},
		eq:   ant.Eq[ant.Gob[prPair]],
	})

	probe(t, spec[ant.Gob[[]string]]{
		name: "ant.Gob[[]string]",
		sql:  "BYTEA",
		vals: []ant.Gob[[]string]{{V: []string{"a"}}, {V: []string{"", "b"}}},
	})
}

// A value JSON cannot encode, or that PostgreSQL's jsonb cannot hold, must be an
// error and leave nothing behind.
func TestProbeUnstorableValues(t *testing.T) {
	knownHere(t)

	check := func(t *testing.T, name string, insert func(ctx context.Context, db *prDB[ant.JSON[any]]) error) {
		ddl := `CREATE TABLE ` + prTable[ant.JSON[any]](
			"row",
		) + ` (id BIGSERIAL PRIMARY KEY, v JSONB NOT NULL)`

		t.Run(name, func(t *testing.T) {
			knownHere(t)

			fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[ant.JSON[any]]) {
				ctx := t.Context()

				err := insert(ctx, db)
				if err == nil {
					t.Errorf("no error for a value that cannot be stored")
				}

				n, cerr := db.Rows.Count(ctx)
				require.NoError(t, cerr)
				require.Zero(t, n, "a failed insert left rows behind")
			})
		})
	}

	one := func(v any) func(context.Context, *prDB[ant.JSON[any]]) error {
		return func(ctx context.Context, db *prDB[ant.JSON[any]]) error {
			return db.Rows.Insert(ctx, &prRow[ant.JSON[any]]{V: ant.JSON[any]{V: v}})
		}
	}

	check(t, "NaN in JSON", one(math.NaN()))
	check(t, "Inf in JSON", one(math.Inf(1)))
	check(t, "channel in JSON", one(make(chan int)))
	check(t, "NUL in JSON string (jsonb rejects \\u0000)", one("a\x00b"))
}

// ---------------------------------------------------------------------------
// third-party types: uuid, decimal, native enum
// ---------------------------------------------------------------------------

func TestProbeThirdParty(t *testing.T) {
	knownHere(t)

	u := func(s string) uuid.UUID { return uuid.MustParse(s) }

	probe(t, spec[uuid.UUID]{
		name: "uuid.UUID",
		sql:  "UUID",
		vals: []uuid.UUID{
			{},
			u("00000000-0000-0000-0000-000000000001"),
			u("7f000000-0000-4000-8000-000000000000"),
			u("ffffffff-ffff-ffff-ffff-ffffffffffff"),
		},
		eq: ant.Eq[uuid.UUID],
		in: ant.In[uuid.UUID],
	})

	probe(t, spec[uuid.NullUUID]{
		name:     "uuid.NullUUID",
		sql:      "UUID",
		nullable: true,
		vals: []uuid.NullUUID{
			{},
			{UUID: uuid.UUID{}, Valid: true},
			{UUID: u("7f000000-0000-4000-8000-000000000000"), Valid: true},
		},
		isNull: func(n uuid.NullUUID) bool { return !n.Valid },
		eq:     ant.Eq[uuid.NullUUID],
		in:     ant.In[uuid.NullUUID],
	})

	probe(t, ord(
		"string in a uuid column",
		"UUID",
		"00000000-0000-0000-0000-000000000000",
		"00000000-0000-0000-0000-000000000001",
		"7f000000-0000-4000-8000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	))

	d := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }

	probe(t, spec[decimal.Decimal]{
		name: "decimal.Decimal",
		sql:  "NUMERIC(40,10)",
		vals: []decimal.Decimal{
			d("-12345678901234567890.0123456789"),
			d("-0.0000000001"),
			d("0"),
			d("0.1"),
			d("1.5"),
			d("99999999999999999999.9999999999"),
		},
		same: func(a, b decimal.Decimal) bool { return a.Equal(b) },
		eq:   ant.Eq[decimal.Decimal],
		in:   ant.In[decimal.Decimal],
	})

	probe(t, spec[decimal.NullDecimal]{
		name:     "decimal.NullDecimal",
		sql:      "NUMERIC(20,4)",
		nullable: true,
		vals: []decimal.NullDecimal{
			{}, {Decimal: d("0"), Valid: true}, {Decimal: d("1.25"), Valid: true},
		},
		isNull: func(n decimal.NullDecimal) bool { return !n.Valid },
		same: func(a, b decimal.NullDecimal) bool {
			return a.Valid == b.Valid && a.Decimal.Equal(b.Decimal)
		},
		eq: ant.Eq[decimal.NullDecimal],
		in: ant.In[decimal.NullDecimal],
	})

	probe(t, spec[string]{
		name: "string in NUMERIC(10,2) (reads back as PG formats it)",
		sql:  "NUMERIC(10,2)",
		vals: []string{"-1.50", "0.00", "0.10", "3.14"},
		eq:   ant.Eq[string],
		in:   ant.In[string],
	})

	probe(t, spec[float64]{
		name: "float64 in NUMERIC(10,2)",
		sql:  "NUMERIC(10,2)",
		vals: []float64{-1.5, 0, 0.1, 3.14},
		eq:   ant.Eq[float64],
		in:   ant.In[float64],
		gt:   ant.Gt[float64],
	})

	probe(t, spec[int64]{
		name: "int64 in NUMERIC(30,0)",
		sql:  "NUMERIC(30,0)",
		vals: []int64{math.MinInt64, 0, math.MaxInt64},
		eq:   ant.Eq[int64],
		in:   ant.In[int64],
		gt:   ant.Gt[int64],
	})
}

const prEnumDDL = `CREATE TYPE pr_mood AS ENUM ('sad', 'ok', 'happy');
	CREATE TABLE pr_moods (id BIGSERIAL PRIMARY KEY, v pr_mood NOT NULL, p pr_mood)`

type prMood struct {
	ID int64
	V  prStatus
	P  *prStatus
}

type prMoodDB struct {
	ant.Context
	Moods ant.Table[prMood]
}

func TestProbeNativeEnum(t *testing.T) {
	knownHere(t)

	fixture.RunWith(t, prEnumDDL, func(t *testing.T, db *prMoodDB) {
		ctx := t.Context()
		f := ant.Fields[prMood]()
		ok := prStatus("ok")

		a, b, c := &prMood{V: "sad"}, &prMood{V: "ok", P: &ok}, &prMood{V: "happy", P: &ok}
		require.NoError(t, db.Moods.Insert(ctx, a, b, c))

		got, err := db.Moods.NoTracking().OrderBy(&f.ID).All(ctx)
		require.NoError(t, err)
		require.Len(t, got, 3)
		require.Equal(t, prStatus("happy"), got[2].V)
		require.Nil(t, got[0].P)
		require.Equal(t, ok, *got[1].P)

		n, err := db.Moods.Where(ant.Eq(&f.V, "ok")).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)

		n, err = db.Moods.Where(ant.In(&f.V, "sad", "happy")).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 2, n)

		n, err = db.Moods.Where(ant.Eq(&f.P, &ok)).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 2, n)

		n, err = db.Moods.Where(ant.IsNull(&f.P)).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)

		n, err = db.Moods.Where(ant.Gt(&f.V, "ok")).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, n, "enum order, not text order")

		cnt, err := db.Moods.Where(ant.Eq(&f.ID, a.ID)).
			Update(ctx, ant.Set(&f.V, "happy"), ant.Set(&f.P, &ok))
		require.NoError(t, err)
		require.EqualValues(t, 1, cnt)

		ok2, err := db.Moods.Upsert(ctx, &prMood{ID: a.ID, V: "ok"}, ant.OnConflict(&f.ID))
		require.NoError(t, err)
		require.True(t, ok2)

		n, err = db.Moods.Where(ant.Eq(&f.V, "ok"), ant.IsNull(&f.P)).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
}

// ---------------------------------------------------------------------------
// strings PostgreSQL cannot hold: an error, and no partial writes
// ---------------------------------------------------------------------------

func TestProbeBadStrings(t *testing.T) {
	knownHere(t)

	ddl := `CREATE TABLE ` + prTable[string]("row") + ` (id BIGSERIAL PRIMARY KEY, v TEXT NOT NULL)`

	bad := map[string]string{
		"NUL byte":             "a\x00b",
		"invalid UTF-8":        "ab\xff\xfecd",
		"lone surrogate bytes": "\xed\xa0\x80",
	}

	for name, value := range bad {
		t.Run(name, func(t *testing.T) {
			knownHere(t)

			fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[string]) {
				ctx := t.Context()
				f := ant.Fields[prRow[string]]()

				count := func() int64 {
					n, err := db.Rows.Count(ctx)
					require.NoError(t, err)

					return n
				}

				// Insert of a batch: nothing is written, and no key is left behind.
				a, b, c := &prRow[string]{
					V: "ok",
				}, &prRow[string]{
					V: value,
				}, &prRow[string]{
					V: "ok2",
				}

				if err := db.Rows.Insert(ctx, a, b, c); err == nil {
					t.Errorf("Insert accepted %q", value)
				}

				if n := count(); n != 0 {
					t.Errorf("a failed multi-row Insert left %d rows", n)
				}

				if a.ID != 0 || c.ID != 0 {
					t.Errorf(
						"failed Insert left keys on rolled-back entities: a=%d c=%d",
						a.ID,
						c.ID,
					)
				}

				// InsertMany: the same.
				rows := []*prRow[string]{{V: "1"}, {V: "2"}, {V: value}, {V: "4"}}
				if _, err := db.Rows.InsertMany(ctx, rows, ant.BatchSize(2)); err == nil {
					t.Errorf("InsertMany accepted %q", value)
				}

				if n := count(); n != 0 {
					t.Errorf("a failed InsertMany left %d rows", n)
				}

				for i, r := range rows {
					if r.ID != 0 {
						t.Errorf("failed InsertMany left key %d on row %d", r.ID, i)
					}
				}

				// Update and Upsert.
				ok := &prRow[string]{V: "fine"}
				require.NoError(t, db.Rows.Insert(ctx, ok))

				if _, err := db.Rows.Where(ant.Eq(&f.ID, ok.ID)).
					Update(ctx, ant.Set(&f.V, value)); err == nil {
					t.Errorf("Update accepted %q", value)
				}

				if _, err := db.Rows.Upsert(
					ctx,
					&prRow[string]{ID: ok.ID, V: value},
					ant.OnConflict(&f.ID),
				); err == nil {
					t.Errorf("Upsert accepted %q", value)
				}

				got, err := db.Rows.NoTracking().Find(ctx, ok.ID)
				require.NoError(t, err)
				require.Equal(t, "fine", got.V)

				// A condition with such a value is an error, or matches nothing: never a row.
				n, err := db.Rows.Where(ant.Eq(&f.V, value)).Count(ctx)
				if err == nil && n != 0 {
					t.Errorf("Eq(%q) matched %d rows", value, n)
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------
// bulk size limits
// ---------------------------------------------------------------------------

func TestProbeBigBatches(t *testing.T) {
	knownHere(t)

	if testing.Short() {
		t.Skip("inserts 140,000 rows")
	}

	ddl := `CREATE TABLE ` + prTable[int64](
		"row",
	) + ` (id BIGSERIAL PRIMARY KEY, v BIGINT NOT NULL)`
	fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[int64]) {
		ctx := t.Context()
		f := ant.Fields[prRow[int64]]()

		const total = 140_000

		rows := make([]*prRow[int64], total)
		for i := range rows {
			rows[i] = &prRow[int64]{V: int64(i)}
		}

		n, err := db.Rows.InsertMany(ctx, rows, ant.BatchSize(1<<30))
		require.NoError(t, err)
		require.EqualValues(t, total, n)

		seen := map[int64]bool{}

		for i, r := range rows {
			if r.ID == 0 || seen[r.ID] {
				t.Fatalf("row %d has key %d (zero or duplicate)", i, r.ID)
			}

			seen[r.ID] = true
		}

		sum, err := db.Rows.Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, total, sum)

		// Keys match their rows.
		for _, i := range []int{0, 1, 999, 1000, 65534, 65535, 65536, total - 1} {
			got, err := db.Rows.NoTracking().Find(ctx, rows[i].ID)
			require.NoError(t, err)
			require.EqualValues(t, i, got.V, "row %d got key %d", i, rows[i].ID)
		}

		t.Run("In with a huge list", func(t *testing.T) {
			knownHere(t)

			vals := make([]int64, 70_000)
			for i := range vals {
				vals[i] = int64(i)
			}

			c, err := db.Rows.Where(ant.In(&f.V, vals...)).Count(ctx)
			if err != nil {
				t.Errorf("In with %d values: %v", len(vals), err)

				return
			}

			if c != 70_000 {
				t.Errorf("In with %d values matched %d rows", len(vals), c)
			}
		})
	})
}

// ---------------------------------------------------------------------------
// embedded structs with prefixes
// ---------------------------------------------------------------------------

type prAddress struct {
	Street string
	City   string
	Zip    *string
	Geo    sql.Null[float64]
	Seen   time.Time
	Tags   prTags
}

type prCustomer struct {
	ID       int64
	Name     string    `ant:"unique"`
	Billing  prAddress `ant:"embedded,prefix=billing_"`
	Shipping prAddress `ant:"embedded,prefix=ship_"`
	Note     *string
}

type prCustomerDB struct {
	ant.Context
	Customers ant.Table[prCustomer]
}

const prCustomerDDL = `CREATE TABLE pr_customers (
	id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL UNIQUE,
	billing_street TEXT NOT NULL, billing_city TEXT NOT NULL, billing_zip TEXT,
	billing_geo DOUBLE PRECISION, billing_seen TIMESTAMPTZ NOT NULL, billing_tags JSONB,
	ship_street TEXT NOT NULL, ship_city TEXT NOT NULL, ship_zip TEXT,
	ship_geo DOUBLE PRECISION, ship_seen TIMESTAMPTZ NOT NULL, ship_tags JSONB,
	note TEXT)`

func TestProbeEmbeddedPrefixes(t *testing.T) {
	knownHere(t)

	fixture.RunTrackedWith(t, prCustomerDDL, func(t *testing.T, db *prCustomerDB) {
		ctx := t.Context()
		f := ant.Fields[prCustomer]()
		zip := "12345"
		seen := time.Date(2024, 3, 4, 5, 6, 7, 8000, time.FixedZone("z", 7200))

		a := &prCustomer{
			Name: "a",
			Billing: prAddress{
				Street: "b st",
				City:   "X",
				Zip:    &zip,
				Geo:    sql.Null[float64]{V: 1.5, Valid: true},
				Seen:   seen,
				Tags:   prTags{"t1"},
			},
			Shipping: prAddress{Street: "s st", City: "Y", Seen: seen},
		}

		b := &prCustomer{
			Name:    "b",
			Billing: prAddress{Street: "b2", City: "Y", Seen: seen},
			Shipping: prAddress{
				Street: "s2",
				City:   "X",
				Zip:    &zip,
				Seen:   seen,
				Tags:   prTags{},
			},
		}
		require.NoError(t, db.Customers.Insert(ctx, a, b))

		check := func(t *testing.T, want, got *prCustomer) {
			t.Helper()

			if want.Name != got.Name ||
				want.Billing.Street != got.Billing.Street || want.Billing.City != got.Billing.City ||
				want.Shipping.Street != got.Shipping.Street || want.Shipping.City != got.Shipping.City {
				t.Errorf(
					"columns mixed up: want %+v / %+v, got %+v / %+v",
					want.Billing,
					want.Shipping,
					got.Billing,
					got.Shipping,
				)
			}

			if (want.Billing.Zip == nil) != (got.Billing.Zip == nil) ||
				(want.Shipping.Zip == nil) != (got.Shipping.Zip == nil) {
				t.Errorf(
					"zip nil-ness: want %v/%v got %v/%v",
					want.Billing.Zip,
					want.Shipping.Zip,
					got.Billing.Zip,
					got.Shipping.Zip,
				)
			}

			if want.Billing.Geo != got.Billing.Geo || want.Shipping.Geo != got.Shipping.Geo {
				t.Errorf(
					"geo: want %v/%v got %v/%v",
					want.Billing.Geo,
					want.Shipping.Geo,
					got.Billing.Geo,
					got.Shipping.Geo,
				)
			}

			if !want.Billing.Seen.Equal(got.Billing.Seen) ||
				!want.Shipping.Seen.Equal(got.Shipping.Seen) {
				t.Errorf(
					"seen: want %v/%v got %v/%v",
					want.Billing.Seen,
					want.Shipping.Seen,
					got.Billing.Seen,
					got.Shipping.Seen,
				)
			}

			if !reflect.DeepEqual(want.Billing.Tags, got.Billing.Tags) ||
				!reflect.DeepEqual(want.Shipping.Tags, got.Shipping.Tags) {
				t.Errorf(
					"tags: want %#v/%#v got %#v/%#v",
					want.Billing.Tags,
					want.Shipping.Tags,
					got.Billing.Tags,
					got.Shipping.Tags,
				)
			}
		}

		got, err := db.Customers.NoTracking().Find(ctx, a.ID)
		require.NoError(t, err)
		check(t, a, got)

		got, err = db.Customers.NoTracking().Find(ctx, b.ID)
		require.NoError(t, err)
		check(t, b, got)

		count := func(c ...ant.Cond) int64 {
			n, err := db.Customers.Where(c...).Count(ctx)
			require.NoError(t, err)

			return n
		}

		for name, tc := range map[string]struct {
			cond ant.Cond
			want int64
		}{
			"Billing.City=X":     {ant.Eq(&f.Billing.City, "X"), 1},
			"Shipping.City=X":    {ant.Eq(&f.Shipping.City, "X"), 1},
			"City in both":       {ant.In(&f.Billing.City, "X", "Y"), 2},
			"Billing.Zip null":   {ant.IsNull(&f.Billing.Zip), 1},
			"Shipping.Zip null":  {ant.IsNull(&f.Shipping.Zip), 1},
			"Billing.Zip=&zip":   {ant.Eq(&f.Billing.Zip, &zip), 1},
			"Shipping.Zip=&zip":  {ant.Eq(&f.Shipping.Zip, &zip), 1},
			"Billing.Geo valid":  {ant.Eq(&f.Billing.Geo, sql.Null[float64]{V: 1.5, Valid: true}), 1},
			"Shipping.Geo null":  {ant.Eq(&f.Shipping.Geo, sql.Null[float64]{}), 2},
			"Billing.Seen":       {ant.Eq(&f.Billing.Seen, seen.UTC()), 2},
			"Billing.Seen in":    {ant.In(&f.Billing.Seen, seen), 2},
			"Billing.Seen gt":    {ant.Gt(&f.Billing.Seen, seen.Add(-time.Second)), 2},
			"Billing.Seen gt no": {ant.Gt(&f.Billing.Seen, seen.Add(time.Second)), 0},
		} {
			if n := count(tc.cond); n != tc.want {
				t.Errorf("%s: %d rows, want %d", name, n, tc.want)
			}
		}

		// Bulk update of one prefix leaves the other alone.
		n, err := db.Customers.Where(ant.Eq(&f.ID, a.ID)).
			Update(ctx, ant.Set(&f.Billing.City, "Z"), ant.Set(&f.Shipping.Zip, &zip), ant.Set(&f.Billing.Zip, nil))
		require.NoError(t, err)
		require.EqualValues(t, 1, n)

		got, err = db.Customers.NoTracking().Find(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, "Z", got.Billing.City)
		require.Equal(t, "Y", got.Shipping.City)
		require.Nil(t, got.Billing.Zip)
		require.NotNil(t, got.Shipping.Zip)

		// Change tracking sees a change in one nested field only.
		db.ClearTracked()
		tracked, err := db.Customers.Find(ctx, b.ID)
		require.NoError(t, err)
		require.False(t, db.HasChanges())

		tracked.Shipping.Tags = append(tracked.Shipping.Tags, "added")
		tracked.Billing.Zip = &zip

		require.True(t, db.HasChanges())
		require.NoError(t, db.SaveChanges(ctx))

		got, err = db.Customers.NoTracking().Find(ctx, b.ID)
		require.NoError(t, err)
		require.Equal(t, prTags{"added"}, got.Shipping.Tags)
		require.Equal(t, &zip, got.Billing.Zip)
		require.Equal(t, "Y", got.Billing.City)

		// Upsert by the unique name.
		up := &prCustomer{
			Name:    "a",
			Billing: prAddress{Street: "up", City: "U", Seen: seen},
			Shipping: prAddress{
				Street: "up2",
				City:   "V",
				Zip:    &zip,
				Seen:   seen,
			},
		}
		want := *up
		ok, err := db.Customers.Upsert(ctx, up, ant.OnConflict(&f.Name))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, a.ID, up.ID, "Upsert returns the stored key")
		check(t, &want, up)

		got, err = db.Customers.NoTracking().Find(ctx, a.ID)
		require.NoError(t, err)
		check(t, &want, got)

		// InsertMany.
		many := []*prCustomer{
			{
				Name:    "m1",
				Billing: prAddress{Street: "1", City: "1", Seen: seen},
				Shipping: prAddress{
					Street: "1",
					City:   "1",
					Seen:   seen,
					Zip:    &zip,
				},
			},
			{
				Name: "m2",
				Billing: prAddress{
					Street: "2",
					City:   "2",
					Seen:   seen,
					Zip:    &zip,
					Tags:   prTags{"x"},
				},
				Shipping: prAddress{Street: "2", City: "2", Seen: seen},
			},
		}
		_, err = db.Customers.InsertMany(ctx, many)
		require.NoError(t, err)

		for _, m := range many {
			got, err = db.Customers.NoTracking().Find(ctx, m.ID)
			require.NoError(t, err)
			check(t, m, got)
		}
	})
}

// An embedded pointer to a struct: either a clear error or support, never a panic.
type prPtrEmbedded struct {
	ID   int64
	Addr *prAddress `ant:"embedded,prefix=addr_"`
}

type prPtrEmbeddedDB struct {
	ant.Context
	Rows ant.Table[prPtrEmbedded]
}

func TestProbeEmbeddedPointer(t *testing.T) {
	knownHere(t)

	fixture.Each(t, func(t *testing.T, target fixture.Target) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("PANIC: %v\n%s", r, debug.Stack())
			}
		}()

		db, err := ant.Open[prPtrEmbeddedDB](t.Context(), target.Dialect, target.DSN)
		if err == nil {
			db.Close()
		}

		t.Logf("Open: %v", err)
	})
}

// ---------------------------------------------------------------------------
// keys: uuid strings, uuid.UUID, big unsigned, small, negative
// ---------------------------------------------------------------------------

type prUUIDKey struct {
	ID   string
	Name string `ant:"unique"`
	Pad  *string
}

func (*prUUIDKey) Configure(b *ant.ModelBuilder[prUUIDKey]) {
	k := ant.Fields[prUUIDKey]()
	ant.GenerateKey(b, &k.ID, ids.UUID)
}

type prGoogleKey struct {
	ID   uuid.UUID
	Name string `ant:"unique"`
	Note string
}

func (*prGoogleKey) Configure(b *ant.ModelBuilder[prGoogleKey]) {
	k := ant.Fields[prGoogleKey]()
	ant.GenerateKey(b, &k.ID, uuid.New)
}

type prBigKey struct {
	ID   uint64
	Name string
}

type prSmallKey struct {
	ID   int32
	Name string
}

type prNamedKey struct {
	ID   prLevel
	Name string
}

type prKeysDB struct {
	ant.Context
	UUIDs   ant.Table[prUUIDKey]
	Googles ant.Table[prGoogleKey]
	Bigs    ant.Table[prBigKey]
	Smalls  ant.Table[prSmallKey]
	Named   ant.Table[prNamedKey]
}

const prKeysDDL = `
	CREATE TABLE pr_uuid_keys (id UUID PRIMARY KEY, name TEXT NOT NULL UNIQUE, pad TEXT);
	CREATE TABLE pr_google_keys (id UUID PRIMARY KEY, name TEXT NOT NULL UNIQUE, note TEXT NOT NULL DEFAULT '');
	CREATE TABLE pr_big_keys (id NUMERIC(20,0) PRIMARY KEY, name TEXT NOT NULL);
	CREATE TABLE pr_small_keys (id SERIAL PRIMARY KEY, name TEXT NOT NULL);
	CREATE TABLE pr_named_keys (id SMALLSERIAL PRIMARY KEY, name TEXT NOT NULL)`

func TestProbeKeys(t *testing.T) {
	knownHere(t)

	fixture.RunTrackedWith(t, prKeysDDL, func(t *testing.T, db *prKeysDB) {
		ctx := t.Context()

		t.Run("string uuid generated", func(t *testing.T) {
			knownHere(t)

			k := ant.Fields[prUUIDKey]()
			a, b := &prUUIDKey{Name: "a"}, &prUUIDKey{Name: "b"}
			require.NoError(t, db.UUIDs.Insert(ctx, a, b))
			require.NotEmpty(t, a.ID)
			require.NotEqual(t, a.ID, b.ID)

			got, err := db.UUIDs.NoTracking().Find(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, "a", got.Name)

			many := []*prUUIDKey{
				{Name: "m1"},
				{Name: "m2"},
				{ID: "00000000-0000-4000-8000-000000000001", Name: "m3"},
			}
			_, err = db.UUIDs.InsertMany(ctx, many)
			require.NoError(t, err)

			for _, m := range many {
				require.NotEmpty(t, m.ID, "InsertMany did not generate a key for %s", m.Name)
			}

			require.Equal(t, "00000000-0000-4000-8000-000000000001", many[2].ID)

			n, err := db.UUIDs.Where(ant.In(&k.ID, a.ID, b.ID, many[0].ID)).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 3, n)

			// Upsert by name with no key: the stored key comes back.
			up := &prUUIDKey{Name: "a", Pad: new("padded")}
			ok, err := db.UUIDs.Upsert(ctx, up, ant.OnConflict(&k.Name))
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, a.ID, up.ID, "Upsert returns the stored row's key")

			total, err := db.UUIDs.Where(ant.Eq(&k.Name, "a")).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)

			// InsertMany with a conflict on name.
			again := []*prUUIDKey{{Name: "a", Pad: new("again")}, {Name: "fresh"}}
			_, err = db.UUIDs.InsertMany(ctx, again, ant.WithConflict(ant.OnConflict(&k.Name)))
			require.NoError(t, err)

			got, err = db.UUIDs.NoTracking().Find(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, "again", *got.Pad)

			require.NoError(t, db.UUIDs.DeleteByID(ctx, a.ID))
		})

		t.Run("uuid.UUID generated", func(t *testing.T) {
			knownHere(t)

			k := ant.Fields[prGoogleKey]()
			a := &prGoogleKey{Name: "a"}
			require.NoError(t, db.Googles.Insert(ctx, a))
			require.NotEqual(t, uuid.Nil, a.ID)

			got, err := db.Googles.NoTracking().Find(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, a.ID, got.ID)

			n, err := db.Googles.Where(ant.Eq(&k.ID, a.ID)).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)

			n, err = db.Googles.Where(ant.In(&k.ID, a.ID, uuid.Nil)).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)

			up := &prGoogleKey{Name: "a"}
			ok, err := db.Googles.Upsert(ctx, up, ant.OnConflict(&k.Name))
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, a.ID, up.ID)

			many := []*prGoogleKey{{Name: "m1"}, {Name: "m2"}}
			_, err = db.Googles.InsertMany(ctx, many)
			require.NoError(t, err)
			require.NotEqual(t, many[0].ID, many[1].ID)
			require.NotEqual(t, uuid.Nil, many[0].ID)
		})

		t.Run("uint64 key above int64", func(t *testing.T) {
			knownHere(t)

			big := &prBigKey{ID: math.MaxUint64, Name: "max"}
			require.NoError(t, db.Bigs.Insert(ctx, big))

			got, err := db.Bigs.NoTracking().Find(ctx, uint64(math.MaxUint64))
			require.NoError(t, err)
			require.Equal(t, "max", got.Name)

			ok, err := db.Bigs.Upsert(
				ctx,
				&prBigKey{ID: math.MaxUint64, Name: "again"},
				ant.OnConflict(&ant.Fields[prBigKey]().ID),
			)
			require.NoError(t, err)
			require.True(t, ok)

			got, err = db.Bigs.NoTracking().Find(ctx, uint64(math.MaxUint64))
			require.NoError(t, err)
			require.Equal(t, "again", got.Name)

			gen := &prBigKey{Name: "generated"}
			err = db.Bigs.Insert(ctx, gen)
			t.Logf("zero uint64 key on a NUMERIC column with no default: err=%v id=%d", err, gen.ID)
		})

		t.Run("int32 and named int16 generated keys", func(t *testing.T) {
			knownHere(t)

			s := &prSmallKey{Name: "s"}
			require.NoError(t, db.Smalls.Insert(ctx, s))
			require.NotZero(t, s.ID)

			many := []*prSmallKey{{Name: "a"}, {Name: "b"}, {Name: "c"}}
			_, err := db.Smalls.InsertMany(ctx, many)
			require.NoError(t, err)
			require.Less(t, many[0].ID, many[1].ID)
			require.Less(t, many[1].ID, many[2].ID)

			n := &prNamedKey{Name: "n"}
			require.NoError(t, db.Named.Insert(ctx, n))
			require.NotZero(t, n.ID)

			got, err := db.Named.NoTracking().Find(ctx, n.ID)
			require.NoError(t, err)
			require.Equal(t, "n", got.Name)

			nm := []*prNamedKey{{Name: "x"}, {Name: "y"}}
			_, err = db.Named.InsertMany(ctx, nm)
			require.NoError(t, err)
			require.NotZero(t, nm[0].ID)
			require.NotEqual(t, nm[0].ID, nm[1].ID)
		})
	})
}

// Extreme explicit keys.
func TestProbeExtremeKeys(t *testing.T) {
	knownHere(t)

	ddl := `CREATE TABLE ` + prTable[string]("row") + ` (id BIGINT PRIMARY KEY, v TEXT NOT NULL)`
	fixture.RunTrackedWith(t, ddl, func(t *testing.T, db *prDB[string]) {
		ctx := t.Context()

		for _, id := range []int64{math.MinInt64, -1, 1, math.MaxInt64} {
			e := &prRow[string]{ID: id, V: fmt.Sprint(id)}
			require.NoError(t, db.Rows.Insert(ctx, e), "key %d", id)

			got, err := db.Rows.Find(ctx, id)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprint(id), got.V)

			got.V += "!"

			require.NoError(t, db.SaveChanges(ctx), "SaveChanges key %d", id)

			again, err := db.Rows.NoTracking().Find(ctx, id)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprint(id)+"!", again.V)
			require.NoError(t, db.Rows.DeleteByID(ctx, id))
		}
	})
}

// ---------------------------------------------------------------------------
// zero values, defaults, and timestamps
// ---------------------------------------------------------------------------

type prDefaulted struct {
	ID     int64
	Name   string
	Active bool      `ant:"default=true"`
	Score  int       `ant:"default=7"`
	Label  *string   `ant:"default='dflt'"`
	Seen   time.Time `ant:"default=now()"`
}

type prDefaultedDB struct {
	ant.Context
	Rows ant.Table[prDefaulted]
}

const prDefaultedDDL = `CREATE TABLE pr_defaulteds (
	id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL UNIQUE,
	active BOOLEAN NOT NULL DEFAULT TRUE, score INT NOT NULL DEFAULT 7,
	label TEXT DEFAULT 'dflt', seen TIMESTAMPTZ NOT NULL DEFAULT now())`

// Documented: a zero field with a default is left for the database. The three
// ways to insert must agree, and SaveChanges / Set must still write zero.
func TestProbeDefaults(t *testing.T) {
	knownHere(t)

	fixture.RunTrackedWith(t, prDefaultedDDL, func(t *testing.T, db *prDefaultedDB) {
		ctx := t.Context()
		f := ant.Fields[prDefaulted]()
		empty := ""

		read := func(name string) *prDefaulted {
			got, err := db.Rows.NoTracking().Where(ant.Eq(&f.Name, name)).Single(ctx)
			require.NoError(t, err)

			return got
		}

		one := &prDefaulted{Name: "one"}
		require.NoError(t, db.Rows.Insert(ctx, one))

		if !one.Active || one.Score != 7 || one.Label == nil || *one.Label != "dflt" ||
			one.Seen.IsZero() {
			t.Errorf("Insert did not copy the defaults back: %+v", one)
		}

		many := []*prDefaulted{{Name: "many"}, {Name: "many2", Score: 3, Label: &empty}}
		_, err := db.Rows.InsertMany(ctx, many)
		require.NoError(t, err)

		got := read("many")
		if !got.Active || got.Score != 7 || got.Label == nil || *got.Label != "dflt" {
			t.Errorf("InsertMany zero fields: %+v (want the defaults, as Insert does)", got)
		}

		got = read("many2")
		if got.Score != 3 || got.Label == nil || *got.Label != "" {
			t.Errorf("InsertMany explicit fields: %+v", got)
		}

		up := &prDefaulted{Name: "up"}
		ok, err := db.Rows.Upsert(ctx, up, ant.OnConflict(&f.Name))
		require.NoError(t, err)
		require.True(t, ok)

		if !up.Active || up.Score != 7 || up.Label == nil {
			t.Errorf("Upsert did not copy the defaults back: %+v", up)
		}

		// Zero values written by SaveChanges and Set are real zeros.
		tracked, err := db.Rows.Find(ctx, one.ID)
		require.NoError(t, err)

		tracked.Active, tracked.Score, tracked.Label = false, 0, nil

		require.NoError(t, db.SaveChanges(ctx))

		got = read("one")
		if got.Active || got.Score != 0 || got.Label != nil {
			t.Errorf("SaveChanges of zero values: %+v", got)
		}

		_, err = db.Rows.Where(ant.Eq(&f.Name, "many")).
			Update(ctx, ant.Set(&f.Active, false), ant.Set(&f.Score, 0), ant.Set(&f.Label, (*string)(nil)))
		require.NoError(t, err)

		got = read("many")
		if got.Active || got.Score != 0 || got.Label != nil {
			t.Errorf("bulk Set of zero values: %+v", got)
		}

		// Upserting the zero value over an existing row with explicit values.
		over := &prDefaulted{Name: "many2"}
		ok, err = db.Rows.Upsert(ctx, over, ant.OnConflict(&f.Name))
		require.NoError(t, err)
		require.True(t, ok)

		t.Logf(
			"Upsert{Name only} over {Score:3, Label:\"\"}: score=%d label=%v (zero = left for the database => default 7/'dflt')",
			over.Score,
			over.Label,
		)
	})
}

// Explicit CreatedAt / UpdatedAt are kept; zero ones are stamped; times survive.
type prStamped struct {
	ID        int64
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type prStampedDB struct {
	ant.Context
	Rows ant.Table[prStamped]
}

func TestProbeTimestamps(t *testing.T) {
	knownHere(t)

	const ddl = `CREATE TABLE pr_stampeds (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`

	fixture.RunTrackedWith(t, ddl, func(t *testing.T, db *prStampedDB) {
		ctx := t.Context()
		f := ant.Fields[prStamped]()
		past := time.Date(2001, 2, 3, 4, 5, 6, 0, time.FixedZone("p", -3*3600))

		explicit := &prStamped{Name: "explicit", CreatedAt: past, UpdatedAt: past}
		require.NoError(t, db.Rows.Insert(ctx, explicit))

		got, err := db.Rows.NoTracking().Find(ctx, explicit.ID)
		require.NoError(t, err)

		if !got.CreatedAt.Equal(past) {
			t.Errorf("explicit CreatedAt %v stored as %v", past, got.CreatedAt)
		}

		if !got.UpdatedAt.Equal(past) {
			t.Errorf("explicit UpdatedAt %v stored as %v", past, got.UpdatedAt)
		}

		stamped := &prStamped{Name: "stamped"}
		require.NoError(t, db.Rows.Insert(ctx, stamped))
		require.WithinDuration(t, time.Now(), stamped.CreatedAt, time.Minute)

		got, err = db.Rows.NoTracking().Find(ctx, stamped.ID)
		require.NoError(t, err)

		if !got.CreatedAt.Equal(stamped.CreatedAt.Truncate(time.Microsecond)) {
			t.Errorf("stamped CreatedAt %v read back as %v", stamped.CreatedAt, got.CreatedAt)
		}

		// A condition on the entity's own (nanosecond) timestamp finds its row.
		n, err := db.Rows.Where(ant.Eq(&f.CreatedAt, stamped.CreatedAt)).Count(ctx)
		require.NoError(t, err)

		if n != 1 {
			t.Errorf("Eq(CreatedAt, entity.CreatedAt) found %d rows, want 1", n)
		}

		// The same instant in another zone matches.
		n, err = db.Rows.Where(ant.Eq(&f.CreatedAt, past.UTC())).Count(ctx)
		require.NoError(t, err)

		if n != 1 {
			t.Errorf("Eq(same instant, UTC) found %d rows, want 1", n)
		}

		n, err = db.Rows.Where(ant.Between(&f.CreatedAt, past.Add(-time.Second), past.Add(time.Second))).
			Count(ctx)
		require.NoError(t, err)

		if n != 1 {
			t.Errorf("Between around %v found %d rows, want 1", past, n)
		}

		// A bulk update stamps UpdatedAt only.
		_, err = db.Rows.Where(ant.Eq(&f.ID, explicit.ID)).Update(ctx, ant.Set(&f.Name, "renamed"))
		require.NoError(t, err)

		got, err = db.Rows.NoTracking().Find(ctx, explicit.ID)
		require.NoError(t, err)

		if !got.CreatedAt.Equal(past) {
			t.Errorf("bulk Update changed CreatedAt to %v", got.CreatedAt)
		}

		if got.UpdatedAt.Equal(past) {
			t.Errorf("bulk Update did not touch UpdatedAt")
		}

		// A tracked change stamps UpdatedAt, and the entity shows it.
		tracked, err := db.Rows.Find(ctx, stamped.ID)
		require.NoError(t, err)

		before := tracked.UpdatedAt
		tracked.Name = "stamped2"

		require.NoError(t, db.SaveChanges(ctx))

		if !tracked.UpdatedAt.After(before) {
			t.Errorf("SaveChanges did not advance UpdatedAt: %v -> %v", before, tracked.UpdatedAt)
		}

		if db.HasChanges() {
			t.Errorf("HasChanges after SaveChanges: %s", db.DebugTracked())
		}

		// Upsert with explicit timestamps overwrites them (UpdatedAt) as written.
		ok, err := db.Rows.Upsert(
			ctx,
			&prStamped{Name: "stamped2", CreatedAt: past, UpdatedAt: past},
			ant.OnConflict(&f.Name),
		)
		require.NoError(t, err)
		require.True(t, ok)

		got, err = db.Rows.NoTracking().Find(ctx, stamped.ID)
		require.NoError(t, err)

		t.Logf(
			"Upsert with explicit past stamps over a row: created=%v updated=%v",
			got.CreatedAt,
			got.UpdatedAt,
		)
	})
}

// A condition and a write with the same time must agree whatever the zone.
func TestProbeTimeZones(t *testing.T) {
	knownHere(t)

	ddl := `CREATE TABLE ` + prTable[time.Time](
		"row",
	) + ` (id BIGSERIAL PRIMARY KEY, v TIMESTAMPTZ NOT NULL)`

	zones := []*time.Location{
		time.UTC,
		time.FixedZone("p", 14*3600),
		time.FixedZone("m", -12*3600),
		time.FixedZone("odd", 5*3600+45*60+30),
		time.Local,
	}

	if ny, err := time.LoadLocation("America/New_York"); err == nil {
		zones = append(zones, ny)
	}

	fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[time.Time]) {
		ctx := t.Context()
		f := ant.Fields[prRow[time.Time]]()

		base := time.Date(
			2024,
			11,
			3,
			1,
			30,
			0,
			500_000_000,
			time.UTC,
		) // inside the NY fall-back hour

		e := &prRow[time.Time]{V: base}
		require.NoError(t, db.Rows.Insert(ctx, e))

		for _, z := range zones {
			in := base.In(z)

			n, err := db.Rows.Where(ant.Eq(&f.V, in)).Count(ctx)
			require.NoError(t, err)

			if n != 1 {
				t.Errorf("Eq(%v) found %d rows, want 1", in, n)
			}

			n, err = db.Rows.Where(ant.In(&f.V, in)).Count(ctx)
			require.NoError(t, err)

			if n != 1 {
				t.Errorf("In(%v) found %d rows, want 1", in, n)
			}

			// A wall-clock time in the zone, built with time.Date, round-trips as an instant.
			wall := time.Date(2024, 11, 3, 1, 30, 0, 0, z)
			w := &prRow[time.Time]{V: wall}
			require.NoError(t, db.Rows.Insert(ctx, w))

			got, err := db.Rows.NoTracking().Find(ctx, w.ID)
			require.NoError(t, err)

			if !got.V.Equal(wall) {
				t.Errorf("wall clock in %v: wrote %v, read %v", z, wall, got.V)
			}
		}
	})
}

// A Go time in the far past or future that PostgreSQL stores fine must come back.
func TestProbeTimeLimits(t *testing.T) {
	knownHere(t)

	ddl := `CREATE TABLE ` + prTable[time.Time](
		"row",
	) + ` (id BIGSERIAL PRIMARY KEY, v TIMESTAMPTZ NOT NULL)`

	fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[time.Time]) {
		ctx := t.Context()

		for _, v := range []time.Time{
			time.Date(-4000, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(-1, 12, 31, 23, 59, 59, 0, time.UTC),
			time.Date(1, 1, 1, 0, 0, 0, 1000, time.UTC),
			time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(200000, 6, 7, 8, 9, 10, 0, time.UTC),
			time.Unix(1<<40, 0).UTC(),
		} {
			e := &prRow[time.Time]{V: v}
			if err := db.Rows.Insert(ctx, e); err != nil {
				t.Errorf("Insert %v: %v", v, err)

				continue
			}

			got, err := db.Rows.NoTracking().Find(ctx, e.ID)
			if err != nil {
				t.Errorf("Find %v: %v", v, err)

				continue
			}

			if !got.V.Equal(v) {
				t.Errorf("wrote %v, read %v", v, got.V)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// postgres.Array, postgres.Point, other native types
// ---------------------------------------------------------------------------

func arrayNullable[E string | int | int32 | int64 | float64 | bool](
	name, sqlType string,
	same func(a, b postgres.Array[E]) bool,
	vals []postgres.Array[E],
) spec[postgres.Array[E]] {
	return spec[postgres.Array[E]]{
		name:     name,
		sql:      sqlType,
		nullable: true,
		vals:     vals,
		same:     same,
		isNull:   func(a postgres.Array[E]) bool { return a == nil },
	}
}

func TestProbeArrays(t *testing.T) {
	knownHere(t)

	probe(t, arrayNullable("postgres.Array[string]", "TEXT[]", nil, []postgres.Array[string]{
		nil,
		{},
		{""},
		{"", ""},
		{"a", "b"},
		{
			"NULL",
			"null",
			"a,b",
			"{x}",
			`\`,
			`"`,
			`\"`,
			"  sp  ",
			"\u65e5\u672c",
			"tab\t",
			"nl\n",
			"'",
			"x y,z",
		},
	}))

	probe(t, arrayNullable("postgres.Array[int]", "BIGINT[]", nil, []postgres.Array[int]{
		nil, {}, {0}, {-1, 1}, {math.MinInt64, math.MaxInt64},
	}))

	probe(t, arrayNullable("postgres.Array[int32]", "INTEGER[]", nil, []postgres.Array[int32]{
		nil, {}, {math.MinInt32, math.MaxInt32},
	}))

	probe(t, arrayNullable("postgres.Array[int64]", "BIGINT[]", nil, []postgres.Array[int64]{
		nil, {}, {math.MinInt64, 0, math.MaxInt64},
	}))

	probe(t, arrayNullable("postgres.Array[bool]", "BOOLEAN[]", nil, []postgres.Array[bool]{
		nil, {}, {true}, {false, true, false},
	}))

	probe(t, arrayNullable(
		"postgres.Array[float64]",
		"DOUBLE PRECISION[]",
		func(a, b postgres.Array[float64]) bool {
			return (a == nil) == (b == nil) && slices.EqualFunc(a, b, floatSame[float64])
		},
		[]postgres.Array[float64]{
			nil,
			{},
			{0, 0.1, -1.5},
			{math.Inf(-1), math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.NaN()},
		},
	))

	probe(t, spec[postgres.Point]{
		name: "postgres.Point extremes",
		sql:  "POINT",
		vals: []postgres.Point{
			{},
			{X: 1.5, Y: -2.5},
			{X: math.MaxFloat64, Y: -math.MaxFloat64},
			{X: math.SmallestNonzeroFloat64, Y: 1e-300},
			{X: 1e21, Y: 123456789.123456789},
		},
		noEq: true,
	})

	probe(t, spec[postgres.Point]{
		name: "postgres.Point with Inf and NaN",
		sql:  "POINT",
		vals: []postgres.Point{{X: math.Inf(1), Y: math.Inf(-1)}, {X: math.NaN(), Y: 1}},
		same: func(a, b postgres.Point) bool {
			return floatSame(a.X, b.X) && floatSame(a.Y, b.Y)
		},
		noPtr: true,
	})
}

func TestProbeOtherNative(t *testing.T) {
	knownHere(t)

	probe(t, spec[time.Time]{
		name: "time.Time in DATE",
		sql:  "DATE",
		vals: []time.Time{
			time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
			time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		},
		same: timeSame,
		eq:   ant.Eq[time.Time],
		in:   ant.In[time.Time],
		gt:   ant.Gt[time.Time],
	})

	probe(t, spec[time.Duration]{
		name:  "time.Duration in INTERVAL",
		sql:   "INTERVAL",
		vals:  []time.Duration{-time.Hour, 0, time.Microsecond, 36 * time.Hour},
		eq:    ant.Eq[time.Duration],
		in:    ant.In[time.Duration],
		noPtr: true,
	})

	probe(t, spec[float32]{
		name: "float32 in DOUBLE PRECISION",
		sql:  "DOUBLE PRECISION",
		vals: []float32{-1.5, 0, 0.1, 16777217},
		eq:   ant.Eq[float32],
		in:   ant.In[float32],
		gt:   ant.Gt[float32],
	})

	probe(t, spec[float64]{
		name: "float64 in REAL (lossy by design)",
		sql:  "REAL",
		vals: []float64{-1.5, 0, 0.5, 1e10},
		eq:   ant.Eq[float64],
		in:   ant.In[float64],
		gt:   ant.Gt[float64],
	})

	probe(t, spec[netip.Addr]{
		name: "netip.Addr in INET",
		sql:  "INET",
		vals: []netip.Addr{
			netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("::1"),
		},
		eq:    ant.Eq[netip.Addr],
		in:    ant.In[netip.Addr],
		noPtr: true,
	})

	probe(t, spec[net.IP]{
		name:     "net.IP in INET",
		sql:      "INET",
		nullable: true,
		vals:     []net.IP{nil, net.ParseIP("10.0.0.1").To4()},
		same:     func(a, b net.IP) bool { return a.Equal(b) },
		isNull:   func(ip net.IP) bool { return ip == nil },
		noPtr:    true,
	})

	probe(t, spec[map[string]any]{
		name:     "map[string]any in JSONB (no ant.JSON)",
		sql:      "JSONB",
		nullable: true,
		vals:     []map[string]any{nil, {}, {"a": 1.0, "b": []any{"x"}}},
		isNull:   func(m map[string]any) bool { return m == nil },
		noPtr:    true,
	})

	probe(t, spec[[]string]{
		name:     "[]string in TEXT[] (raw slice)",
		sql:      "TEXT[]",
		nullable: true,
		vals:     [][]string{nil, {}, {"a", "b"}},
		isNull:   func(m []string) bool { return m == nil },
		noPtr:    true,
	})

	probe(t, spec[[]int64]{
		name:     "[]int64 in BIGINT[] (raw slice)",
		sql:      "BIGINT[]",
		nullable: true,
		vals:     [][]int64{nil, {}, {1, 2}},
		isNull:   func(m []int64) bool { return m == nil },
		noPtr:    true,
	})
}

// ---------------------------------------------------------------------------
// tracked pointers
// ---------------------------------------------------------------------------

func (s spec[V]) pointersTracked(t *testing.T, db *prDB[V]) {
	ctx := t.Context()
	n := len(s.vals)

	read := func(t *testing.T, id int64) *V {
		t.Helper()

		got, err := db.NRows.NoTracking().Find(ctx, id)
		require.NoError(t, err)

		return got.P
	}

	step(t, "nil to value to nil to value in place", func(t *testing.T) {
		knownHere(t)

		e := &prNRow[V]{}
		require.NoError(t, db.NRows.Insert(ctx, e))
		require.False(t, db.HasChanges())

		for i := range s.vals {
			v := s.vals[i]
			e.P = &v
			require.True(t, db.HasChanges(), "nil -> &%#v not seen as a change", v)
			require.NoError(t, db.SaveChanges(ctx), "SaveChanges &%#v", v)

			if db.HasChanges() {
				t.Errorf("HasChanges after saving &%#v: %s", v, db.DebugTracked())
			}

			if p := read(t, e.ID); p == nil && !s.null(v) {
				t.Errorf("saved &%#v, read nil", v)
			} else if p != nil && !s.equal(*p, s.want(v)) {
				t.Errorf("saved &%#v, read %#v", v, *p)
			}

			e.P = nil

			require.True(t, db.HasChanges() || s.null(v), "&%#v -> nil not seen as a change", v)
			require.NoError(t, db.SaveChanges(ctx))

			if p := read(t, e.ID); p != nil {
				t.Errorf("saved nil, read %#v", *p)
			}
		}

		// Changing what the pointer points to, with the pointer itself kept.
		if n >= 2 {
			a, b := s.vals[0], s.vals[n-1]
			e.P = &a

			require.NoError(t, db.SaveChanges(ctx))

			*e.P = b

			if !s.equal(a, b) && !db.HasChanges() {
				t.Errorf("in-place change of the pointee %#v -> %#v not seen", a, b)
			}

			require.NoError(t, db.SaveChanges(ctx))

			if p := read(t, e.ID); p == nil && !s.null(b) {
				t.Errorf("in-place change saved, read nil")
			} else if p != nil && !s.equal(*p, s.want(b)) {
				t.Errorf("in-place change to %#v saved, read %#v", b, *p)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Valuer / Scanner errors
// ---------------------------------------------------------------------------

type prColorRow struct {
	ID int64
	C  prColor
}

type prColorDB struct {
	ant.Context
	Rows ant.Table[prColorRow]
}

func TestProbeValuerScannerErrors(t *testing.T) {
	knownHere(t)

	const ddl = `CREATE TABLE pr_color_rows (id BIGSERIAL PRIMARY KEY, c TEXT NOT NULL)`

	fixture.RunTrackedWith(t, ddl, func(t *testing.T, db *prColorDB) {
		ctx := t.Context()
		f := ant.Fields[prColorRow]()

		count := func() int64 {
			n, err := db.Rows.Count(ctx)
			require.NoError(t, err)

			return n
		}

		good := &prColorRow{C: 1}
		require.NoError(t, db.Rows.Insert(ctx, good))

		t.Run("Valuer error on Insert", func(t *testing.T) {
			knownHere(t)

			a, b := &prColorRow{C: 2}, &prColorRow{C: 99}
			err := db.Rows.Insert(ctx, a, b)
			require.Error(t, err)
			require.ErrorContains(t, err, "out of range")

			if count() != 1 {
				t.Errorf("rows after a failed Insert: %d, want 1", count())
			}

			if a.ID != 0 {
				t.Errorf("rolled-back entity kept key %d", a.ID)
			}
		})

		t.Run("Valuer error on InsertMany", func(t *testing.T) {
			knownHere(t)

			rows := []*prColorRow{{C: 0}, {C: 99}}
			_, err := db.Rows.InsertMany(ctx, rows)
			require.Error(t, err)

			if count() != 1 {
				t.Errorf("rows after a failed InsertMany: %d, want 1", count())
			}
		})

		t.Run("Valuer error in conditions and Set", func(t *testing.T) {
			knownHere(t)

			_, err := db.Rows.Where(ant.Eq(&f.C, prColor(99))).Count(ctx)
			require.Error(t, err)

			_, err = db.Rows.Where(ant.In(&f.C, 1, 99)).Count(ctx)
			require.Error(t, err)

			_, err = db.Rows.Where(ant.Eq(&f.ID, good.ID)).Update(ctx, ant.Set(&f.C, prColor(99)))
			require.Error(t, err)

			_, err = db.Rows.Upsert(ctx, &prColorRow{ID: good.ID, C: 99}, ant.OnConflict(&f.ID))
			require.Error(t, err)
		})

		t.Run("Valuer error in SaveChanges keeps the entity dirty", func(t *testing.T) {
			knownHere(t)

			tracked, err := db.Rows.Find(ctx, good.ID)
			require.NoError(t, err)

			tracked.C = 99

			require.Error(t, db.SaveChanges(ctx))

			if !db.HasChanges() {
				t.Errorf("a failed SaveChanges left the entity looking clean")
			}

			tracked.C = 2

			require.NoError(t, db.SaveChanges(ctx))

			got, err := db.Rows.NoTracking().Find(ctx, good.ID)
			require.NoError(t, err)
			require.Equal(t, prColor(2), got.C)
		})

		t.Run("Scanner error on every read path", func(t *testing.T) {
			knownHere(t)

			_, err := db.DB().ExecContext(ctx, `INSERT INTO pr_color_rows (c) VALUES ('purple')`)
			require.NoError(t, err)

			_, err = db.Rows.NoTracking().All(ctx)
			require.ErrorContains(t, err, "purple")

			_, err = db.Rows.Where(ant.Eq(&f.ID, good.ID+1)).First(ctx)
			require.ErrorContains(t, err, "purple")

			_, err = db.Rows.NoTracking().Find(ctx, good.ID+1)
			require.ErrorContains(t, err, "purple")

			var yielded int

			var each error

			for _, e := range db.Rows.Query().Each(ctx) {
				if e != nil {
					each = e

					break
				}

				yielded++
			}

			require.ErrorContains(t, each, "purple")

			_, err = ant.Pluck(ctx, db.Rows.Query(), &f.C)
			require.ErrorContains(t, err, "purple")

			_, err = ant.MaxOf(ctx, db.Rows.Query(), &f.ID)
			require.NoError(t, err)
		})
	})
}

// ---------------------------------------------------------------------------
// aggregates and Pluck over typed columns
// ---------------------------------------------------------------------------

func TestProbeAggregates(t *testing.T) {
	knownHere(t)

	ddl := `CREATE TABLE ` + prTable[int64](
		"row",
	) + ` (id BIGSERIAL PRIMARY KEY, v BIGINT NOT NULL);
		CREATE TABLE ` + prTable[int64](
		"nrow",
	) + ` (id BIGSERIAL PRIMARY KEY, p BIGINT)`

	fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[int64]) {
		ctx := t.Context()
		f := ant.Fields[prRow[int64]]()
		nf := ant.Fields[prNRow[int64]]()

		sum, err := ant.SumOf(ctx, db.Rows.Query(), &f.V)
		require.NoError(t, err)
		require.Zero(t, sum)

		mx, err := ant.MaxOf(ctx, db.Rows.Query(), &f.V)
		require.NoError(t, err)
		require.Zero(t, mx)

		_, err = db.Rows.InsertMany(ctx, []*prRow[int64]{{V: 1}, {V: 2}, {V: 3}, {V: -4}})
		require.NoError(t, err)

		sum, err = ant.SumOf(ctx, db.Rows.Query(), &f.V)
		require.NoError(t, err)
		require.EqualValues(t, 2, sum)

		avg, err := ant.AvgOf(ctx, db.Rows.Query(), &f.V)
		require.NoError(t, err)
		require.InDelta(t, 0.5, avg, 1e-9)

		mn, err := ant.MinOf(ctx, db.Rows.Query(), &f.V)
		require.NoError(t, err)
		require.EqualValues(t, -4, mn)

		// A sum beyond int64 is an error, not a wrapped number.
		_, err = db.Rows.InsertMany(ctx, []*prRow[int64]{{V: math.MaxInt64}, {V: math.MaxInt64}})
		require.NoError(t, err)

		if got, err := ant.SumOf(ctx, db.Rows.Query(), &f.V); err == nil {
			t.Errorf("SumOf beyond int64 returned %d with no error", got)
		}

		one, two := int64(1), int64(2)
		require.NoError(
			t,
			db.NRows.Insert(
				ctx,
				&prNRow[int64]{},
				&prNRow[int64]{P: &one},
				&prNRow[int64]{P: &two},
			),
		)

		plucked, err := ant.Pluck(ctx, db.NRows.Query().OrderBy(&nf.ID), &nf.P)
		require.NoError(t, err)
		require.Len(t, plucked, 3)
		require.Nil(t, plucked[0])
		require.EqualValues(t, 1, *plucked[1])
		require.EqualValues(t, 2, *plucked[2])
	})

	timeDDL := `CREATE TABLE ` + prTable[time.Time](
		"row",
	) + ` (id BIGSERIAL PRIMARY KEY, v TIMESTAMPTZ NOT NULL)`
	fixture.RunWith(t, timeDDL, func(t *testing.T, db *prDB[time.Time]) {
		ctx := t.Context()
		f := ant.Fields[prRow[time.Time]]()
		lo := time.Date(1999, 1, 1, 0, 0, 0, 0, time.FixedZone("x", 3600))
		hi := time.Date(2030, 1, 1, 0, 0, 0, 5000, time.UTC)
		_, err := db.Rows.InsertMany(ctx, []*prRow[time.Time]{{V: hi}, {V: lo}})
		require.NoError(t, err)

		mn, err := ant.MinOf(ctx, db.Rows.Query(), &f.V)
		require.NoError(t, err)
		require.True(t, mn.Equal(lo), "min %v", mn)

		mx, err := ant.MaxOf(ctx, db.Rows.Query(), &f.V)
		require.NoError(t, err)
		require.True(t, mx.Equal(hi), "max %v", mx)

		zero, err := ant.MaxOf(ctx, db.Rows.Where(ant.Gt(&f.V, hi)), &f.V)
		require.NoError(t, err)
		require.True(t, zero.IsZero(), "no rows gives the zero time, got %v", zero)
	})

	moneyDDL := `CREATE TABLE ` + prTable[prMoney](
		"row",
	) + ` (id BIGSERIAL PRIMARY KEY, v NUMERIC(12,2) NOT NULL)`
	fixture.RunWith(t, moneyDDL, func(t *testing.T, db *prDB[prMoney]) {
		ctx := t.Context()
		f := ant.Fields[prRow[prMoney]]()
		_, err := db.Rows.InsertMany(ctx, []*prRow[prMoney]{{V: prMoney{150}}, {V: prMoney{-5}}})
		require.NoError(t, err)

		got, err := ant.Pluck(ctx, db.Rows.Query().OrderBy(&f.ID), &f.V)
		require.NoError(t, err)
		require.Equal(t, []prMoney{{150}, {-5}}, got)
	})
}

// ---------------------------------------------------------------------------
// change tracking over many field types
// ---------------------------------------------------------------------------

type prTrackRow struct {
	ID    int64
	PI    *int64
	PF    *float64
	PI32  *int32
	PT    *time.Time
	PS    *string
	PB    *bool
	Raw   []byte
	Tags  prTags
	J     ant.JSON[map[string]any]
	JS    ant.JSON[[]prPair]
	N     sql.Null[int64]
	NT    sql.Null[time.Time]
	Arr   postgres.Array[int]
	Money prMoney
	Opt   *prMoney
	At    time.Time
	D     time.Duration
	F32   float32
	F     float64
	St    prStatus
	Un    ant.UnixTime
}

type prTrackDB struct {
	ant.Context
	Rows ant.Table[prTrackRow]
}

const prTrackDDL = `CREATE TABLE pr_track_rows (
	id BIGSERIAL PRIMARY KEY, pi BIGINT, pf DOUBLE PRECISION, pi32 INTEGER, pt TIMESTAMPTZ,
	ps TEXT, pb BOOLEAN, raw BYTEA, tags JSONB, j JSONB NOT NULL, js JSONB NOT NULL,
	n BIGINT, nt TIMESTAMPTZ, arr BIGINT[], money NUMERIC(12,2) NOT NULL, opt NUMERIC(12,2),
	at TIMESTAMPTZ NOT NULL, d BIGINT NOT NULL, f32 REAL NOT NULL, f DOUBLE PRECISION NOT NULL,
	st TEXT NOT NULL, un BIGINT NOT NULL)`

func prTrackSeed() *prTrackRow {
	return &prTrackRow{
		PI:   new(int64(1)),
		PF:   new(1.5),
		PI32: new(int32(2)),
		PT:   new(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)),
		PS:   new("s"),
		PB:   new(true),
		Raw:  []byte{1, 2, 3},
		Tags: prTags{"a", "b"},
		J: ant.JSON[map[string]any]{
			V: map[string]any{"k": []any{1.0, "x"}, "m": map[string]any{"z": true}},
		},
		JS:    ant.JSON[[]prPair]{V: []prPair{{1, "a"}, {2, "b"}}},
		N:     sql.Null[int64]{V: 5, Valid: true},
		NT:    sql.Null[time.Time]{V: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
		Arr:   postgres.Array[int]{1, 2, 3},
		Money: prMoney{100},
		Opt:   &prMoney{200},
		At:    time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
		D:     time.Second,
		F32:   1.5,
		F:     2.5,
		St:    "x",
		Un:    ant.UnixTime{Time: time.Unix(1_700_000_000, 0).UTC()},
	}
}

func TestProbeTrackingMutations(t *testing.T) {
	knownHere(t)

	mutations := map[string]func(e *prTrackRow){
		"*int64 pointee":       func(e *prTrackRow) { *e.PI = 9 },
		"*float64 pointee":     func(e *prTrackRow) { *e.PF = 9.5 },
		"*int32 pointee":       func(e *prTrackRow) { *e.PI32 = 9 },
		"*time.Time pointee":   func(e *prTrackRow) { *e.PT = e.PT.Add(time.Hour) },
		"*string pointee":      func(e *prTrackRow) { *e.PS = "changed" },
		"*bool pointee":        func(e *prTrackRow) { *e.PB = false },
		"[]byte element":       func(e *prTrackRow) { e.Raw[1] = 99 },
		"[]byte append":        func(e *prTrackRow) { e.Raw = append(e.Raw, 4) },
		"[]byte to empty":      func(e *prTrackRow) { e.Raw = []byte{} },
		"[]byte to nil":        func(e *prTrackRow) { e.Raw = nil },
		"Tags element":         func(e *prTrackRow) { e.Tags[0] = "z" },
		"Tags append":          func(e *prTrackRow) { e.Tags = append(e.Tags, "c") },
		"Tags to nil":          func(e *prTrackRow) { e.Tags = nil },
		"JSON map nested":      func(e *prTrackRow) { e.J.V["m"].(map[string]any)["z"] = false },
		"JSON map slice elem":  func(e *prTrackRow) { e.J.V["k"].([]any)[0] = 2.0 },
		"JSON map new key":     func(e *prTrackRow) { e.J.V["new"] = nil },
		"JSON map to nil":      func(e *prTrackRow) { e.J.V = nil },
		"JSON slice of struct": func(e *prTrackRow) { e.JS.V[1].B = "changed" },
		"JSON slice append":    func(e *prTrackRow) { e.JS.V = append(e.JS.V, prPair{3, "c"}) },
		"Null value":           func(e *prTrackRow) { e.N.V = 6 },
		"Null validity": func(e *prTrackRow) {
			e.N = sql.Null[int64]{}
		}, // a NULL keeps no value
		"Null time":      func(e *prTrackRow) { e.NT.V = e.NT.V.Add(time.Second) },
		"Array element":  func(e *prTrackRow) { e.Arr[0] = 100 },
		"Array append":   func(e *prTrackRow) { e.Arr = append(e.Arr, 4) },
		"Money value":    func(e *prTrackRow) { e.Money.Cents++ },
		"*Money pointee": func(e *prTrackRow) { e.Opt.Cents++ },
		"time.Time":      func(e *prTrackRow) { e.At = e.At.Add(time.Microsecond) },
		"Duration":       func(e *prTrackRow) { e.D++ },
		"float32":        func(e *prTrackRow) { e.F32 = 2.5 },
		"float64":        func(e *prTrackRow) { e.F = -0.5 },
		"named string":   func(e *prTrackRow) { e.St = "y" },
		"UnixTime":       func(e *prTrackRow) { e.Un.Time = e.Un.Time.Add(time.Second) },
	}

	noops := map[string]func(e *prTrackRow){
		"*int64 same value": func(e *prTrackRow) { e.PI = new(*e.PI) },
		"*time.Time other zone": func(e *prTrackRow) {
			e.PT = new(e.PT.In(time.FixedZone("z", 7200)))
		},
		"time.Time other zone": func(e *prTrackRow) {
			e.At = e.At.In(time.FixedZone("z", -3600))
		},
		"[]byte equal copy": func(e *prTrackRow) { e.Raw = slices.Clone(e.Raw) },
		"Tags equal copy":   func(e *prTrackRow) { e.Tags = slices.Clone(e.Tags) },
		"JSON equal rebuilt map": func(e *prTrackRow) {
			e.J.V = map[string]any{"k": []any{1.0, "x"}, "m": map[string]any{"z": true}}
		},
		"Null time other zone": func(e *prTrackRow) {
			e.NT.V = e.NT.V.In(time.FixedZone("z", 3600))
		},
		"Array equal copy": func(e *prTrackRow) { e.Arr = slices.Clone(e.Arr) },
		"*Money same":      func(e *prTrackRow) { e.Opt = &prMoney{e.Opt.Cents} },
		"UnixTime other zone": func(e *prTrackRow) {
			e.Un.Time = e.Un.Time.In(time.FixedZone("z", 3600))
		},
	}

	fixture.RunTrackedWith(t, prTrackDDL, func(t *testing.T, db *prTrackDB) {
		ctx := t.Context()

		load := func(t *testing.T) *prTrackRow {
			t.Helper()

			seed := prTrackSeed()
			require.NoError(t, db.Rows.Insert(ctx, seed))
			db.ClearTracked()

			e, err := db.Rows.Find(ctx, seed.ID)
			require.NoError(t, err)
			require.False(t, db.HasChanges(), "fresh load: %s", db.DebugTracked())

			return e
		}

		for name, mutate := range mutations {
			t.Run("mutate "+name, func(t *testing.T) {
				knownHere(t)

				e := load(t)
				mutate(e)

				if !db.HasChanges() {
					t.Fatalf("change not detected")
				}

				if err := db.SaveChanges(ctx); err != nil {
					t.Fatalf("SaveChanges: %v", err)
				}

				if db.HasChanges() {
					t.Errorf("still dirty after SaveChanges: %s", db.DebugTracked())
				}

				got, err := db.Rows.NoTracking().Find(ctx, e.ID)
				require.NoError(t, err)

				// Compare through JSON of the values that matter, and times by instant.
				want, _ := json.Marshal(e)
				have, _ := json.Marshal(got)

				var w, h map[string]any

				_ = json.Unmarshal(want, &w)
				_ = json.Unmarshal(have, &h)

				for k := range w {
					if !reflect.DeepEqual(w[k], h[k]) {
						if tw, ok := w[k].(string); ok {
							a, e1 := time.Parse(time.RFC3339Nano, tw)

							b, e2 := time.Parse(time.RFC3339Nano, fmt.Sprint(h[k]))
							if e1 == nil && e2 == nil && a.Equal(b) {
								continue
							}
						}

						t.Errorf("column %s: saved %v, read %v", k, w[k], h[k])
					}
				}
			})
		}

		for name, mutate := range noops {
			t.Run("no-op "+name, func(t *testing.T) {
				knownHere(t)

				e := load(t)
				mutate(e)

				if db.HasChanges() {
					t.Errorf("an equal value counts as a change: %s", db.DebugTracked())
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// keys that are times, bytes, or composite with a time
// ---------------------------------------------------------------------------

type prTimeKey struct {
	ID   time.Time
	Name string
}

type prBytesKey struct {
	ID   []byte
	Name string
}

type prReading struct {
	DeviceID int64
	At       time.Time
	Value    float64
	Tag      sql.NullString
}

func (*prReading) Configure(b *ant.ModelBuilder[prReading]) {
	k := ant.Fields[prReading]()
	b.CompositeKey(ant.F(&k.DeviceID), ant.F(&k.At))
}

type prOddKeysDB struct {
	ant.Context
	TimeKeys ant.Table[prTimeKey]
	ByteKeys ant.Table[prBytesKey]
	Readings ant.Table[prReading]
}

const prOddKeysDDL = `
	CREATE TABLE pr_time_keys (id TIMESTAMPTZ PRIMARY KEY, name TEXT NOT NULL);
	CREATE TABLE pr_bytes_keys (id BYTEA PRIMARY KEY, name TEXT NOT NULL);
	CREATE TABLE pr_readings (device_id BIGINT NOT NULL, at TIMESTAMPTZ NOT NULL, value DOUBLE PRECISION NOT NULL,
		tag TEXT, PRIMARY KEY (device_id, at))`

func TestProbeOddKeys(t *testing.T) {
	knownHere(t)

	fixture.RunTrackedWith(t, prOddKeysDDL, func(t *testing.T, db *prOddKeysDB) {
		ctx := t.Context()

		t.Run("time key: identity, Find, SaveChanges", func(t *testing.T) {
			knownHere(t)

			at := time.Date(2024, 1, 2, 3, 4, 5, 123_456_000, time.UTC)
			e := &prTimeKey{ID: at, Name: "a"}
			require.NoError(t, db.TimeKeys.Insert(ctx, e))

			got, err := db.TimeKeys.Find(ctx, at.In(time.FixedZone("z", 3600)))
			require.NoError(t, err, "Find by the same instant in another zone")

			if got != e {
				t.Errorf("Find returned a second instance of the tracked row")
			}

			all, err := db.TimeKeys.OrderBy(&ant.Fields[prTimeKey]().Name).All(ctx)
			require.NoError(t, err)
			require.Len(t, all, 1)

			if all[0] != e {
				t.Errorf(
					"All returned a second instance of the tracked row (identity lost for a time key)",
				)
			}

			e.Name = "b"

			require.NoError(t, db.SaveChanges(ctx))

			fresh, err := db.TimeKeys.NoTracking().Find(ctx, at)
			require.NoError(t, err)
			require.Equal(t, "b", fresh.Name)

			db.ClearTracked()
			loaded, err := db.TimeKeys.Find(ctx, at)
			require.NoError(t, err)

			loaded.Name = "c"

			require.NoError(t, db.SaveChanges(ctx))

			fresh, err = db.TimeKeys.NoTracking().Find(ctx, at)
			require.NoError(t, err)
			require.Equal(t, "c", fresh.Name)
		})

		t.Run("[]byte key", func(t *testing.T) {
			knownHere(t)

			e := &prBytesKey{ID: []byte{0, 1, 2, 255}, Name: "a"}
			require.NoError(t, db.ByteKeys.Insert(ctx, e))

			got, err := db.ByteKeys.Find(ctx, string([]byte{0, 1, 2, 255}))
			if err != nil {
				t.Logf("Find with a string for a []byte key: %v", err)
			} else if got != e {
				t.Errorf("Find returned a second instance of the tracked row")
			}

			e.Name = "b"

			require.NoError(t, db.SaveChanges(ctx))

			fresh, err := db.ByteKeys.NoTracking().
				Where(ant.SQLCond("id = ?", []byte{0, 1, 2, 255})).
				Single(ctx)
			require.NoError(t, err)
			require.Equal(t, "b", fresh.Name)
		})

		t.Run("composite key with a time", func(t *testing.T) {
			knownHere(t)

			f := ant.Fields[prReading]()
			at := time.Now()
			r := &prReading{DeviceID: 7, At: at, Value: 1.5}
			require.NoError(t, db.Readings.Insert(ctx, r))
			require.False(t, db.HasChanges())

			all, err := db.Readings.All(ctx)
			require.NoError(t, err)
			require.Len(t, all, 1)

			if all[0] != r {
				t.Errorf(
					"All returned a second instance of the tracked row (identity lost for a composite key with a time)",
				)
			}

			got, err := db.Readings.FindKey(ctx, int64(7), at)
			require.NoError(t, err)

			if got != r {
				t.Errorf("FindKey returned a second instance of the tracked row")
			}

			r.Value = 2.5
			r.Tag = sql.NullString{String: "t", Valid: true}

			require.NoError(t, db.SaveChanges(ctx))

			fresh, err := db.Readings.NoTracking().Where(ant.Eq(&f.DeviceID, int64(7))).Single(ctx)
			require.NoError(t, err)
			require.Equal(t, 2.5, fresh.Value)
			require.Equal(t, sql.NullString{String: "t", Valid: true}, fresh.Tag)

			ok, err := db.Readings.Upsert(
				ctx,
				&prReading{DeviceID: 7, At: at, Value: 9},
				ant.OnConflict(&f.DeviceID).And(ant.F(&f.At)),
			)
			require.NoError(t, err)
			require.True(t, ok)

			n, err := db.Readings.Count(ctx)
			require.NoError(t, err)

			require.EqualValues(
				t,
				1,
				n,
				"Upsert on a composite key with a ns time made a second row",
			)

			// Delete by the tracked entity.
			require.NoError(t, ant.Remove(db, r))
			require.NoError(t, db.SaveChanges(ctx))

			n, err = db.Readings.Count(ctx)
			require.NoError(t, err)
			require.Zero(t, n, "Remove + SaveChanges of a composite-key row with a ns time")
		})
	})
}

// PostgreSQL prints non-finite floats as Infinity / -Infinity / NaN, and Point.Value
// writes +Inf / -Inf; both must scan back.
func TestProbePointScanFormats(t *testing.T) {
	knownHere(t)

	for _, text := range []string{
		"(Infinity,-Infinity)", "(NaN,1)", "(1,2)", "(1e+21,2)", "(+Inf,1)", "(-Inf,1)",
	} {
		var p postgres.Point
		if err := p.Scan(text); err != nil {
			t.Errorf("Point.Scan(%q): %v", text, err)
		}
	}
}

// ---------------------------------------------------------------------------
// secure.Value: encrypted columns
// ---------------------------------------------------------------------------

func TestProbeSecure(t *testing.T) {
	knownHere(t)

	require.NoError(t, secure.SetKeys("probe", map[string][]byte{"probe": make([]byte, 32)}))

	sv := func(s string) secure.Value[string] { return secure.New(s) }

	probe(t, spec[secure.Value[string]]{
		noEq: true,
		name: "secure.Value[string]",
		sql:  "TEXT",
		vals: []secure.Value[string]{
			sv(""),
			sv("a"),
			sv("a\x00b"),
			sv("NULL"),
			sv("\U0001F600 \u65e5\u672c"),
			sv(`"'\`),
			sv(strings.Repeat("x", 500_000)),
		},
	})

	probe(t, spec[secure.Value[string]]{
		noEq:  true,
		name:  "secure.Value[string] invalid UTF-8 (JSON replaces the bytes)",
		sql:   "TEXT",
		vals:  []secure.Value[string]{sv("ab\xffcd")},
		noPtr: true,
	})

	probe(t, spec[secure.Value[int64]]{
		noEq: true,
		name: "secure.Value[int64]",
		sql:  "TEXT",
		vals: []secure.Value[int64]{
			secure.New(
				int64(math.MinInt64),
			),
			secure.New(int64(0)),
			secure.New(int64(math.MaxInt64)),
		},
	})

	probe(t, spec[secure.Value[uint64]]{
		noEq: true,
		name: "secure.Value[uint64]",
		sql:  "TEXT",
		vals: []secure.Value[uint64]{secure.New(uint64(0)), secure.New(uint64(math.MaxUint64))},
	})

	probe(t, spec[secure.Value[float64]]{
		noEq: true,
		name: "secure.Value[float64]",
		sql:  "TEXT",
		vals: []secure.Value[float64]{
			secure.New(-math.MaxFloat64), secure.New(0.1), secure.New(math.SmallestNonzeroFloat64),
		},
	})

	probe(t, spec[secure.Value[[]byte]]{
		noEq: true,
		name: "secure.Value[[]byte] (nil vs empty)",
		sql:  "TEXT",
		vals: []secure.Value[[]byte]{
			secure.New([]byte(nil)), secure.New([]byte{}), secure.New([]byte{0, 255}),
		},
	})

	probe(t, spec[secure.Value[time.Time]]{
		noEq: true,
		name: "secure.Value[time.Time]",
		sql:  "TEXT",
		vals: []secure.Value[time.Time]{
			secure.New(time.Time{}),
			secure.New(time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC)),
			secure.New(time.Date(2024, 1, 2, 3, 4, 5, 0, time.FixedZone("", 3600))),
		},
		same: func(a, b secure.Value[time.Time]) bool { return a.V.Equal(b.V) },
	})

	probe(t, spec[secure.Value[map[string]any]]{
		noEq: true,
		name: "secure.Value[map[string]any]",
		sql:  "TEXT",
		vals: []secure.Value[map[string]any]{
			secure.New(map[string]any(nil)),
			secure.New(map[string]any{}),
			secure.New(map[string]any{"a": 1.0, "b": []any{"x", nil}}),
		},
	})

	probe(t, spec[secure.Value[prPair]]{
		noEq: true,
		name: "secure.Value[struct]",
		sql:  "TEXT",
		vals: []secure.Value[prPair]{secure.New(prPair{}), secure.New(prPair{A: 1, B: "x"})},
	})
}

// A secure column is nothing in the clear, whatever the value is.
func TestProbeSecureIsCiphertext(t *testing.T) {
	knownHere(t)

	require.NoError(t, secure.SetKeys("probe", map[string][]byte{"probe": make([]byte, 32)}))

	ddl := `CREATE TABLE ` + prTable[secure.Value[string]](
		"row",
	) + ` (id BIGSERIAL PRIMARY KEY, v TEXT NOT NULL)`
	fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[secure.Value[string]]) {
		ctx := t.Context()
		f := ant.Fields[prRow[secure.Value[string]]]()
		e := &prRow[secure.Value[string]]{V: secure.New("hunter2-secret")}
		require.NoError(t, db.Rows.Insert(ctx, e))

		raw, err := ant.Raw[string](
			db,
			"SELECT v FROM "+prTable[secure.Value[string]]("row"),
		).First(ctx)
		require.NoError(t, err)
		require.NotContains(t, *raw, "hunter2")

		// Bulk update, upsert and InsertMany keep it encrypted too.
		_, err = db.Rows.Where(ant.Eq(&f.ID, e.ID)).
			Update(ctx, ant.Set(&f.V, secure.New("hunter3-secret")))
		require.NoError(t, err)

		_, err = db.Rows.Upsert(
			ctx,
			&prRow[secure.Value[string]]{ID: e.ID, V: secure.New("hunter4-secret")},
			ant.OnConflict(&f.ID),
		)
		require.NoError(t, err)

		_, err = db.Rows.InsertMany(
			ctx,
			[]*prRow[secure.Value[string]]{{V: secure.New("hunter5-secret")}},
		)
		require.NoError(t, err)

		all, err := ant.Pluck(ctx, db.Rows.Query(), &f.V)
		require.NoError(t, err)
		require.Len(t, all, 2)

		var texts []string

		rows, err := db.DB().
			QueryContext(ctx, "SELECT v FROM "+prTable[secure.Value[string]]("row"))
		require.NoError(t, err)

		defer rows.Close()

		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))

			texts = append(texts, s)
		}

		for _, text := range texts {
			require.NotContains(t, text, "hunter")
		}

		// The value does not leak through fmt or JSON.
		require.NotContains(t, fmt.Sprintf("%v %+v %#v", e.V, e, e.V.String()), "hunter")

		js, err := json.Marshal(e)
		require.NoError(t, err)
		require.NotContains(t, string(js), "hunter")
	})
}

// ---------------------------------------------------------------------------
// NULL read into a Go type that cannot hold NULL: an error or a documented zero,
// never a panic. Logged, not asserted, because the answer differs by type.
// ---------------------------------------------------------------------------

func nullInto[V any](t *testing.T, sqlType string) {
	t.Helper()

	var zero V

	t.Run(fmt.Sprintf("%T", zero), func(t *testing.T) {
		knownHere(t)

		ddl := `CREATE TABLE ` + prTable[V](
			"row",
		) + ` (id BIGSERIAL PRIMARY KEY, v ` + sqlType + `)`
		fixture.RunWith(t, ddl, func(t *testing.T, db *prDB[V]) {
			ctx := t.Context()

			_, err := db.DB().
				ExecContext(ctx, "INSERT INTO "+prTable[V]("row")+" (v) VALUES (NULL)")
			require.NoError(t, err)

			defer func() {
				if r := recover(); r != nil {
					t.Errorf("PANIC reading NULL: %v", r)
				}
			}()

			rows, err := db.Rows.NoTracking().All(ctx)
			if err != nil {
				t.Logf("NULL into %T: error: %v", zero, err)

				return
			}

			t.Logf("NULL into %T: silently %#v", zero, rows[0].V)
		})
	})
}

func TestProbeNullIntoNonNullable(t *testing.T) {
	knownHere(t)

	nullInto[int64](t, "BIGINT")
	nullInto[string](t, "TEXT")
	nullInto[bool](t, "BOOLEAN")
	nullInto[float64](t, "DOUBLE PRECISION")
	nullInto[time.Time](t, "TIMESTAMPTZ")
	nullInto[[]byte](t, "BYTEA")
	nullInto[ant.UnixTime](t, "BIGINT")
	nullInto[ant.JSON[prPair]](t, "JSONB")
	nullInto[ant.Gob[prPair]](t, "BYTEA")
	nullInto[uuid.UUID](t, "UUID")
	nullInto[decimal.Decimal](t, "NUMERIC")
	nullInto[postgres.Point](t, "POINT")
	nullInto[postgres.Array[string]](t, "TEXT[]")
	nullInto[prStatus](t, "TEXT")
	nullInto[prMoney](t, "NUMERIC")
	nullInto[prOptional](t, "BIGINT")
	nullInto[secure.Value[string]](t, "TEXT")
	nullInto[time.Duration](t, "BIGINT")
	nullInto[prTags](t, "JSONB")
	nullInto[json.RawMessage](t, "JSONB")
	nullInto[netip.Addr](t, "INET")
}

// The reverse: a value too big for the Go type is an error, not a wrapped number.
func TestProbeOutOfRangeReads(t *testing.T) {
	knownHere(t)

	check := func(t *testing.T, name, sqlType, literal string, read func(ctx context.Context, h ant.Handle) error) {
		t.Run(name, func(t *testing.T) {
			knownHere(t)

			fixture.RunWith(
				t,
				`CREATE TABLE pr_probe_tmp (v `+sqlType+`)`,
				func(t *testing.T, db *prDB[int64]) {
					ctx := t.Context()
					_, err := db.DB().
						ExecContext(ctx, "INSERT INTO pr_probe_tmp VALUES ("+literal+")")
					require.NoError(t, err)

					if err := read(ctx, db); err == nil {
						t.Errorf("%s: no error reading %s", name, literal)
					}
				},
			)
		})
	}

	check(t, "300 into uint8", "INT", "300", func(ctx context.Context, h ant.Handle) error {
		_, err := ant.Raw[uint8](h, "SELECT v FROM pr_probe_tmp").First(ctx)

		return err
	})

	check(t, "-1 into uint64", "BIGINT", "-1", func(ctx context.Context, h ant.Handle) error {
		_, err := ant.Raw[uint64](h, "SELECT v FROM pr_probe_tmp").First(ctx)

		return err
	})

	check(
		t,
		"2^40 into int32",
		"BIGINT",
		"1099511627776",
		func(ctx context.Context, h ant.Handle) error {
			_, err := ant.Raw[int32](h, "SELECT v FROM pr_probe_tmp").First(ctx)

			return err
		},
	)

	check(t, "1.5 into int64", "NUMERIC", "1.5", func(ctx context.Context, h ant.Handle) error {
		_, err := ant.Raw[int64](h, "SELECT v FROM pr_probe_tmp").First(ctx)

		return err
	})

	check(
		t,
		"1e300 into float32",
		"DOUBLE PRECISION",
		"1e300",
		func(ctx context.Context, h ant.Handle) error {
			_, err := ant.Raw[float32](h, "SELECT v FROM pr_probe_tmp").First(ctx)

			return err
		},
	)
}

// ant.JSON also replaces invalid UTF-8, silently, as encoding/json does.
func TestProbeJSONInvalidUTF8(t *testing.T) {
	knownHere(t)

	probe(t, spec[ant.JSON[string]]{
		name: "ant.JSON[string] invalid UTF-8",
		sql:  "JSONB",
		vals: []ant.JSON[string]{{V: "ab\xffcd"}},
	})
}

// knownProbes are probes that fail for a tracked issue, by test name.
var knownProbes = []struct {
	pattern *regexp.Regexp
	issue   string
}{
	{regexp.MustCompile(`^TestProbeScalars/float(32|64)/postgres#0[23]/`), "#48"},
	{regexp.MustCompile(`^TestProbeArrays/postgres\.Array\[float64\]/postgres#0[23]/`), "#48"},
	{regexp.MustCompile(`^TestProbeArrays/postgres\.Point_with_Inf_and_NaN/`), "#48, #52"},
	{
		regexp.MustCompile(
			`^TestProbeTrackingMutations/postgres/no-op_(UnixTime|Null_time)_other_zone$`,
		),
		"#48",
	},
	{
		regexp.MustCompile(
			`^TestProbeTrackingMutations/postgres/mutate_JSON_map_(nested|slice_elem)$`,
		),
		"#47",
	},
	{regexp.MustCompile(`^TestProbeOddKeys/postgres/(time_key|composite_key_with_a_time)`), "#49"},
	{regexp.MustCompile(`^TestProbeCustomValuers/Lines_`), "#50"},
	{regexp.MustCompile(`^TestProbeEmbeddedPointer$`), "#51"},
	{regexp.MustCompile(`^TestProbePointScanFormats$`), "#52"},
	{regexp.MustCompile(`^TestProbeScalars/ant\.UnixTime_epoch/`), "#53"},
	{regexp.MustCompile(`^TestProbeNullTypes/sql\.Null\[uint64\]_high_bit/`), "#53"},
	{regexp.MustCompile(`^TestProbeInWithNulls$`), "#53"},
	{regexp.MustCompile(`^TestProbeCustomValuers/Temp_`), "#53"},
	{regexp.MustCompile(`^TestProbeBytes/(named_\[\]byte|json\.RawMessage)_with_NULL/`), "#53"},
	{regexp.MustCompile(`^TestProbeBigBatches/postgres/In_with_a_huge_list$`), "#53"},
	{
		regexp.MustCompile(`^TestProbeOtherNative/(time\.Duration_in_INTERVAL|net\.IP_in_INET)/`),
		"#53",
	},
	{regexp.MustCompile(`^TestProbeSecure/secure\.Value\[string\]_invalid_UTF-8`), "#53"},
	{regexp.MustCompile(`^TestProbeJSONInvalidUTF8$`), "#53"},
}

// knownHere skips the running test if it fails for a tracked issue, unless
// ANT_RUN_KNOWN is set, as it is when working on that issue.
func knownHere(t *testing.T) {
	t.Helper()

	if os.Getenv("ANT_RUN_KNOWN") != "" {
		return
	}

	for _, k := range knownProbes {
		if k.pattern.MatchString(t.Name()) {
			t.Skipf("known failure, %s; ANT_RUN_KNOWN=1 runs it", k.issue)
		}
	}
}
