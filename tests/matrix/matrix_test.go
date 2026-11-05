package matrix

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/internal/testdb"
)

// known lists combinations that fail for a reason an issue tracks, so the
// matrix stays green while they wait for a fix. A pattern matches the test's
// full name, as go test prints it. Some matches pass anyway, such as a write
// the guard refuses before any SQL runs, so an entry is checked as a whole: one
// that matches combinations but sees none fail is fixed, and fails the test, so
// that the list only holds what is still broken.
var known = []*knownFailure{
	{pattern: `/Union_of_paged_sides/`, issue: "#8, fixed by PR #16"},
	{pattern: `^TestMatrix/(mysql|mariadb)/.*/offset_3/(Update|Delete)$`, issue: "#22"},
	{pattern: `^TestMatrix/sqlserver/(FromCTE|Recursive)/.*/(Count|Exists)$`, issue: "#23"},
	{pattern: `^TestMatrix/sqlserver/Recursive/.*/(All|First|Include)$`, issue: "#46"},
}

type knownFailure struct {
	pattern, issue  string
	matched, failed int
}

// knownFor returns the entry a combination is waiting on, or nil.
func knownFor(name string) *knownFailure {
	for _, k := range known {
		if regexp.MustCompile(k.pattern).MatchString(name) {
			k.matched++

			return k
		}
	}

	return nil
}

// checkKnown fails for each entry that matched combinations on this database
// and saw none of them fail, and resets the counts for the next one. A run of
// some combinations, by -run, may pick only those that pass anyway, so then it
// says nothing.
func checkKnown(t *testing.T) {
	whole := true

	if run := flag.Lookup("test.run"); run != nil && strings.Contains(run.Value.String(), "/") {
		whole = false
	}

	for _, k := range known {
		if whole && k.matched > 0 && k.failed == 0 {
			t.Errorf(
				"%d combinations match %s and all pass: take it out of the known list",
				k.matched,
				k.issue,
			)
		}

		k.matched, k.failed = 0, 0
	}
}

// check runs one combination as a subtest. A failure on a known issue is a
// skip.
func check(t *testing.T, name string, fn func(t *testing.T) error) {
	t.Run(name, func(t *testing.T) {
		err := fn(t)
		k := knownFor(t.Name())

		switch {
		case err != nil && k != nil:
			k.failed++
			t.Skipf("known, %s: %v", k.issue, err)
		case err != nil:
			t.Error(err)
		}
	})
}

// expected works one combination out in Go: the rows the query returns, in order
// if it has one.
func expected(
	w *world,
	src source,
	c condition,
	o order,
	p paging,
) []*Author {
	var rows []*Author

	for _, a := range src.rows(w) {
		if c.eval == nil || c.eval(w, a) == yes {
			rows = append(rows, a)
		}
	}

	if o.less != nil {
		slices.SortFunc(rows, o.less)
	}

	return p.page(rows)
}

func ids(rows []*Author) []int64 {
	out := make([]int64, len(rows))

	for i, a := range rows {
		out[i] = a.ID
	}

	return out
}

// sameRows compares results: in order when the query has one, and otherwise as
// sets. Paging without an order may return any rows of the right count, so then
// only the count and that they belong are checked.
func sameRows(got, want []*Author, all []*Author, o order, p paging) error {
	g, wnt := ids(got), ids(want)

	switch {
	case o.less != nil:
		if !slices.Equal(g, wnt) {
			return fmt.Errorf("got %v, want %v", g, wnt)
		}
	case p.limit >= 0 || p.offset > 0:
		pool := ids(all)

		// Each row of the pool can be returned once.
		for _, id := range g {
			at := slices.Index(pool, id)
			if at < 0 {
				return fmt.Errorf("got %v, with %d not among %v, or twice", g, id, ids(all))
			}

			pool = slices.Delete(pool, at, at+1)
		}

		if len(g) != len(wnt) {
			return fmt.Errorf("got %d rows %v, want %d", len(g), g, len(wnt))
		}
	default:
		slices.Sort(g)
		slices.Sort(wnt)

		if !slices.Equal(g, wnt) {
			return fmt.Errorf("got %v, want %v", g, wnt)
		}
	}

	return nil
}

