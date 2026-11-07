package querying

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// A subquery is a query used inside another.
func TestSubqueries(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		alice, _, _ := fixture.Seed(t, db)
		ctx := t.Context()
		require.NoError(t, db.Posts.Insert(ctx, &fixture.Post{UserID: alice.ID, Title: "Hello"}))
		authors := ant.SubOf(db.Posts.Where(), &P.UserID)

		t.Run("InSub", func(t *testing.T) {
			got, err := db.Users.Where(ant.InSub(&U.ID, authors)).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice"}, fixture.Names(got))
		})

		t.Run("NotInSub", func(t *testing.T) {
			got, err := db.Users.Where(ant.NotInSub(&U.ID, authors)).OrderBy(&U.Name).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Bob", "Carol"}, fixture.Names(got))
		})

		t.Run("Exists can refer to the outer row", func(t *testing.T) {
			wrote := db.Posts.Where(ant.EqCol(&P.UserID, &U.ID))
			got, err := db.Users.Where(ant.Exists(wrote)).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice"}, fixture.Names(got))

			got, err = db.Users.Where(ant.NotExists(wrote)).OrderBy(&U.Name).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Bob", "Carol"}, fixture.Names(got))
		})

		t.Run("a subquery as a value", func(t *testing.T) {
			oldest := ant.SubExpr(db.Users.Where(), ant.Max(&U.Age)).Expr()
			got, err := db.Users.Where(ant.Col(&U.Age).EqExpr(oldest)).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Carol"}, fixture.Names(got))
		})

		t.Run("ordering by a subquery", func(t *testing.T) {
			posts := ant.SubExpr(db.Posts.Where(ant.EqCol(&P.UserID, &U.ID)), ant.Count()).Expr()
			got, err := db.Users.Order(posts.Desc(), ant.Col(&U.Name).Asc()).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice", "Bob", "Carol"}, fixture.Names(got))
		})

		t.Run("From reads from a subquery", func(t *testing.T) {
			adults := db.Users.Where(ant.Gt(&U.Age, 18))
			got, err := db.Users.Query().From(adults).Where(ant.Lt(&U.Age, 35)).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice"}, fixture.Names(got))
		})

		t.Run("a bad subquery is reported", func(t *testing.T) {
			x := 1
			//antvet:ignore not a probe, on purpose: the test checks the error
			bad := ant.SubOf(db.Posts.Where(ant.Gt(&x, 0)), &P.UserID)
			_, err := db.Users.Where(ant.InSub(&U.ID, bad)).All(ctx)
			require.ErrorIs(t, err, ant.ErrNotProbe)
		})

		t.Run("a subquery referring to a model that is nowhere", func(t *testing.T) {
			type other struct{ ID int64 }

			o := ant.Fields[other]()
			sub := ant.SubOf(db.Posts.Where(ant.Eq(&o.ID, int64(1))), &P.UserID)
			_, err := db.Users.Where(ant.InSub(&U.ID, sub)).All(ctx)
			require.ErrorIs(t, err, ant.ErrWrongModel)
		})

		t.Run("Include cannot be a subquery", func(t *testing.T) {
			_, err := db.Users.Query().From(db.Users.Include(&U.Posts)).All(ctx)
			require.ErrorIs(t, err, ant.ErrIncludeInSubquery)
		})
	})
}
