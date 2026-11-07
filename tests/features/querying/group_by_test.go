package querying

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

type bucket struct {
	Age     int
	Users   int64
	Longest int
}

var B = ant.Fields[bucket]()

// GroupBy returns aggregates per group. Project says what each result holds.
func TestGroupBy(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		ctx := t.Context()

		for i, age := range []int{20, 20, 20, 30, 30, 40} {
			name := string(rune('a' + i))
			require.NoError(
				t,
				db.Users.Insert(ctx, &fixture.User{Name: name, Email: name, Age: age}),
			)
		}

		t.Run("a column and an aggregate", func(t *testing.T) {
			got, err := ant.Project[bucket](
				db.Users.GroupBy(&U.Age).Order(ant.Col(&U.Age).Asc()),
				ant.Into(&B.Age, &U.Age),
				ant.IntoExpr(&B.Users, ant.Count()),
			).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []bucket{{20, 3, 0}, {30, 2, 0}, {40, 1, 0}}, got)
		})

		t.Run("Having filters groups", func(t *testing.T) {
			got, err := ant.Project[bucket](
				db.Users.GroupBy(&U.Age).Having(ant.Count().Gt(1)).Order(ant.Count().Desc()),
				ant.Into(&B.Age, &U.Age),
			).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []bucket{{Age: 20}, {Age: 30}}, got)
		})

		t.Run("Where filters rows first", func(t *testing.T) {
			got, err := ant.Project[bucket](
				db.Users.Where(ant.Gt(&U.Age, 20)).GroupBy(&U.Age).Order(ant.Col(&U.Age).Asc()),
				ant.Into(&B.Age, &U.Age),
				ant.IntoExpr(&B.Users, ant.Count()),
			).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []bucket{{30, 2, 0}, {40, 1, 0}}, got)
		})

		t.Run("order an aggregate and limit", func(t *testing.T) {
			got, err := ant.Project[bucket](
				db.Users.GroupBy(&U.Age).Order(ant.Count().Asc()).Limit(1),
				ant.Into(&B.Age, &U.Age),
			).First(ctx)
			require.NoError(t, err)
			require.Equal(t, 40, got.Age)
		})

		t.Run("several columns", func(t *testing.T) {
			g := ant.Then(db.Users.GroupBy(&U.Age), &U.Name)
			got, err := ant.Project[bucket](g, ant.Into(&B.Age, &U.Age)).All(ctx)
			require.NoError(t, err)
			require.Len(t, got, 6)
		})

		t.Run("an expression", func(t *testing.T) {
			decade := ant.SQL[int]("? / 20", ant.Col(&U.Age))
			got, err := ant.Project[bucket](
				db.Users.GroupByExpr(decade).Order(decade.Asc()),
				ant.IntoExpr(&B.Age, decade),
				ant.IntoExpr(&B.Users, ant.Count()),
			).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []bucket{{1, 5, 0}, {2, 1, 0}}, got)
		})

		t.Run("a filtered aggregate", func(t *testing.T) {
			got, err := ant.Project[bucket](
				db.Users.GroupBy(&U.Age).Order(ant.Col(&U.Age).Asc()).Limit(1),
				ant.Into(&B.Age, &U.Age),
				ant.IntoExpr(&B.Users, ant.Count().Filter(ant.Eq(&U.Name, "a"))),
			).First(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, got.Users)
		})

		t.Run("across a join", func(t *testing.T) {
			owner, err := db.Users.First(ctx)
			require.NoError(t, err)
			require.NoError(t, db.Posts.Insert(ctx, &fixture.Post{UserID: owner.ID, Title: "x"}))

			got, err := ant.Project[bucket](
				db.Posts.Join(&P.User).GroupBy(&U.Age),
				ant.IntoExpr(&B.Users, ant.Count()),
			).All(ctx)
			require.NoError(t, err)
			require.Len(t, got, 1)
		})

		t.Run("a bad Having is reported", func(t *testing.T) {
			x := 1
			_, err := ant.Project[bucket](
				//antvet:ignore not a probe, on purpose: the test checks the error
				db.Users.GroupBy(&U.Age).Having(ant.Gt(&x, 0)),
				ant.Into(&B.Age, &U.Age),
			).All(ctx)
			require.ErrorIs(t, err, ant.ErrNotProbe)
		})
	})
}
