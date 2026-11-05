package tests

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/internal/testdb"
)

// These run on every database, and each covers something the dialects write
// differently. Where a database lacks a feature, the test asks for the error
// instead, because a clear refusal is part of what a dialect promises.

func openAndSeed(t *testing.T, target testdb.Target) *AppDB {
	t.Helper()
	app := openApp(t, target)
	seedUsers(t, app, "alice", 30, "bob", 25, "carol", 35)

	return app
}

func has(
	target testdb.Target,
	f dialect.Feature,
) bool {
	return dialect.Supports(target.Dialect, f)
}

func TestConformancePaging(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()

		page, err := app.Users.OrderBy(&U.Name).Limit(2).Offset(1).All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"bob", "carol"}, names(page))

		rest, err := app.Users.OrderBy(&U.Name).Offset(2).All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"carol"}, names(rest), "OFFSET alone")

		some, err := app.Users.Limit(2).All(ctx)
		require.NoError(t, err)
		require.Len(t, some, 2, "LIMIT with no ORDER BY")

		first, err := app.Users.OrderByDesc(&U.Age).First(ctx)
		require.NoError(t, err)
		require.Equal(t, "carol", first.Name)
	})
}

func TestConformanceConditions(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()

		found, err := app.Users.Where(ant.ILike(&U.Name, "ALI%")).All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"alice"}, names(found), "ILIKE")

		found, err = app.Users.Where(ant.Between(&U.Age, 26, 34)).All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"alice"}, names(found))

		found, err = app.Users.Where(ant.InTuples(
			[]ant.FieldRef{ant.F(&U.Name), ant.F(&U.Age)},
			[]any{"alice", 30},
			[]any{"bob", 99},
		)).
			All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"alice"}, names(found), "multi-column IN")

		exists, err := app.Users.Where(ant.Eq(&U.Name, "bob")).Exists(ctx)
		require.NoError(t, err)
		require.True(t, exists)

		exists, err = app.Users.Where(ant.Eq(&U.Name, "nobody")).Exists(ctx)
		require.NoError(t, err)
		require.False(t, exists)
	})
}

func TestConformanceNullsOrder(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openApp(t, target)
		ctx := t.Context()
		bio := "hello"
		require.NoError(
			t,
			app.Users.Insert(ctx, &User{Name: "with", Email: "with@example.com", Bio: &bio}),
		)

		require.NoError(
			t,
			app.Users.Insert(ctx, &User{Name: "without", Email: "without@example.com"}),
		)

		found, err := app.Users.Order(ant.Col(&U.Bio).Asc().NullsFirst()).All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"without", "with"}, names(found))

		found, err = app.Users.Order(ant.Col(&U.Bio).Asc().NullsLast()).All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"with", "without"}, names(found))
	})
}

func TestConformanceAggregates(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()

		type row struct {
			Total, Adults, Young int64
			Longest              int
		}

		R := ant.Fields[row]()
		rows, err := ant.Project[row](app.Users.Where(ant.Gte(&U.Age, 0)),
			ant.IntoExpr(&R.Total, ant.Count()),
			ant.IntoExpr(&R.Adults, ant.Count().Filter(ant.Gte(&U.Age, 30))),
			ant.IntoExpr(&R.Young, ant.CountOf(&U.Age).Filter(ant.Lt(&U.Age, 30))),
		).All(ctx)
		require.NoError(t, err)
		require.Equal(t, row{Total: 3, Adults: 2, Young: 1}, rows[0], "FILTER")

		lengths, err := ant.Project[row](app.Users.Where(ant.Eq(&U.Name, "carol")),
			ant.IntoExpr(&R.Longest, ant.Length(&U.Name))).All(ctx)
		require.NoError(t, err)
		require.Equal(t, 5, lengths[0].Longest, "LENGTH")
	})
}

func TestConformanceSets(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()
		found, err := app.Users.Where(ant.Eq(&U.Name, "alice")).
			Union(app.Users.Where(ant.Eq(&U.Name, "bob"))).All(ctx)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"alice", "bob"}, names(found))
	})
}

