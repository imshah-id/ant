package querying

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// OrderBy sets the order, and ThenBy breaks ties.
func TestOrderBy(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		ctx := t.Context()

		for _, u := range []fixture.User{
			{Name: "Bea", Email: "1", Age: 30},
			{Name: "Abe", Email: "2", Age: 30},
			{Name: "Cal", Email: "3", Age: 20},
		} {
			require.NoError(t, db.Users.Insert(ctx, &u))
		}

		cases := []struct {
			name  string
			query *ant.Query[fixture.User]
			want  []string
		}{
			{"ascending", db.Users.OrderBy(&U.Name), []string{"Abe", "Bea", "Cal"}},
			{"descending", db.Users.OrderByDesc(&U.Name), []string{"Cal", "Bea", "Abe"}},
			{
				"ThenBy breaks ties",
				db.Users.OrderBy(&U.Age).ThenBy(&U.Name),
				[]string{"Cal", "Abe", "Bea"},
			},
			{
				"ThenByDesc breaks ties",
				db.Users.OrderBy(&U.Age).ThenByDesc(&U.Name),
				[]string{"Cal", "Bea", "Abe"},
			},
			{
				"a later OrderBy replaces earlier ones",
				db.Users.OrderBy(&U.Age).OrderBy(&U.Name),
				[]string{"Abe", "Bea", "Cal"},
			},
		}

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				got, err := c.query.All(ctx)
				require.NoError(t, err)
				require.Equal(t, c.want, fixture.Names(got))
			})
		}

		t.Run("ThenBy needs an OrderBy", func(t *testing.T) {
			_, err := db.Users.Where(ant.Gt(&U.Age, 0)).ThenBy(&U.Name).All(ctx)
			require.ErrorIs(t, err, ant.ErrThenWithoutOrder)
		})

		t.Run("a probe of another model is an error", func(t *testing.T) {
			//antvet:ignore another model's field, on purpose: the test checks the error
			_, err := db.Users.OrderBy(&P.Title).All(ctx)
			require.ErrorIs(t, err, ant.ErrWrongModel)
		})
	})
}
