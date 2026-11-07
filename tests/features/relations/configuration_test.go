package relations

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Configure names the key when conventions cannot find it, or when a relation
// points at something other than the primary key.
type Author struct {
	ID     int64
	Handle string
	Books  []Volume // Volume has no AuthorID, so this needs configuring
	Quotes []Quote  // joined on Handle, not ID
}

func (Author) Configure(b *ant.ModelBuilder[Author]) {
	b.HasMany(&Au.Books).ForeignKey(&Vo.WriterID).OnDelete(ant.Cascade)
	b.HasMany(&Au.Quotes).ForeignKey(&Qu.AuthorHandle).References(&Au.Handle)
}

type Volume struct {
	ID       int64
	WriterID int64
	Title    string
	Writer   *Author
}

func (Volume) Configure(b *ant.ModelBuilder[Volume]) {
	b.BelongsTo(&Vo.Writer).ForeignKey(&Vo.WriterID)
}

type Quote struct {
	ID           int64
	AuthorHandle string
	Text         string
}

// Nothing connects Orphan to Stray, by name or by configuration.
type Orphan struct {
	ID     int64
	Strays []Stray
}

type Stray struct{ ID int64 }

type ShelfDB struct {
	ant.Context
	Authors ant.Table[Author]
	Volumes ant.Table[Volume]
	Quotes  ant.Table[Quote]
	Orphans ant.Table[Orphan]
}

var (
	Au = ant.Fields[Author]()
	Vo = ant.Fields[Volume]()
	Qu = ant.Fields[Quote]()
	Or = ant.Fields[Orphan]()
)

func TestConfiguredRelations(t *testing.T) {
	ddl := `
		CREATE TABLE authors (id BIGSERIAL PRIMARY KEY, handle TEXT UNIQUE NOT NULL);
		CREATE TABLE volumes (id BIGSERIAL PRIMARY KEY, writer_id BIGINT NOT NULL REFERENCES authors, title TEXT NOT NULL);
		CREATE TABLE quotes (id BIGSERIAL PRIMARY KEY, author_handle TEXT NOT NULL REFERENCES authors (handle), text TEXT NOT NULL);
		CREATE TABLE orphans (id BIGSERIAL PRIMARY KEY)`
	fixture.RunWith(t, ddl, func(t *testing.T, db *ShelfDB) {
		ctx := t.Context()
		ada := &Author{Handle: "ada"}
		require.NoError(t, db.Authors.Insert(ctx, ada))
		require.NoError(t, db.Volumes.Insert(ctx, &Volume{WriterID: ada.ID, Title: "Notes"}))

		require.NoError(
			t,
			db.Quotes.Insert(ctx, &Quote{AuthorHandle: "ada", Text: "That brain of mine"}),
		)

		t.Run("ForeignKey on the child", func(t *testing.T) {
			found, err := db.Authors.Include(&Au.Books).First(ctx)
			require.NoError(t, err)
			require.Equal(t, "Notes", found.Books[0].Title)
		})

		t.Run("ForeignKey on the owner", func(t *testing.T) {
			found, err := db.Volumes.IncludeOne(&Vo.Writer).First(ctx)
			require.NoError(t, err)
			require.Equal(t, "ada", found.Writer.Handle)
		})

		t.Run("References a column other than the key", func(t *testing.T) {
			found, err := db.Authors.Include(&Au.Quotes).First(ctx)
			require.NoError(t, err)
			require.Len(t, found.Quotes, 1)
		})

		t.Run("no foreign key is an error at the terminal call", func(t *testing.T) {
			_, err := db.Orphans.Include(&Or.Strays).All(ctx)
			require.ErrorIs(t, err, ant.ErrNoForeignKey)
			_, err = db.Orphans.Where(ant.Has(&Or.Strays)).All(ctx)
			require.ErrorIs(t, err, ant.ErrNoForeignKey)
		})

		t.Run("a column used as a relation", func(t *testing.T) {
			var notRelated []string

			//antvet:ignore not a probe, on purpose: the test checks the error
			_, err := db.Orphans.Include(&notRelated).All(ctx)
			require.ErrorIs(t, err, ant.ErrNotProbe)
		})
	})
}