// Wanted is a row that exists only in memory, to join against.
type Wanted struct {
	Name string
	Rank int
}

var W = ant.Fields[Wanted]()

func TestConformanceValuesSource(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()
		wanted := ant.Values(app, Wanted{"alice", 1}, Wanted{"carol", 2})

		listed, err := wanted.OrderBy(&W.Rank).All(ctx)
		require.NoError(t, err)
		require.Equal(t, "carol", listed[1].Name)

		found, err := ant.JoinSub(app.Users.OrderBy(&U.Name), wanted, ant.EqCol(&U.Name, &W.Name)).
			Select(func(u *User, w *Wanted) string { return u.Name }).All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"alice", "carol"}, found)
	})
}

func TestConformanceBulkInsertKeys(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openApp(t, target)
		ctx := t.Context()

		users := []*User{
			{Name: "a", Email: "a@x"},
			{Name: "b", Email: "b@x"},
			{Name: "c", Email: "c@x"},
		}
		n, err := app.Users.InsertMany(ctx, users)
		require.NoError(t, err)
		require.EqualValues(t, 3, n)

		for _, u := range users {
			stored, err := app.Users.Find(ctx, u.ID)
			require.NoError(t, err)
			require.Equal(t, u.Name, stored.Name, "the key belongs to this row")
		}

		// SaveChanges batches these where it can.
		more := []*User{{Name: "d", Email: "d@x"}, {Name: "e", Email: "e@x"}}
		for _, u := range more {
			require.NoError(t, ant.Add(app, u))
		}

		require.NoError(t, app.SaveChanges(ctx))

		for _, u := range more {
			stored, err := app.Users.Find(ctx, u.ID)
			require.NoError(t, err)
			require.Equal(t, u.Name, stored.Name)
			require.False(t, stored.CreatedAt.IsZero())
		}
	})
}

func TestConformanceUpsert(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openApp(t, target)
		ctx := t.Context()

		conflict := ant.OnConflict(&U.Email).DoUpdate(ant.F(&U.Age))

		if !has(target, dialect.OnConflict) {
			_, err := app.Users.Upsert(ctx, &User{Name: "a", Email: "a@x", Age: 1}, conflict)
			require.ErrorIs(t, err, ant.ErrUnsupported)

			return
		}

		first := &User{Name: "first", Email: "same@x", Age: 1}
		inserted, err := app.Users.Upsert(ctx, first, conflict)
		require.NoError(t, err)
		require.True(t, inserted)
		require.NotZero(t, first.ID)

		second := &User{Name: "second", Email: "same@x", Age: 2}
		_, err = app.Users.Upsert(ctx, second, conflict)
		require.NoError(t, err)
		require.Equal(t, first.ID, second.ID, "the stored row came back")
		require.Equal(t, "first", second.Name)

		stored, err := app.Users.NoTracking().Where(ant.Eq(&U.ID, first.ID)).First(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, stored.Age)

		third := &User{Name: "third", Email: "same@x", Age: 3}
		wrote, err := app.Users.Upsert(ctx, third, ant.OnConflict(&U.Email).DoNothing())
		require.NoError(t, err)
		require.False(t, wrote)
		require.Zero(t, third.ID)

		n, err := app.Users.InsertMany(
			ctx,
			[]*User{{Name: "fourth", Email: "same@x"}, {Name: "fifth", Email: "new@x"}},
			ant.WithConflict(ant.OnConflict(&U.Email).DoNothing()),
		)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
}

func TestConformanceReturning(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()

		updated, err := app.Users.Where(ant.Eq(&U.Name, "bob")).
			UpdateReturning(ctx, ant.Set(&U.Age, 26))

		if has(target, dialect.ReturningUpdate) {
			require.NoError(t, err)
			require.Equal(t, 26, updated[0].Age)
		} else {
			require.ErrorIs(t, err, ant.ErrUnsupported)
		}

		removed, err := app.Users.Where(ant.Eq(&U.Name, "bob")).DeleteReturning(ctx)

		if has(target, dialect.ReturningDelete) {
			require.NoError(t, err)
			require.Equal(t, "bob", removed[0].Name)
		} else {
			require.ErrorIs(t, err, ant.ErrUnsupported)
		}
	})
}