func build(db *MatrixDB, src source, c condition, o order, p paging) *ant.Query[Author] {
	q := src.build(db)

	if c.cond != nil {
		q = q.Where(c.cond(db))
	}

	return p.apply(o.apply(q))
}

// errRollback ends a write's transaction after the test has looked inside it.
var errRollback = errors.New("matrix: roll back")

// TestMatrix runs every combination of source, condition, ordering, and paging
// through each terminal call, and compares what comes back with the oracle.
// Writes run in a transaction that rolls back, and the test reads inside it which
// rows they changed.
func TestMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("the matrix runs every combination, which takes about a minute")
	}

	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		create(t, target)

		db, err := ant.Open[MatrixDB](
			t.Context(),
			target.Dialect,
			target.DSN,
			ant.WithTracking(false),
		)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { db.Close() })

		w := seed(t, db)

		t.Cleanup(func() { checkKnown(t) })

		for _, src := range sources() {
			for _, c := range conditions() {
				for _, o := range orders() {
					for _, p := range pagings() {
						name := fmt.Sprintf("%s/%s/%s/%s", src.name, c.name, o.name, p.name)
						t.Run(name, func(t *testing.T) { combination(t, db, w, src, c, o, p) })
					}
				}
			}
		}
	})
}

func combination(t *testing.T, db *MatrixDB, w *world, src source, c condition, o order, p paging) {
	ctx := t.Context()
	want := expected(w, src, c, o, p)
	all := expected(w, src, c, o, paging{"all", -1, -1})

	check(t, "All", func(t *testing.T) error {
		got, err := build(db, src, c, o, p).All(ctx)
		if err != nil {
			return err
		}

		return sameRows(got, want, all, o, p)
	})

	check(t, "Count", func(t *testing.T) error {
		n, err := build(db, src, c, o, p).Count(ctx)
		if err != nil {
			return err
		}

		if int(n) != len(want) {
			return fmt.Errorf("count %d, want %d", n, len(want))
		}

		return nil
	})

	check(t, "Exists", func(t *testing.T) error {
		found, err := build(db, src, c, o, p).Exists(ctx)
		if err != nil {
			return err
		}

		if found != (len(want) > 0) {
			return fmt.Errorf("exists %v, want %v", found, len(want) > 0)
		}

		return nil
	})

	if o.less != nil {
		check(t, "First", func(t *testing.T) error {
			got, err := build(db, src, c, o, p).First(ctx)

			switch {
			case len(want) == 0 && errors.Is(err, ant.ErrNotFound):
				return nil
			case err != nil:
				return err
			case len(want) == 0:
				return fmt.Errorf("first %d, want none", got.ID)
			case got.ID != want[0].ID:
				return fmt.Errorf("first %d, want %d", got.ID, want[0].ID)
			}

			return nil
		})
	}

	check(t, "Include", func(t *testing.T) error {
		got, err := build(db, src, c, o, p).Include(&A.Posts).All(ctx)
		if err != nil {
			return err
		}

		if err := sameRows(got, want, all, o, p); err != nil {
			return err
		}

		for _, a := range got {
			var have, should []int64

			for _, post := range a.Posts {
				have = append(have, post.ID)
			}

			for _, post := range w.postsOf(a.ID) {
				should = append(should, post.ID)
			}

			slices.Sort(have)

			if !slices.Equal(have, should) {
				return fmt.Errorf("author %d has posts %v, want %v", a.ID, have, should)
			}
		}

		return nil
	})

	// A write without a condition anywhere needs Unfiltered.
	guarded := (c.cond == nil || c.all) && !src.filtered

	// SQL Server allows WITH only at the start of a statement, so a write on a
	// CTE refuses there, as decision 38 says.
	if (src.name == "FromCTE" || src.name == "Recursive") &&
		!dialect.Supports(db.Dialect(), dialect.SubqueryWith) {
		for _, terminal := range []string{"Update", "Delete"} {
			check(t, terminal, func(t *testing.T) error {
				return refused(t, db, func(tx *MatrixDB) error {
					q := build(tx, src, c, o, p)

					if terminal == "Update" {
						_, err := q.Update(ctx, ant.Set(&A.Marked, true))

						return err
					}

					_, err := q.Delete(ctx)

					return err
				})
			})
		}

		return
	}

	check(t, "Update", func(t *testing.T) error {
		return written(
			t,
			db,
			guarded,
			want,
			all,
			o,
			p,
			func(tx *MatrixDB) error {
				_, err := build(tx, src, c, o, p).Update(ctx, ant.Set(&A.Marked, true))

				return err
			},
			func(tx *MatrixDB) ([]*Author, error) {
				return tx.Authors.Where(ant.Eq(&A.Marked, true)).All(ctx)
			},
		)
	})

	check(t, "Delete", func(t *testing.T) error {
		return written(
			t,
			db,
			guarded,
			want,
			all,
			o,
			p,
			func(tx *MatrixDB) error {
				_, err := build(tx, src, c, o, p).Delete(ctx)

				return err
			},
			func(tx *MatrixDB) ([]*Author, error) {
				kept, err := tx.Authors.Query().All(ctx)
				if err != nil {
					return nil, err
				}

				var gone []*Author

				for _, a := range w.authors {
					if !slices.ContainsFunc(kept, func(k *Author) bool { return k.ID == a.ID }) {
						gone = append(gone, a)
					}
				}

				return gone, nil
			},
		)
	})
}

