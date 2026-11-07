package writing

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Upsert inserts a row, or updates the one a unique constraint already holds.
func TestUpsert(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		ctx := t.Context()
		onEmail := ant.OnConflict(&U.Email)

		stored := func(email string) *fixture.User {
			u, err := db.Users.Where(ant.Eq(&U.Email, email)).Single(ctx)
			require.NoError(t, err)

			return u
		}

		t.Run("inserts when the row is new", func(t *testing.T) {
			u := &fixture.User{Name: "Ann", Email: "ann@x", Age: 30}
			wrote, err := db.Users.Upsert(ctx, u, onEmail)
			require.NoError(t, err)
			require.True(t, wrote)
			require.NotZero(t, u.ID)
		})

		t.Run("updates when it exists, and returns the stored key", func(t *testing.T) {
			id := stored("ann@x").ID
			u := &fixture.User{Name: "Anna", Email: "ann@x", Age: 31}
			_, err := db.Users.Upsert(ctx, u, onEmail)
			require.NoError(t, err)
			require.Equal(t, id, u.ID)
			require.Equal(t, "Anna", stored("ann@x").Name)
		})

		t.Run("DoUpdate writes only the named columns", func(t *testing.T) {
			u := &fixture.User{Name: "Changed", Email: "ann@x", Age: 99}
			_, err := db.Users.Upsert(ctx, u, onEmail.DoUpdate(ant.F(&U.Age)))
			require.NoError(t, err)

			got := stored("ann@x")
			require.Equal(t, "Anna", got.Name)
			require.Equal(t, 99, got.Age)
			require.Equal(t, "Anna", u.Name, "the entity receives the stored row")
		})

		t.Run("Set can use the existing value", func(t *testing.T) {
			u := &fixture.User{Name: "x", Email: "ann@x"}
			_, err := db.Users.Upsert(ctx, u, onEmail.Set(ant.Inc(&U.Age, 1)))
			require.NoError(t, err)
			require.Equal(t, 100, stored("ann@x").Age)
		})

		t.Run("DoNothing keeps the row and reports false", func(t *testing.T) {
			u := &fixture.User{Name: "Ignored", Email: "ann@x"}
			wrote, err := db.Users.Upsert(ctx, u, onEmail.DoNothing())
			require.NoError(t, err)
			require.False(t, wrote)
			require.Zero(t, u.ID)
			require.True(t, u.CreatedAt.IsZero(), "the entity is left as it was")
			require.Equal(t, "Anna", stored("ann@x").Name)
		})

		t.Run("a zero value overwrites", func(t *testing.T) {
			u := &fixture.User{Name: "", Email: "ann@x", Age: 0}
			_, err := db.Users.Upsert(ctx, u, onEmail)
			require.NoError(t, err)
			require.Zero(t, stored("ann@x").Age)
		})

		t.Run("no matching constraint is the database's error", func(t *testing.T) {
			_, err := db.Users.Upsert(ctx, &fixture.User{Email: "b@x"}, ant.OnConflict(&U.Age))
			require.Error(t, err)
		})

		t.Run("mistakes", func(t *testing.T) {
			x := 1
			//antvet:ignore not a probe, on purpose: the test checks the error
			_, err := db.Users.Upsert(ctx, &fixture.User{}, ant.OnConflict(&x))
			require.ErrorIs(t, err, ant.ErrNotProbe)
			_, err = db.Users.Upsert(ctx, nil, onEmail)
			require.ErrorIs(t, err, ant.ErrNilEntity)
			_, err = db.Users.Upsert(ctx, &fixture.User{}, nil)
			require.Error(t, err)
		})
	})
}