func TestConformanceLimitedWrites(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()
		changed, err := app.Users.Where(ant.Gt(&U.Age, 0)).
			OrderBy(&U.Age).
			Limit(1).
			Update(ctx, ant.Set(&U.Age, 99))
		require.NoError(t, err)
		require.EqualValues(t, 1, changed)

		youngest, err := app.Users.NoTracking().Where(ant.Eq(&U.Name, "bob")).First(ctx)
		require.NoError(t, err)
		require.Equal(t, 99, youngest.Age)

		removed, err := app.Users.Where(ant.Gt(&U.Age, 0)).OrderBy(&U.Name).Limit(2).Delete(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 2, removed)

		left, err := app.Users.Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, left)
	})
}

// A bulk write on a derived source selects its rows by key, so the source
// still decides which rows change.
func TestConformanceDerivedWrites(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()
		older := app.Users.Where(ant.Gt(&U.Age, 28))

		age := func(name string) int {
			u, err := app.Users.NoTracking().Where(ant.Eq(&U.Name, name)).First(ctx)
			require.NoError(t, err)

			return u.Age
		}

		// Without paging, the order is left out of the key subquery, where SQL
		// Server refuses it.
		changed, err := app.Users.Query().
			From(older).
			OrderBy(&U.Name).
			Update(ctx, ant.Inc(&U.Age, 1))
		require.NoError(t, err)
		require.EqualValues(t, 2, changed, "From")
		require.Equal(t, 25, age("bob"))

		// A source without the key cannot say which rows it holds. Unchecked, some
		// databases would match the key to the table being written, and change
		// every row.
		_, err = app.Users.Query().
			From(older.Column(&U.Name)).
			Where(ant.Eq(&U.Name, "alice")).
			Update(ctx, ant.Set(&U.Age, 99))
		require.ErrorContains(t, err, "need it to select User.ID")

		// A set operation matches columns by position, so ages from the right side
		// would land in the left side's key.
		_, err = app.Users.Where(ant.Eq(&U.Name, "alice")).
			Column(&U.ID).
			Column(&U.Age).
			Union(app.Users.Where(ant.Eq(&U.Name, "bob")).Column(&U.Age).Column(&U.ID)).
			Delete(ctx)
		require.ErrorContains(t, err, "need it to select User.ID")

		_, err = ant.FromRaw[User](app, "SELECT * FROM users").
			Where(ant.Eq(&U.Name, "bob")).
			Delete(ctx)
		require.ErrorContains(t, err, "cannot use FromRaw")
		require.Equal(t, 25, age("bob"))

		// The key subquery carries the WITH, which SQL Server allows only at the
		// start of a statement.
		changed, err = app.Users.Query().
			FromCTE("older", older).
			Where(ant.Eq(&U.Name, "carol")).
			Update(ctx, ant.Set(&U.Age, 50))

		if has(target, dialect.SubqueryWith) {
			require.NoError(t, err)
			require.EqualValues(t, 1, changed, "FromCTE")
		} else {
			require.ErrorIs(t, err, ant.ErrUnsupported)
		}

		require.Equal(t, 31, age("alice"))

		removed, err := app.Users.Where(ant.Eq(&U.Name, "alice")).
			Union(app.Users.Where(ant.Eq(&U.Name, "bob"))).
			Delete(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 2, removed, "Union")

		left, err := app.Users.All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"carol"}, names(left))
	})
}

func TestConformanceLocks(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		err := app.Tx(t.Context(), func(tx *AppDB) error {
			_, err := tx.Users.Where(ant.Eq(&U.Name, "bob")).ForUpdate().First(t.Context())

			return err
		})

		if has(target, dialect.RowLocks) {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ant.ErrUnsupported)
		}
	})
}

func TestConformanceMissingFeatures(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()
		_, err := app.Users.FullJoinOn[Post](ant.EqCol(&U.ID, &P.UserID)).
			Select(func(u *User, p *Post) string { return u.Name }).All(ctx)

		if has(target, dialect.FullJoin) {
			require.NoError(t, err)

			return
		}

		require.ErrorIs(t, err, ant.ErrUnsupported)
		require.Contains(t, err.Error(), target.Dialect.Name(), "the error names the database")
	})
}

