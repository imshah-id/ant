package querying

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Find loads a row by primary key.
func TestFind(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		alice, _, _ := fixture.Seed(t, db)
		ctx := t.Context()

		t.Run("found", func(t *testing.T) {
			user, err := db.Users.Find(ctx, alice.ID)
			require.NoError(t, err)
			require.Equal(t, "Alice", user.Name)
		})

		t.Run("not found", func(t *testing.T) {
			user, err := db.Users.Find(ctx, int64(-1))
			require.ErrorIs(t, err, ant.ErrNotFound)
			require.Nil(t, user)
		})

		t.Run("an untyped constant works with an int64 key", func(t *testing.T) {
			_, err := db.Users.Find(ctx, 1_000_000)
			require.ErrorIs(t, err, ant.ErrNotFound)
		})

		// The compiler cannot tie the key type to the model, so ant checks it.
		t.Run("a key of the wrong type", func(t *testing.T) {
			//antvet:ignore a key of the wrong type, on purpose: the test checks the error
			_, err := db.Users.Find(ctx, "1")
			require.ErrorIs(t, err, ant.ErrKeyType)
		})
	})
}
