package writing

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Update changes every matching row and reports how many it changed.
func TestUpdate(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		alice, bob, carol := fixture.Seed(t, db)
		ctx := t.Context()

		t.Run("returns the rows changed", func(t *testing.T) {
			n, err := db.Users.Where(ant.Gt(&U.Age, 18)).Update(ctx, ant.Set(&U.Bio, new("Adult")))
			require.NoError(t, err)
			require.EqualValues(t, 2, n)
		})

		t.Run("no match changes nothing", func(t *testing.T) {
			n, err := db.Users.Where(ant.Eq(&U.Name, "Nobody")).Update(ctx, ant.Set(&U.Age, 1))
			require.NoError(t, err)
			require.Zero(t, n)
		})

		t.Run("several assignments", func(t *testing.T) {
			_, err := db.Users.Where(ant.Eq(&U.ID, bob.ID)).Update(ctx,
				ant.Set(&U.Name, "Robert"),
				ant.Set(&U.Age, 18))
			require.NoError(t, err)
			require.NoError(t, db.Users.Reload(ctx, bob))
			require.Equal(t, "Robert", bob.Name)
			require.Equal(t, 18, bob.Age)
		})

		t.Run("zero values are written", func(t *testing.T) {
			_, err := db.Users.Where(ant.Eq(&U.ID, alice.ID)).
				Update(ctx, ant.Set(&U.Age, 0), ant.Set(&U.Email, ""))
			require.NoError(t, err)
			require.NoError(t, db.Users.Reload(ctx, alice))
			require.Zero(t, alice.Age)
			require.Empty(t, alice.Email)
		})

		t.Run("nil writes NULL", func(t *testing.T) {
			_, err := db.Users.Where(ant.Eq(&U.ID, carol.ID)).Update(ctx, ant.Set(&U.Bio, nil))
			require.NoError(t, err)
			require.NoError(t, db.Users.Reload(ctx, carol))
			require.Nil(t, carol.Bio)
		})

		t.Run("nothing to set", func(t *testing.T) {
			_, err := db.Users.Where(ant.Eq(&U.ID, alice.ID)).Update(ctx)
			require.ErrorIs(t, err, ant.ErrNoAssignment)
		})

		t.Run("a field of another model", func(t *testing.T) {
			//antvet:ignore another model's field, on purpose: the test checks the error
			_, err := db.Users.Where(ant.Eq(&U.ID, alice.ID)).Update(ctx, ant.Set(&P.Title, "x"))
			require.ErrorIs(t, err, ant.ErrWrongModel)
		})

		t.Run("a pointer that is not a probe", func(t *testing.T) {
			age := 1
			//antvet:ignore not a probe, on purpose: the test checks the error
			_, err := db.Users.Where(ant.Eq(&U.ID, alice.ID)).Update(ctx, ant.Set(&age, 2))
			require.ErrorIs(t, err, ant.ErrNotProbe)
		})

		t.Run("a relation cannot be set", func(t *testing.T) {
			_, err := db.Users.Where(ant.Eq(&U.ID, alice.ID)).Update(ctx, ant.Set(&U.Posts, nil))
			require.ErrorIs(t, err, ant.ErrNotColumn)
		})

		t.Run("a constraint failure is the database's error", func(t *testing.T) {
			_, err := db.Users.Where(ant.Eq(&U.ID, alice.ID)).
				Update(ctx, ant.Set(&U.Email, "Carol@example.com"))
			require.ErrorContains(t, err, "duplicate key")
		})
	})
}