func TestConformanceSavepoints(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openApp(t, target)
		ctx := t.Context()
		err := app.Tx(ctx, func(tx *AppDB) error {
			require.NoError(t, tx.Users.Insert(ctx, &User{Name: "kept", Email: "kept@x"}))

			inner := tx.Tx(ctx, func(nested *AppDB) error {
				require.NoError(
					t,
					nested.Users.Insert(ctx, &User{Name: "undone", Email: "undone@x"}),
				)

				return sql.ErrNoRows
			})
			require.ErrorIs(t, inner, sql.ErrNoRows)

			return tx.Users.Insert(ctx, &User{Name: "after", Email: "after@x"})
		})
		require.NoError(t, err)
		found, err := app.Users.OrderBy(&U.Name).All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"after", "kept"}, names(found))
	})
}

func TestConformanceErrors(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()

		err := app.Users.Insert(ctx, &User{Name: "copy", Email: "alice@example.com"})
		require.Error(t, err)
		require.True(t, ant.IsUniqueViolation(err), "%v", err)

		err = app.Posts.Insert(ctx, &Post{UserID: 999999, Title: "orphan"})
		require.Error(t, err)
		require.True(t, ant.IsForeignKeyViolation(err), "%v", err)
	})
}

func TestConformanceTimes(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openApp(t, target)
		ctx := t.Context()
		before := time.Now().Add(-time.Minute)
		u := &User{Name: "t", Email: "t@x"}
		require.NoError(t, app.Users.Insert(ctx, u))
		stored, err := app.Users.Find(ctx, u.ID)
		require.NoError(t, err)
		require.WithinDuration(t, time.Now(), stored.CreatedAt, 2*time.Minute)

		found, err := app.Users.Where(ant.Gt(&U.CreatedAt, before)).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, found, "a time compares as a time")
	})
}

func TestConformanceQuoting(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openApp(t, target)
		ctx := t.Context()
		tricky := `it's "quoted" ` + "`and`" + ` [bracketed]`
		u := &User{Name: tricky, Email: strings.Repeat("e", 40) + "@x"}
		require.NoError(t, app.Users.Insert(ctx, u))
		stored, err := app.Users.Where(ant.Eq(&U.Name, tricky)).First(ctx)
		require.NoError(t, err)
		require.Equal(t, tricky, stored.Name)
	})
}

func TestConformanceDates(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()

		// An hour away, as CURRENT_TIMESTAMP has whole seconds on some databases.
		for _, shift := range []time.Duration{-time.Hour, time.Hour} {
			_, err := app.Users.Where(ant.Gt(&U.Age, 0)).
				Update(ctx, ant.Set(&U.CreatedAt, time.Now().Add(shift)))
			require.NoError(t, err)
			past, err := app.Users.Where(ant.Past(&U.CreatedAt)).Count(ctx)
			require.NoError(t, err)
			future, err := app.Users.Where(ant.Future(&U.CreatedAt)).Count(ctx)
			require.NoError(t, err)

			if shift < 0 {
				require.EqualValues(t, []int64{3, 0}, []int64{past, future})
			} else {
				require.EqualValues(t, []int64{0, 3}, []int64{past, future})
			}
		}

		_, err := app.Users.Where(ant.Today(&U.CreatedAt)).Count(ctx)

		if has(target, dialect.DateCast) {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ant.ErrUnsupported)
		}

		type row struct{ Year float64 }

		R := ant.Fields[row]()

		years, err := ant.Project[row](
			app.Users.Limit(1),
			ant.IntoExpr(&R.Year, ant.DatePart(&U.CreatedAt, "year")),
		).All(ctx)

		if has(target, dialect.DatePart) {
			require.NoError(t, err)
			require.InDelta(t, time.Now().Year(), years[0].Year, 1)
		} else {
			require.ErrorIs(t, err, ant.ErrUnsupported)
		}
	})
}

