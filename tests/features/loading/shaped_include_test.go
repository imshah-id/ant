package loading

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// The optional function shapes the related query, with the same methods as any
// other query.
func TestShapedInclude(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		seed(t, db)
		ctx := t.Context()
		alice := ant.Eq(&U.Name, "Alice")

		t.Run("filter", func(t *testing.T) {
			got, err := db.Users.Where(alice).
				Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
					return q.Where(ant.Eq(&P.Published, true))
				}).
				Single(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"A1"}, titles(got.Posts))
		})

		t.Run("order", func(t *testing.T) {
			got, err := db.Users.Where(alice).
				Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
					return q.OrderByDesc(&P.Title)
				}).
				Single(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"A3", "A2", "A1"}, titles(got.Posts))
		})

		t.Run("Limit applies per parent", func(t *testing.T) {
			users, err := db.Users.OrderBy(&U.Name).
				Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
					return q.OrderByDesc(&P.Title).Limit(2)
				}).
				All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"A3", "A2"}, titles(users[0].Posts))
			require.Equal(t, []string{"B1"}, titles(users[1].Posts))
			require.Empty(t, users[2].Posts)
		})

		t.Run("Offset applies per parent", func(t *testing.T) {
			users, err := db.Users.OrderBy(&U.Name).
				Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
					return q.OrderBy(&P.Title).Offset(1).Limit(1)
				}).
				All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"A2"}, titles(users[0].Posts))
			require.Empty(t, users[1].Posts)
		})

		t.Run("chosen columns keep the foreign key", func(t *testing.T) {
			got, err := db.Users.Where(alice).
				Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
					return q.Column(&P.Title).OrderBy(&P.Title)
				}).
				Single(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"A1", "A2", "A3"}, titles(got.Posts))
			require.Zero(t, got.Posts[0].ID)
		})

		t.Run("Omit cannot drop the foreign key", func(t *testing.T) {
			got, err := db.Users.Where(alice).
				Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
					return q.Omit(&P.UserID)
				}).
				Single(ctx)
			require.NoError(t, err)
			require.Len(t, got.Posts, 3)
		})

		t.Run("an error inside the function reaches the terminal call", func(t *testing.T) {
			x := 1
			_, err := db.Users.Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
				//antvet:ignore not a probe, on purpose: the test checks the error
				return q.Where(ant.Gt(&x, 0))
			}).All(ctx)
			require.ErrorIs(t, err, ant.ErrNotProbe)
		})
	})
}

// Include inside the function loads deeper levels.
func TestNestedInclude(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		seed(t, db)
		ctx := t.Context()

		t.Run("down then up", func(t *testing.T) {
			got, err := db.Users.Where(ant.Eq(&U.Name, "Bob")).
				Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
					return q.IncludeOne(&P.User)
				}).Single(ctx)
			require.NoError(t, err)
			require.Equal(t, "Bob", got.Posts[0].User.Name)
		})

		t.Run("up then down", func(t *testing.T) {
			got, err := db.Posts.Where(ant.Eq(&P.Title, "B1")).
				IncludeOne(&P.User, func(q *ant.Query[User]) *ant.Query[User] {
					return q.Include(&U.Posts)
				}).Single(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"B1"}, titles(got.User.Posts))
		})

		t.Run("several relations on one query", func(t *testing.T) {
			got, err := db.Posts.Where(ant.Eq(&P.Title, "B1")).
				IncludeOne(&P.User, func(q *ant.Query[User]) *ant.Query[User] {
					return q.Include(&U.Posts).Include(&U.Posts)
				}).Single(ctx)
			require.NoError(t, err)
			require.Len(t, got.User.Posts, 1, "including twice does not double the rows")
		})
	})
}
