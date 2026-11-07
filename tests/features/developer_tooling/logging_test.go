package developer_tooling

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

func TestLogging(t *testing.T) {
	fixture.Each(t, func(t *testing.T, target fixture.Target) {
		ctx := t.Context()

		var out bytes.Buffer

		logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))

		t.Run("statements log at debug, without values", func(t *testing.T) {
			out.Reset()

			db := fixture.Open[fixture.AppDB](t, target, ant.WithLogger(logger))
			_, err := db.Users.Where(ant.Eq(&U.Name, "secret-value")).All(ctx)
			require.NoError(t, err)
			require.Contains(t, out.String(), "level=DEBUG")
			require.Contains(t, out.String(), "SELECT")

			require.NotContains(
				t,
				out.String(),
				"secret-value",
				"values stay out of logs by default",
			)
		})

		t.Run("LogValues opts in", func(t *testing.T) {
			out.Reset()

			db := fixture.Open[fixture.AppDB](t, target, ant.WithLogger(logger, ant.LogValues()))
			_, err := db.Users.Where(ant.Eq(&U.Name, "visible-value")).All(ctx)
			require.NoError(t, err)
			require.Contains(t, out.String(), "visible-value")
		})

		t.Run("failures log at error", func(t *testing.T) {
			out.Reset()

			db := fixture.Open[fixture.AppDB](t, target, ant.WithLogger(logger))
			_, err := ant.Raw[any](db, `SELEC`).Exec(ctx)
			require.Error(t, err)
			require.Contains(t, out.String(), "level=ERROR")
		})

		t.Run("slow statements log at warning", func(t *testing.T) {
			out.Reset()

			db := fixture.Open[fixture.AppDB](
				t,
				target,
				ant.WithLogger(logger, ant.SlowThreshold(time.Nanosecond)),
			)
			_, err := db.Users.All(ctx)
			require.NoError(t, err)
			require.Contains(t, out.String(), "level=WARN")
		})
	})
}

// ToSQL shows a query's statement, and Explain the database's plan for it.
func TestInspection(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		q := db.Users.Where(ant.Gt(&U.Age, 18)).OrderBy(&U.Name).Limit(5)

		text, args, err := q.ToSQL()
		require.NoError(t, err)
		require.Contains(t, text, `WHERE "users"."age" > $1 ORDER BY "users"."name" LIMIT $2`)
		require.Equal(t, []any{18, 5}, args)

		plan, err := q.Explain(t.Context())
		require.NoError(t, err)
		require.Contains(t, plan, "Limit")

		x := 1
		//antvet:ignore not a probe, on purpose: the test checks the error
		_, _, err = db.Users.Where(ant.Gt(&x, 0)).ToSQL()
		require.ErrorIs(t, err, ant.ErrNotProbe)
	})
}