func TestConformanceRandomOrder(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		found, err := app.Users.Query().OrderByRandom().All(t.Context())
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"alice", "bob", "carol"}, names(found))
	})
}

func TestConformanceCursorPaging(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()
		first, err := app.Users.OrderBy(&U.ID).After(ctx, "", 2)
		require.NoError(t, err)
		require.Equal(t, []string{"alice", "bob"}, names(first.Items))
		next, err := app.Users.OrderBy(&U.ID).After(ctx, first.Next, 2)
		require.NoError(t, err)
		require.Equal(t, []string{"carol"}, names(next.Items))
	})
}

func TestConformanceGroups(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openApp(t, target)
		ctx := t.Context()
		seedUsers(t, app, "a", 20, "b", 20, "c", 30)

		type bucket struct {
			Age   int
			Users int64
		}

		B := ant.Fields[bucket]()
		got, err := ant.Project[bucket](
			app.Users.GroupBy(&U.Age).Having(ant.Count().Gt(1)),
			ant.Into(&B.Age, &U.Age),
			ant.IntoExpr(&B.Users, ant.Count()),
		).All(ctx)
		require.NoError(t, err)
		require.Equal(t, []bucket{{20, 2}}, got)
	})
}

func TestConformanceWindows(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)

		type row struct {
			Name     string
			Position int64
		}

		R := ant.Fields[row]()
		got, err := ant.Project[row](
			app.Users.OrderBy(&U.Name),
			ant.Into(&R.Name, &U.Name),
			ant.IntoExpr(
				&R.Position,
				ant.RowNumber().Over(ant.OrderedBy(ant.Col(&U.Age).Asc())),
			),
		).All(t.Context())
		require.NoError(t, err)
		require.Equal(t, []row{{"alice", 2}, {"bob", 1}, {"carol", 3}}, got)
	})
}

func TestConformanceRelations(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()
		users, err := app.Users.OrderBy(&U.Name).NoTracking().All(ctx)
		require.NoError(t, err)

		for i, title := range []string{"one", "two", "three"} {
			require.NoError(
				t,
				app.Posts.Insert(ctx, &Post{UserID: users[0].ID, Title: title, Published: i > 0}),
			)
		}

		writers, err := app.Users.Where(ant.Has(&U.Posts, ant.Eq(&P.Published, true))).
			NoTracking().
			All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"alice"}, names(writers), "EXISTS with a boolean")

		top, err := app.Users.Where(ant.Eq(&U.Name, "alice")).NoTracking().
			Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
				return q.OrderByDesc(&P.Title).Limit(2)
			}).
			All(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"two", "three"}, titles(top[0].Posts), "a limit per parent")
	})
}

func TestConformanceMerge(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()
		source := ant.Values(app, Wanted{"alice", 41})
		merge := ant.Merge(&app.Users, source, ant.EqCol(&U.Name, &W.Name)).
			WhenMatched(ant.SetExpr(&U.Age, ant.Col(&W.Rank)))
		changed, err := merge.Exec(ctx)

		if !has(target, dialect.Merge) {
			require.ErrorIs(t, err, ant.ErrUnsupported)

			return
		}

		if target.Name == "postgres" {
			version, err := ant.Raw[int](
				app,
				"SELECT current_setting('server_version_num')::int",
			).First(ctx)
			require.NoError(t, err)

			if *version < 150000 {
				t.Skip("MERGE needs PostgreSQL 15")
			}
		}

		require.NoError(t, err)
		require.EqualValues(t, 1, changed)

		alice, err := app.Users.NoTracking().Where(ant.Eq(&U.Name, "alice")).First(ctx)
		require.NoError(t, err)
		require.Equal(t, 41, alice.Age)
	})
}

