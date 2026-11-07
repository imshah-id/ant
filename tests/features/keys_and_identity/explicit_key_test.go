package keys_and_identity

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Key replaces the convention. Here ID is an ordinary column.
type Country struct {
	ID   int64
	Code string
}

func (Country) Configure(b *ant.ModelBuilder[Country]) {
	b.Key(&Co.Code)
}

var Co = ant.Fields[Country]()

type GeoDB struct {
	ant.Context
	Countries ant.Table[Country]
}

func TestExplicitKey(t *testing.T) {
	ddl := `CREATE TABLE countries (code TEXT PRIMARY KEY, id BIGINT NOT NULL)`
	fixture.RunWith(t, ddl, func(t *testing.T, db *GeoDB) {
		ctx := t.Context()
		latvia := &Country{ID: 0, Code: "LV"}
		require.NoError(t, db.Countries.Insert(ctx, latvia))

		t.Run("Find uses the configured key", func(t *testing.T) {
			found, err := db.Countries.Find(ctx, "LV")
			require.NoError(t, err)
			require.Equal(t, "LV", found.Code)
		})

		t.Run("the ID field is now a plain column", func(t *testing.T) {
			require.Zero(t, latvia.ID)
		})

		t.Run("the key type follows the configured field", func(t *testing.T) {
			//antvet:ignore a key of the wrong type, on purpose: the test checks the error
			_, err := db.Countries.Find(ctx, 1)
			require.ErrorIs(t, err, ant.ErrKeyType)
		})
	})
}
