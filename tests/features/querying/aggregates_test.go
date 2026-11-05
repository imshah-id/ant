package querying

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Count, Exists, and the aggregate functions run one query and return a value.
func TestAggregates(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		ctx := t.Context()
		adults := db.Users.Where(ant.Gt(&U.Age, 18))

		t.Run("empty table", func(t *testing.T) {
			n, err := db.Users.Count(ctx)
			require.NoError(t, err)
			require.Zero(t, n)

			found, err := db.Users.Exists(ctx)
			require.NoError(t, err)
			require.False(t, found)

			none, err := adults.DoesNotExist(ctx)
			require.NoError(t, err)
			require.True(t, none)
		})

		// SUM over no rows is NULL in SQL, and zero here.
		t.Run("aggregates over no rows are zero", func(t *testing.T) {
			sum, err := ant.SumOf(ctx, adults, &U.Age)
			require.NoError(t, err)
			require.Zero(t, sum)

			avg, err := ant.AvgOf(ctx, adults, &U.Age)
			require.NoError(t, err)
			require.Zero(t, avg)

			largest, err := ant.MaxOf(ctx, adults, &U.Age)
			require.NoError(t, err)
			require.Zero(t, largest)
		})

		fixture.Seed(t, db)

		t.Run("Count", func(t *testing.T) {
			n, err := adults.Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, n)
		})

		t.Run("Count honors Limit, Offset, and Distinct", func(t *testing.T) {
			n, err := db.Users.OrderBy(&U.Name).Limit(2).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, n)
			n, err = db.Users.OrderBy(&U.Name).Offset(2).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
			n, err = db.Users.Column(&U.Bio).Distinct().Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, n)
		})

		t.Run("Exists", func(t *testing.T) {
			found, err := adults.Exists(ctx)
			require.NoError(t, err)
			require.True(t, found)
		})

		t.Run("Sum, Avg, Min, Max", func(t *testing.T) {
			sum, err := ant.SumOf(ctx, adults, &U.Age)
			require.NoError(t, err)
			require.Equal(t, 70, sum)

			avg, err := ant.AvgOf(ctx, adults, &U.Age)
			require.NoError(t, err)
			require.EqualValues(t, 35, avg)

			smallest, err := ant.MinOf(ctx, adults, &U.Age)
			require.NoError(t, err)
			require.Equal(t, 30, smallest)

			last, err := ant.MaxOf(ctx, adults, &U.Name)
			require.NoError(t, err)
			require.Equal(t, "Carol", last)
		})

		t.Run("Pluck reads one column", func(t *testing.T) {
			names, err := ant.Pluck(ctx, db.Users.OrderBy(&U.Name), &U.Name)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice", "Bob", "Carol"}, names)
		})

		t.Run("Pluck keeps NULLs with a pointer column", func(t *testing.T) {
			bios, err := ant.Pluck(ctx, db.Users.OrderBy(&U.Name), &U.Bio)
			require.NoError(t, err)
			require.Nil(t, bios[0])
			require.Equal(t, "Gopher", *bios[2])
		})

		t.Run("errors reach the terminal call", func(t *testing.T) {
			x := 1
			//antvet:ignore not a probe, on purpose: the test checks the error
			_, err := db.Users.Where(ant.Gt(&x, 0)).Count(ctx)
			require.ErrorIs(t, err, ant.ErrNotProbe)
			//antvet:ignore another model's field, on purpose: the test checks the error
			_, err = ant.SumOf(ctx, db.Users.Where(), &P.ID)
			require.ErrorIs(t, err, ant.ErrWrongModel)
		})
	})
}