// More rows than any database takes parameters for in one statement: SQL Server
// stops at 2100, so inserts and loads have to split their work.
func TestConformanceParameterLimits(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openApp(t, target)
		ctx := t.Context()

		users := make([]*User, 2500)
		for i := range users {
			users[i] = &User{Name: "u", Email: fmt.Sprintf("u%d@x", i), Age: 20}
		}

		_, err := app.Users.InsertMany(ctx, users)
		require.NoError(t, err)

		require.NoError(
			t,
			app.Posts.Insert(ctx, &Post{UserID: users[len(users)-1].ID, Title: "last"}),
		)

		loaded, err := app.Users.NoTracking().OrderBy(&U.ID).Include(&U.Posts).All(ctx)
		require.NoError(t, err)
		require.Len(t, loaded, len(users))
		require.Equal(t, []string{"last"}, titles(loaded[len(loaded)-1].Posts))
	})
}

func TestConformanceLateralJoin(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openAndSeed(t, target)
		ctx := t.Context()
		alice, err := app.Users.NoTracking().Where(ant.Eq(&U.Name, "alice")).First(ctx)
		require.NoError(t, err)

		for _, title := range []string{"older", "newer"} {
			require.NoError(t, app.Posts.Insert(ctx, &Post{UserID: alice.ID, Title: title}))
		}

		first := app.Posts.Where(ant.EqCol(&P.UserID, &U.ID)).OrderBy(&P.Title).Limit(1)

		newest, err := ant.JoinLateral(app.Users.Query(), first).
			Select(func(u *User, p *Post) string { return p.Title }).
			All(ctx)

		if !has(target, dialect.LateralJoin) {
			require.ErrorIs(t, err, ant.ErrUnsupported)

			return
		}

		require.NoError(t, err)
		require.Equal(t, []string{"newer"}, newest)
	})
}

