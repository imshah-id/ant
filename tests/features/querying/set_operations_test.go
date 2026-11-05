package querying

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Union, Intersect, and Except combine queries of one model. Each side keeps its
// own paging, and conditions and ordering added afterward apply to the combined
// rows.
func TestSetOperations(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		fixture.Seed(t, db)
		ctx := t.Context()
		young := db.Users.Where(ant.Lt(&U.Age, 35))
		old := db.Users.Where(ant.Gt(&U.Age, 25))

		t.Run("Union removes duplicates", func(t *testing.T) {
			got, err := young.Union(old).OrderBy(&U.Name).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice", "Bob", "Carol"}, fixture.Names(got))
		})

		t.Run("UnionAll keeps them", func(t *testing.T) {
			got, err := young.UnionAll(old).OrderBy(&U.Name).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice", "Alice", "Bob", "Carol"}, fixture.Names(got))
		})

		t.Run("Intersect", func(t *testing.T) {
			got, err := young.Intersect(old).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice"}, fixture.Names(got))
		})

		t.Run("Except", func(t *testing.T) {
			got, err := young.Except(old).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Bob"}, fixture.Names(got))
		})

		t.Run("the result is a normal query", func(t *testing.T) {
			n, err := young.Union(old).Where(ant.Ne(&U.Name, "Bob")).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, n)

			got, err := young.Union(old).OrderByDesc(&U.Age).Limit(1).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Carol"}, fixture.Names(got))
		})

		// A side's own ordering and paging pick its rows before they combine:
		// the youngest user, and the oldest.
		t.Run("each side keeps its own paging", func(t *testing.T) {
			youngest := db.Users.OrderBy(&U.Age).Limit(1)
			oldest := db.Users.OrderByDesc(&U.Age).Limit(1)

			got, err := youngest.Union(oldest).OrderBy(&U.Name).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Bob", "Carol"}, fixture.Names(got))
		})

		t.Run("chained", func(t *testing.T) {
			got, err := young.Union(old).
				Except(db.Users.Where(ant.Eq(&U.Name, "Bob"))).
				OrderBy(&U.Name).
				All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice", "Carol"}, fixture.Names(got))
		})

		t.Run("an error in either side is reported", func(t *testing.T) {
			x := 1
			//antvet:ignore not a probe, on purpose: the test checks the error
			_, err := young.Union(db.Users.Where(ant.Gt(&x, 0))).All(ctx)
			require.ErrorIs(t, err, ant.ErrNotProbe)
		})
	})
}
