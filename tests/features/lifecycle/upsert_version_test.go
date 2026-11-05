package lifecycle

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Article is a versioned model with a unique slug, for upserts.
type Article struct {
	ID      int64
	Slug    string `ant:"unique"`
	Title   string
	Views   int
	Version int64 `ant:"version"`
}

// Proposal is a staged Article, as a source for Merge.
type Proposal struct {
	ID    int64
	Slug  string
	Title string
}

type ArticlesDB struct {
	ant.Context
	Articles  ant.Table[Article]
	Proposals ant.Table[Proposal]
	Tallies   ant.Table[Tally]
}

// Tally has three columns to insert, a third of PostgreSQL's 65535 parameters.
type Tally struct {
	ID      int64
	Name    string `ant:"unique"`
	Note    string
	Version int64 `ant:"version"`
}

var (
	Ar = ant.Fields[Article]()
	Pr = ant.Fields[Proposal]()
	Ta = ant.Fields[Tally]()
)

const articlesDDL = `
	CREATE TABLE articles (
		id BIGSERIAL PRIMARY KEY,
		slug TEXT NOT NULL UNIQUE,
		title TEXT NOT NULL,
		views INT NOT NULL DEFAULT 0,
		version BIGINT NOT NULL
	);
	CREATE TABLE proposals (id BIGSERIAL PRIMARY KEY, slug TEXT NOT NULL, title TEXT NOT NULL);
	CREATE TABLE tallies (
		id BIGSERIAL PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		note TEXT NOT NULL,
		version BIGINT NOT NULL
	)`