// A set operation's members keep their own ordering and paging: the youngest
// two users, united with the oldest one, are three users, not the youngest of
// all six. Every pair of member shapes is checked against the same rows worked
// out in Go, for each kind of set operation.
func TestConformanceSetMembers(t *testing.T) {
	people := []struct {
		name string
		age  int
	}{{"alice", 30}, {"bob", 25}, {"carol", 35}, {"dan", 40}, {"eve", 20}}

	// pick works out a member's rows in Go, from people sorted by less.
	pick := func(keep func(age int) bool, less func(a, b int) bool, offset, limit int) []string {
		var rows []int

		for i, p := range people {
			if keep(p.age) {
				rows = append(rows, i)
			}
		}

		slices.SortFunc(rows, func(a, b int) int {
			if less(a, b) {
				return -1
			}

			return 1
		})

		rows = rows[min(offset, len(rows)):]

		if limit >= 0 {
			rows = rows[:min(limit, len(rows))]
		}

		out := make([]string, len(rows))

		for i, r := range rows {
			out[i] = people[r].name
		}

		return out
	}

	all := func(int) bool { return true }
	byAge := func(a, b int) bool { return people[a].age < people[b].age }
	byAgeDesc := func(a, b int) bool { return people[a].age > people[b].age }
	byName := func(a, b int) bool { return people[a].name < people[b].name }

	members := []struct {
		name  string
		shape func(q *ant.Query[User]) *ant.Query[User]
		rows  []string
	}{
		{
			"filtered",
			func(q *ant.Query[User]) *ant.Query[User] { return q.Where(ant.Gt(&U.Age, 22)) },
			pick(func(age int) bool { return age > 22 }, byName, 0, -1),
		},
		{
			"youngest two",
			func(q *ant.Query[User]) *ant.Query[User] { return q.OrderBy(&U.Age).Limit(2) },
			pick(all, byAge, 0, 2),
		},
		{
			"second oldest",
			func(q *ant.Query[User]) *ant.Query[User] {
				return q.OrderByDesc(&U.Age).Offset(1).Limit(1)
			},
			pick(all, byAgeDesc, 1, 1),
		},
		{
			"ordered, not paged",
			func(q *ant.Query[User]) *ant.Query[User] { return q.OrderByDesc(&U.Name) },
			pick(all, byName, 0, -1),
		},
		{
			"offset alone",
			func(q *ant.Query[User]) *ant.Query[User] { return q.OrderBy(&U.Name).Offset(3) },
			pick(all, byName, 3, -1),
		},
		{
			"filtered and paged",
			func(q *ant.Query[User]) *ant.Query[User] {
				return q.Where(ant.Lt(&U.Age, 30)).OrderByDesc(&U.Age).Limit(1)
			},
			pick(func(age int) bool { return age < 30 }, byAgeDesc, 0, 1),
		},
	}

	kinds := []struct {
		name    string
		combine func(a, b *ant.Query[User]) *ant.Query[User]
		rows    func(a, b []string) []string
	}{
		{"Union", (*ant.Query[User]).Union, func(a, b []string) []string {
			return slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(a), b...))))
		}},
		{"UnionAll", (*ant.Query[User]).UnionAll, func(a, b []string) []string {
			return slices.Sorted(slices.Values(append(slices.Clone(a), b...)))
		}},
		{"Intersect", (*ant.Query[User]).Intersect, func(a, b []string) []string {
			return slices.Sorted(
				slices.Values(slices.DeleteFunc(slices.Clone(a), func(s string) bool {
					return !slices.Contains(b, s)
				})),
			)
		}},
		{"Except", (*ant.Query[User]).Except, func(a, b []string) []string {
			return slices.Sorted(
				slices.Values(slices.DeleteFunc(slices.Clone(a), func(s string) bool {
					return slices.Contains(b, s)
				})),
			)
		}},
	}

	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		app := openApp(t, target)
		ctx := t.Context()

		for _, p := range people {
			seedUsers(t, app, p.name, p.age)
		}

		for _, kind := range kinds {
			for _, left := range members {
				for _, right := range members {
					name := kind.name + "/" + left.name + "/" + right.name

					t.Run(name, func(t *testing.T) {
						q := kind.combine(
							left.shape(app.Users.Query()),
							right.shape(app.Users.Query()),
						)

						got, err := q.OrderBy(&U.Name).All(ctx)
						require.NoError(t, err)

						want := kind.rows(left.rows, right.rows)
						if want == nil {
							want = []string{}
						}

						require.Equal(t, want, names(got))
					})
				}
			}
		}

		youngTwo := app.Users.OrderBy(&U.Age).Limit(2)   // eve, bob
		oldest := app.Users.OrderByDesc(&U.Age).Limit(1) // dan
		thirty := app.Users.Where(ant.Eq(&U.Age, 30))    // alice

		byName := func(t *testing.T, q *ant.Query[User]) []string {
			t.Helper()

			got, err := q.OrderBy(&U.Name).All(ctx)
			require.NoError(t, err)

			return names(got)
		}

		t.Run("nested sets", func(t *testing.T) {
			want := []string{"alice", "bob", "dan", "eve"}
			require.Equal(t, want, byName(t, youngTwo.Union(oldest).Union(thirty)))
			require.Equal(t, want, byName(t, youngTwo.Union(oldest.Union(thirty))))
		})

		t.Run("a paged set as a member", func(t *testing.T) {
			firstTwo := youngTwo.Union(oldest).OrderBy(&U.Name).Limit(2) // bob, dan
			require.Equal(t, []string{"alice", "bob", "dan"}, byName(t, firstTwo.Union(thirty)))
			require.Equal(t, []string{"bob"}, byName(t, firstTwo.Intersect(youngTwo)))
		})

		t.Run("counted", func(t *testing.T) {
			n, err := youngTwo.Union(oldest).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 3, n)
		})

		// A limit is not a condition, so each side has one for the write guard.
		t.Run("an update changes the members' rows only", func(t *testing.T) {
			adults := app.Users.Where(ant.Gt(&U.Age, 0))
			changed, err := adults.OrderBy(&U.Age).Limit(2).
				Union(adults.OrderByDesc(&U.Age).Limit(1)).
				Update(ctx, ant.Inc(&U.Age, 100))
			require.NoError(t, err)
			require.EqualValues(t, 3, changed)

			old, err := app.Users.Where(ant.Gt(&U.Age, 100)).OrderBy(&U.Name).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"bob", "dan", "eve"}, names(old))

			_, err = app.Users.Where(ant.Gt(&U.Age, 100)).Update(ctx, ant.Inc(&U.Age, -100))
			require.NoError(t, err)
		})

		t.Run("a delete removes the members' rows only", func(t *testing.T) {
			removed, err := app.Users.OrderBy(&U.Age).Limit(3). // eve, bob, alice
										Intersect(app.Users.Where(ant.Gt(&U.Age, 22))).
										Delete(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, removed)
			require.Equal(t, []string{"carol", "dan", "eve"}, byName(t, app.Users.Query()))
		})
	})
}

