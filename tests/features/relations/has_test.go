package relations

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Has filters by what a row relates to, without loading it.
func TestHas(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		seed(t, db)
		ctx := t.Context()
		names := func(c ...ant.Cond) []string {
			got, err := db.Users.Where(c...).OrderBy(&U.Name).All(ctx)
			require.NoError(t, err)

			return fixture.Names(got)
		}

		t.Run("any related row", func(t *testing.T) {
			require.Equal(t, []string{"Alice", "Bob"}, names(ant.Has(&U.Posts)))
		})

		t.Run("a related row that meets conditions", func(t *testing.T) {
			require.Equal(t, []string{"Alice"}, names(ant.Has(&U.Posts, ant.Eq(&P.Title, "Draft"))))
		})

		t.Run("HasNone", func(t *testing.T) {
			require.Equal(t, []string{"Carol"}, names(ant.HasNone(&U.Posts)))

			require.Equal(
				t,
				[]string{"Bob", "Carol"},
				names(ant.HasNone(&U.Posts, ant.Eq(&P.Title, "Draft"))),
			)
		})

		t.Run("HasOne follows a many-to-one relation", func(t *testing.T) {
			got := titles(
				t,
				db,
				db.Posts.Where(ant.HasOne(&P.User, ant.Gt(&U.Age, 18))).OrderBy(&P.Title),
			)
			require.Equal(t, []string{"Draft", "Hello"}, got)
		})

		t.Run("combines with other conditions", func(t *testing.T) {
			require.Equal(t, []string{"Bob"}, names(ant.Has(&U.Posts), ant.Lt(&U.Age, 18)))
			require.Equal(t, []string{"Carol"}, names(ant.Not(ant.Has(&U.Posts))))
		})

		t.Run("an error in a nested condition is reported", func(t *testing.T) {
			x := 1
			//antvet:ignore not a probe, on purpose: the test checks the error
			_, err := db.Users.Where(ant.Has(&U.Posts, ant.Gt(&x, 0))).All(ctx)
			require.ErrorIs(t, err, ant.ErrNotProbe)
		})

		t.Run("a relation of another model", func(t *testing.T) {
			_, err := db.Posts.Where(ant.Has(&U.Posts)).All(ctx)
			require.ErrorIs(t, err, ant.ErrWrongModel)
		})
	})
}
