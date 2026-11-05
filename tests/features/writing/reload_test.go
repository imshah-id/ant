package writing

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Reload replaces an entity's columns with what is stored now.
func TestReload(t *testing.T) {
	fixture.Run(t, func(t *testing.T, db *fixture.AppDB) {
		alice, _, carol := fixture.Seed(t, db)
		ctx := t.Context()

		t.Run("picks up changes", func(t *testing.T) {
			_, err := db.Users.Where(ant.Eq(&U.ID, alice.ID)).Update(ctx, ant.Set(&U.Age, 31))
			require.NoError(t, err)
			require.NoError(t, db.Users.Reload(ctx, alice))
			require.Equal(t, 31, alice.Age)
		})

		t.Run("discards unsaved changes", func(t *testing.T) {
			alice.Name = "Changed in memory"
			require.NoError(t, db.Users.Reload(ctx, alice))
			require.Equal(t, "Alice", alice.Name)
		})

		t.Run("a NULL clears a pointer", func(t *testing.T) {
			_, err := db.Users.Where(ant.Eq(&U.ID, carol.ID)).Update(ctx, ant.Set(&U.Bio, nil))
			require.NoError(t, err)
			require.NoError(t, db.Users.Reload(ctx, carol))
			require.Nil(t, carol.Bio)
		})

		t.Run("relations are left alone", func(t *testing.T) {
			alice.Posts = []fixture.Post{{Title: "In memory"}}
			require.NoError(t, db.Users.Reload(ctx, alice))
			require.Len(t, alice.Posts, 1)
			alice.Posts = nil
		})

		t.Run("the row is gone", func(t *testing.T) {
			require.NoError(t, db.Users.DeleteByID(ctx, alice.ID))
			require.ErrorIs(t, db.Users.Reload(ctx, alice), ant.ErrNotFound)
		})

		t.Run("a nil entity", func(t *testing.T) {
			require.ErrorIs(t, db.Users.Reload(ctx, nil), ant.ErrNilEntity)
		})
	})
}

// Labels is a column whose Scan fills the map it already holds, as a Scanner
// may.
type Labels map[string]string

func (l Labels) Value() (driver.Value, error) {
	parts := make([]string, 0, len(l))

	for _, k := range slices.Sorted(maps.Keys(l)) {
		parts = append(parts, k+"="+l[k])
	}

	return strings.Join(parts, ","), nil
}

func (l *Labels) Scan(src any) error {
	text, ok := src.(string)
	if !ok {
		return fmt.Errorf("labels: unexpected %T", src)
	}

	if *l == nil {
		*l = Labels{}
	}

	clear(*l)

	for part := range strings.SplitSeq(text, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			(*l)[k] = v
		}
	}

	return nil
}

type Box struct {
	ID     int64
	Labels Labels
}

type BoxDB struct {
	ant.Context
	Boxes ant.Table[Box]
}

var B = ant.Fields[Box]()

// A rollback takes a reload back to the values it replaced, even a map that the
// column's Scan fills in place.
func TestReloadRollbackKeepsScannedMaps(t *testing.T) {
	ddl := `CREATE TABLE boxes (id BIGSERIAL PRIMARY KEY, labels TEXT NOT NULL)`
	fixture.RunTrackedWith(t, ddl, func(t *testing.T, db *BoxDB) {
		ctx := t.Context()
		box := &Box{Labels: Labels{"color": "red"}}
		require.NoError(t, db.Boxes.Insert(ctx, box))

		err := db.Tx(ctx, func(tx *BoxDB) error {
			_, err := tx.Boxes.Where(ant.Eq(&B.ID, box.ID)).
				Update(ctx, ant.Set(&B.Labels, Labels{"color": "blue"}))
			require.NoError(t, err)
			require.NoError(t, tx.Boxes.Reload(ctx, box))
			require.Equal(t, Labels{"color": "blue"}, box.Labels)

			return errors.New("roll back")
		})
		require.ErrorContains(t, err, "roll back")
		require.Equal(t, Labels{"color": "red"}, box.Labels)
	})
}