// Every write moves the version on, so that a copy loaded before it fails to
// save. An upsert that updates is such a write: it moves the version on rather
// than starting it again at 1, which would let a copy from long ago match. A copy
// that carries a version only updates the row it loaded.
func TestUpsertKeepsVersions(t *testing.T) {
	fixture.RunTrackedWith(t, articlesDDL, func(t *testing.T, db *ArticlesDB) {
		ctx := t.Context()
		onSlug := ant.OnConflict(&Ar.Slug)

		// seed stores a doc and moves it to version 3, as two saves would.
		seed := func(t *testing.T, slug string) *Article {
			t.Helper()

			doc := &Article{Slug: slug, Title: "v1"}
			require.NoError(t, db.Articles.Insert(ctx, doc))

			for _, title := range []string{"v2", "v3"} {
				doc.Title = title

				require.NoError(t, db.SaveChanges(ctx))
			}

			require.EqualValues(t, 3, doc.Version)

			return doc
		}

		stored := func(t *testing.T, slug string) *Article {
			t.Helper()

			doc, err := db.Articles.NoTracking().Where(ant.Eq(&Ar.Slug, slug)).First(ctx)
			require.NoError(t, err)

			return doc
		}

		t.Run("a new row starts at 1", func(t *testing.T) {
			doc := &Article{Slug: "new", Title: "t"}
			wrote, err := db.Articles.Upsert(ctx, doc, onSlug)
			require.NoError(t, err)
			require.True(t, wrote)
			require.EqualValues(t, 1, doc.Version)
			require.EqualValues(t, 1, stored(t, "new").Version)
		})

		conflicts := []struct {
			name     string
			conflict *ant.Conflict
			title    string
		}{
			{"every column", onSlug, "upserted"},
			{"DoUpdate", onSlug.DoUpdate(ant.F(&Ar.Title)), "upserted"},
			{"Set", onSlug.Set(ant.Set(&Ar.Title, "set")), "set"},
			{
				"DoUpdate and Set",
				onSlug.DoUpdate(ant.F(&Ar.Title)).Set(ant.Inc(&Ar.Views, 1)),
				"upserted",
			},
		}

		for _, tt := range conflicts {
			t.Run("an update moves it on: "+tt.name, func(t *testing.T) {
				slug := "update-" + tt.name
				seed(t, slug)

				doc := &Article{Slug: slug, Title: "upserted"}
				wrote, err := db.Articles.Upsert(ctx, doc, tt.conflict)
				require.NoError(t, err)
				require.True(t, wrote)
				require.EqualValues(t, 4, doc.Version, "the entity gets the stored version")

				got := stored(t, slug)
				require.EqualValues(t, 4, got.Version)
				require.Equal(t, tt.title, got.Title)
			})
		}

		t.Run("DoNothing leaves it alone", func(t *testing.T) {
			seed(t, "nothing")

			doc := &Article{Slug: "nothing", Title: "ignored"}
			wrote, err := db.Articles.Upsert(ctx, doc, onSlug.DoNothing())
			require.NoError(t, err)
			require.False(t, wrote)
			require.Zero(t, doc.Version, "what Upsert filled in is taken back")
			require.EqualValues(t, 3, stored(t, "nothing").Version)
		})

		// The copy was loaded at version 1. Starting the version again at 1
		// would make it match, and it would save over two newer writes.
		t.Run("a copy from version 1 cannot save after an upsert", func(t *testing.T) {
			first := &Article{Slug: "stale", Title: "v1"}
			require.NoError(t, db.Articles.Insert(ctx, first))

			holder := ant.Session(db, ant.WithTracking(true))
			copyOf, err := holder.Articles.Find(ctx, first.ID)
			require.NoError(t, err)
			require.EqualValues(t, 1, copyOf.Version)

			for _, title := range []string{"v2", "v3"} {
				first.Title = title

				require.NoError(t, db.SaveChanges(ctx))
			}

			_, err = db.Articles.Upsert(ctx, &Article{Slug: "stale", Title: "upserted"}, onSlug)
			require.NoError(t, err)

			copyOf.Title = "from the old copy"

			require.ErrorIs(t, holder.SaveChanges(ctx), ant.ErrConcurrency)
			require.Equal(t, "upserted", stored(t, "stale").Title)
		})

		t.Run("a copy with a version updates only that version", func(t *testing.T) {
			doc := seed(t, "copy")

			mine := &Article{Slug: "copy", Title: "mine", Version: doc.Version}
			wrote, err := db.Articles.Upsert(ctx, mine, onSlug)
			require.NoError(t, err)
			require.True(t, wrote)
			require.EqualValues(t, 4, mine.Version)

			stale := &Article{Slug: "copy", Title: "stale", Version: 3}
			_, err = db.Articles.Upsert(ctx, stale, onSlug)
			require.ErrorIs(t, err, ant.ErrConcurrency)

			var conflict *ant.ConcurrencyError
			require.True(t, errors.As(err, &conflict))
			require.Equal(t, "Article", conflict.Model)

			require.EqualValues(t, 3, stale.Version, "the failed upsert leaves the copy alone")
			require.Zero(t, stale.ID)

			got := stored(t, "copy")
			require.Equal(t, "mine", got.Title)
			require.EqualValues(t, 4, got.Version)
		})

		// Without RETURNING, the row count tells whether the version let the
		// update through.
		t.Run("a stale copy is refused without RETURNING too", func(t *testing.T) {
			seed(t, "counted")

			plain, err := ant.Wrap[ArticlesDB](noReturning{db.Dialect()}, db.DB())
			require.NoError(t, err)

			stale := &Article{Slug: "counted", Title: "stale", Version: 2}
			_, err = plain.Articles.Upsert(ctx, stale, onSlug)
			require.ErrorIs(t, err, ant.ErrConcurrency)
			require.Equal(t, "stale", stale.Title, "the copy was not overwritten")

			current := &Article{Slug: "counted", Title: "current", Version: 3}
			wrote, err := plain.Articles.Upsert(ctx, current, onSlug)
			require.NoError(t, err)
			require.True(t, wrote)
			require.EqualValues(t, 4, current.Version)
		})

		t.Run("a copy from a later version is refused too", func(t *testing.T) {
			seed(t, "ahead")

			_, err := db.Articles.Upsert(
				ctx,
				&Article{Slug: "ahead", Title: "x", Version: 9},
				onSlug,
			)
			require.ErrorIs(t, err, ant.ErrConcurrency)
			require.EqualValues(t, 3, stored(t, "ahead").Version)
		})

		t.Run("ant keeps the version, so naming it is an error", func(t *testing.T) {
			seed(t, "named")

			for _, conflict := range []*ant.Conflict{
				onSlug.DoUpdate(ant.F(&Ar.Version)),
				onSlug.Set(ant.Set(&Ar.Version, int64(1))),
				onSlug.Set(ant.Inc(&Ar.Version, 1)),
			} {
				_, err := db.Articles.Upsert(ctx, &Article{Slug: "named", Title: "x"}, conflict)
				require.ErrorContains(t, err, "Version")
			}

			got := stored(t, "named")
			require.Equal(t, "v3", got.Title, "nothing ran")
			require.EqualValues(t, 3, got.Version)
		})

		t.Run("InsertMany with a conflict moves each version on", func(t *testing.T) {
			seed(t, "many-1")
			seed(t, "many-2")

			n, err := db.Articles.InsertMany(
				ctx,
				[]*Article{
					{Slug: "many-1", Title: "a"},
					{Slug: "many-2", Title: "b"},
					{Slug: "many-3", Title: "c"},
				},
				ant.WithConflict(onSlug),
			)
			require.NoError(t, err)
			require.EqualValues(t, 3, n)

			require.EqualValues(t, 4, stored(t, "many-1").Version)
			require.EqualValues(t, 4, stored(t, "many-2").Version)
			require.EqualValues(t, 1, stored(t, "many-3").Version)

			_, err = db.Articles.InsertMany(
				ctx,
				[]*Article{{Slug: "many-1", Title: "z"}},
				ant.WithConflict(onSlug.Set(ant.Set(&Ar.Version, int64(1)))),
			)
			require.ErrorContains(t, err, "Version")
		})

		// A batch fills every parameter the database takes, so moving the
		// version on must not take one more.
		t.Run("a full batch has no parameter to spare", func(t *testing.T) {
			rows := make([]*Tally, 65535/3)

			for i := range rows {
				rows[i] = &Tally{Name: fmt.Sprint(i), Note: "n"}
			}

			conflict := ant.WithConflict(ant.OnConflict(&Ta.Name))
			_, err := db.Tallies.InsertMany(ctx, rows, conflict, ant.BatchSize(len(rows)))
			require.NoError(t, err)

			again := make([]*Tally, len(rows))

			for i := range again {
				again[i] = &Tally{Name: fmt.Sprint(i), Note: "again"}
			}

			_, err = db.Tallies.InsertMany(ctx, again, conflict, ant.BatchSize(len(rows)))
			require.NoError(t, err)

			n, err := db.Tallies.Where(ant.Eq(&Ta.Version, int64(2))).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, len(rows), n)
		})

		t.Run("Merge counts as a write", func(t *testing.T) {
			version, err := ant.Raw[int](
				db,
				"SELECT current_setting('server_version_num')::int",
			).First(ctx)
			require.NoError(t, err)

			if *version < 150000 {
				t.Skip("MERGE needs PostgreSQL 15")
			}

			seed(t, "merged")

			holder := ant.Session(db, ant.WithTracking(true))
			copyOf, err := holder.Articles.Where(ant.Eq(&Ar.Slug, "merged")).First(ctx)
			require.NoError(t, err)

			require.NoError(t, db.Proposals.Insert(ctx,
				&Proposal{Slug: "merged", Title: "from a draft"},
				&Proposal{Slug: "fresh", Title: "new from a draft"}))

			on := ant.EqCol(&Ar.Slug, &Pr.Slug)
			n, err := ant.Merge(&db.Articles, db.Proposals.Query(), on).
				WhenMatched(ant.SetExpr(&Ar.Title, ant.Col(&Pr.Title))).
				WhenNotMatched(ant.Into(&Ar.Slug, &Pr.Slug), ant.Into(&Ar.Title, &Pr.Title)).
				Exec(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, n)

			require.EqualValues(t, 4, stored(t, "merged").Version, "a matched row moves on")
			require.EqualValues(t, 1, stored(t, "fresh").Version, "an inserted row starts at 1")

			copyOf.Title = "from the old copy"

			require.ErrorIs(t, holder.SaveChanges(ctx), ant.ErrConcurrency)

			_, err = ant.Merge(&db.Articles, db.Proposals.Query(), on).
				WhenMatched(ant.Set(&Ar.Version, int64(1))).
				Exec(ctx)
			require.ErrorContains(t, err, "Version")
		})
	})
}

// noReturning is a dialect that takes ON CONFLICT with a condition but has no
// RETURNING, as a custom dialect might.
type noReturning struct{ dialect.Dialect }

func (noReturning) SupportsReturning() bool { return false }
