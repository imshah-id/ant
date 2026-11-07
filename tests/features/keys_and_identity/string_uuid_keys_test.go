package keys_and_identity

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Non-integer keys are never generated. The application supplies them.
type Slug string

type Page struct {
	ID    Slug
	Title string
}

type Account struct {
	ID    string // a UUID column
	Owner string
}

type SiteDB struct {
	ant.Context
	Pages    ant.Table[Page]
	Accounts ant.Table[Account]
}

func TestStringAndUUIDKeys(t *testing.T) {
	ddl := `CREATE TABLE pages (id TEXT PRIMARY KEY, title TEXT NOT NULL);
	        CREATE TABLE accounts (id UUID PRIMARY KEY, owner TEXT NOT NULL)`
	fixture.RunWith(t, ddl, func(t *testing.T, db *SiteDB) {
		ctx := t.Context()

		const id = "0192f3a4-7c3d-7b1e-8f00-1234567890ab"

		t.Run("string key", func(t *testing.T) {
			require.NoError(t, db.Pages.Insert(ctx, &Page{ID: "home", Title: "Home"}))
			found, err := db.Pages.Find(ctx, Slug("home"))
			require.NoError(t, err)
			require.Equal(t, "Home", found.Title)
		})

		t.Run("a plain string finds a named key", func(t *testing.T) {
			_, err := db.Pages.Find(ctx, "home")
			require.NoError(t, err)
		})

		t.Run("an empty key is written, not generated", func(t *testing.T) {
			require.NoError(t, db.Pages.Insert(ctx, &Page{ID: "", Title: "Empty"}))
			found, err := db.Pages.Find(ctx, Slug(""))
			require.NoError(t, err)
			require.Equal(t, "Empty", found.Title)
		})

		t.Run("UUID key", func(t *testing.T) {
			require.NoError(t, db.Accounts.Insert(ctx, &Account{ID: id, Owner: "Alice"}))
			found, err := db.Accounts.Find(ctx, id)
			require.NoError(t, err)
			require.Equal(t, id, found.ID)
		})

		t.Run("an invalid UUID is the database's error", func(t *testing.T) {
			require.Error(t, db.Accounts.Insert(ctx, &Account{ID: "not-a-uuid"}))
		})

		t.Run("a key of the wrong type", func(t *testing.T) {
			//antvet:ignore a key of the wrong type, on purpose: the test checks the error
			_, err := db.Pages.Find(ctx, 42)
			require.ErrorIs(t, err, ant.ErrKeyType)
		})
	})
}
