package model_definition

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// fixture.U and fixture.P are field probes: var U = ant.Fields[User](). A probe
// holds no data. Taking the address of its field names a column.
func TestFieldProbes(t *testing.T) {
	U, P := fixture.U, fixture.P

	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		fixture.Seed(t, db)
		ctx := t.Context()

		t.Run("a field pointer names a column", func(t *testing.T) {
			adults, err := db.Users.Where(ant.Gt(&U.Age, 18)).OrderBy(&U.Name).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Alice", "Carol"}, fixture.Names(adults))
		})

		t.Run("fields promoted from ant.Base work", func(t *testing.T) {
			_, err := db.Users.OrderBy(&U.CreatedAt).All(ctx)
			require.NoError(t, err)
		})

		t.Run("there is one probe per model, however often you ask", func(t *testing.T) {
			require.Same(t, U, ant.Fields[fixture.User]())
		})

		// These compile, so ant reports them at the terminal call.
		t.Run("a pointer that is not a probe is an error", func(t *testing.T) {
			age := 18
			//antvet:ignore not a probe, on purpose: the test checks the error
			_, err := db.Users.Where(ant.Gt(&age, 18)).All(ctx)
			require.ErrorIs(t, err, ant.ErrNotProbe)
		})

		t.Run("a field of a normal value is not a probe", func(t *testing.T) {
			var user fixture.User

			//antvet:ignore not a probe, on purpose: the test checks the error
			_, err := db.Users.Where(ant.Gt(&user.Age, 18)).All(ctx)
			require.ErrorIs(t, err, ant.ErrNotProbe)
		})

		t.Run("a probe of another model is an error", func(t *testing.T) {
			//antvet:ignore another model's probe, on purpose: the test checks the error
			_, err := db.Posts.Where(ant.Gt(&U.Age, 18)).All(ctx)
			require.ErrorIs(t, err, ant.ErrWrongModel)
		})

		t.Run("a relation is not a column", func(t *testing.T) {
			_, err := db.Posts.Where(ant.IsNull(&P.User)).All(ctx)
			require.ErrorIs(t, err, ant.ErrNotColumn)
		})

		t.Run("errors from several conditions are all reported", func(t *testing.T) {
			age := 18
			//antvet:ignore not a probe, on purpose: the test checks the error
			_, err := db.Posts.Where(ant.Gt(&age, 18), ant.Gt(&U.Age, 18)).All(ctx)
			require.True(t, errors.Is(err, ant.ErrNotProbe) && errors.Is(err, ant.ErrWrongModel))
		})
	})
}
