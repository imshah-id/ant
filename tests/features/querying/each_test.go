package querying

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Each streams rows one at a time, for results too large to hold in memory.
func TestEach(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		fixture.Seed(t, db)
		ctx := t.Context()

		t.Run("yields each row", func(t *testing.T) {
			var seen []string

			for user, err := range db.Users.OrderBy(&U.Name).Each(ctx) {
				require.NoError(t, err)

				seen = append(seen, user.Name)
			}

			require.Equal(t, []string{"Alice", "Bob", "Carol"}, seen)
		})

		t.Run("no rows means no iterations", func(t *testing.T) {
			for range db.Users.Where(ant.Eq(&U.Name, "Nobody")).Each(ctx) {
				t.Fatal("unexpected row")
			}
		})

		t.Run("break releases the connection", func(t *testing.T) {
			for range 20 {
				for _, err := range db.Users.Each(ctx) {
					require.NoError(t, err)

					break
				}
			}

			_, err := db.Users.All(ctx)
			require.NoError(t, err)
		})

		t.Run("a builder error arrives as the only item", func(t *testing.T) {
			age := 1
			count := 0

			//antvet:ignore not a probe, on purpose: the test checks the error
			for _, err := range db.Users.Where(ant.Gt(&age, 0)).Each(ctx) {
				require.ErrorIs(t, err, ant.ErrNotProbe)

				count++
			}

			require.Equal(t, 1, count)
		})

		t.Run("a cancelled context is an error", func(t *testing.T) {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()

			for _, err := range db.Users.Each(cancelled) {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	})
}