// refused runs a write that the database cannot take, which must say so with
// ErrUnsupported, or ErrNoCondition first.
func refused(t *testing.T, db *MatrixDB, write func(tx *MatrixDB) error) error {
	var result error

	err := db.Tx(t.Context(), func(tx *MatrixDB) error {
		err := write(tx)
		if !errors.Is(err, ant.ErrUnsupported) && !errors.Is(err, ant.ErrNoCondition) {
			result = fmt.Errorf("got %v, want ErrUnsupported", err)
		}

		return errRollback
	})
	if err != nil && !errors.Is(err, errRollback) {
		return err
	}

	return result
}

// written runs a write in a transaction that rolls back, and compares the rows
// it changed, as changed reads them inside, with want.
func written(
	t *testing.T,
	db *MatrixDB,
	guarded bool,
	want, all []*Author,
	o order,
	p paging,
	write func(tx *MatrixDB) error,
	changed func(tx *MatrixDB) ([]*Author, error),
) error {
	var result error

	err := db.Tx(t.Context(), func(tx *MatrixDB) error {
		err := write(tx)

		switch {
		case guarded && errors.Is(err, ant.ErrNoCondition):
			return errRollback
		case guarded && err == nil:
			result = errors.New("wrote with no condition anywhere, want ErrNoCondition")

			return errRollback
		case err != nil:
			result = err

			return errRollback
		}

		got, err := changed(tx)
		if err != nil {
			result = err

			return errRollback
		}

		// A write keeps the query's rows, but not its order.
		unordered := order{name: o.name}

		if o.less != nil && (p.limit >= 0 || p.offset > 0) {
			slices.SortFunc(got, func(a, b *Author) int { return cmp.Compare(a.ID, b.ID) })

			sorted := slices.Clone(want)
			slices.SortFunc(sorted, func(a, b *Author) int { return cmp.Compare(a.ID, b.ID) })

			if !slices.Equal(ids(got), ids(sorted)) {
				result = fmt.Errorf("changed %v, want %v", ids(got), ids(sorted))
			}

			return errRollback
		}

		result = sameRows(got, want, all, unordered, p)

		return errRollback
	})
	if err != nil && !errors.Is(err, errRollback) {
		return err
	}

	return result
}