// Ledger is a versioned model with a unique code, for upserts.
type Ledger struct {
	ID      int64
	Code    string
	Total   int
	Version int64 `ant:"version"`
}

func (Ledger) Configure(b *ant.ModelBuilder[Ledger]) {
	b.Table("ledgers")
	b.Index(&Lg.Code).Unique()
}

var Lg = ant.Fields[Ledger]()

type LedgerDB struct {
	ant.Context
	Ledgers ant.Table[Ledger]
}

// An upsert that updates moves the version on, as every write does. Where the
// database takes a condition on an upsert, a copy that carries a version updates
// only that version. ON DUPLICATE KEY takes none, so on MySQL and MariaDB the
// copy's update goes ahead.
func TestConformanceUpsertVersion(t *testing.T) {
	testdb.ForEach(t, func(t *testing.T, target testdb.Target) {
		ctx := t.Context()
		_, err := target.DB.ExecContext(ctx, `CREATE TABLE ledgers (
			id BIGINT PRIMARY KEY,
			code VARCHAR(20) NOT NULL UNIQUE,
			total INT NOT NULL,
			version BIGINT NOT NULL)`)
		require.NoError(t, err)

		t.Cleanup(func() {
			_, _ = target.DB.ExecContext(context.Background(), "DROP TABLE ledgers")
		})

		db, err := ant.Open[LedgerDB](ctx, target.Dialect, target.DSN)
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		onCode := ant.OnConflict(&Lg.Code)

		if !has(target, dialect.OnConflict) {
			_, err := db.Ledgers.Upsert(ctx, &Ledger{ID: 1, Code: "a"}, onCode)
			require.ErrorIs(t, err, ant.ErrUnsupported)

			return
		}

		version := func() int64 {
			stored, err := db.Ledgers.NoTracking().Where(ant.Eq(&Lg.Code, "a")).First(ctx)
			require.NoError(t, err)

			return stored.Version
		}

		_, err = db.Ledgers.Upsert(ctx, &Ledger{ID: 1, Code: "a", Total: 1}, onCode)
		require.NoError(t, err)
		require.EqualValues(t, 1, version())

		for i, conflict := range []*ant.Conflict{
			onCode,
			onCode.DoUpdate(ant.F(&Lg.Total)),
			onCode.Set(ant.Inc(&Lg.Total, 1)),
		} {
			fresh := &Ledger{ID: 2, Code: "a", Total: 5}
			_, err = db.Ledgers.Upsert(ctx, fresh, conflict)
			require.NoError(t, err)
			require.EqualValues(t, i+2, version(), "the version moves on, not back to 1")
			require.EqualValues(t, i+2, fresh.Version)
		}

		_, err = db.Ledgers.Upsert(
			ctx,
			&Ledger{
				ID:      1,
				Code:    "a",
				Total:   9,
				Version: 1,
			},
			onCode,
		)

		if has(target, dialect.ConflictWhere) {
			require.ErrorIs(t, err, ant.ErrConcurrency, "a stale copy is refused")
			require.EqualValues(t, 4, version())
		} else {
			require.NoError(t, err, "ON DUPLICATE KEY has no condition")
			require.EqualValues(t, 5, version())
		}

		n, err := db.Ledgers.InsertMany(ctx,
			[]*Ledger{{ID: 3, Code: "a", Total: 1}, {ID: 4, Code: "b", Total: 1}},
			ant.WithConflict(onCode))
		require.NoError(t, err)
		require.Positive(t, n)

		stored, err := db.Ledgers.NoTracking().OrderBy(&Lg.Code).All(ctx)
		require.NoError(t, err)
		require.Equal(t, version(), stored[0].Version)
		require.Greater(t, stored[0].Version, int64(1), "InsertMany moved it on too")
		require.EqualValues(t, 1, stored[1].Version)
	})
}
