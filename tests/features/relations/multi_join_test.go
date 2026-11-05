package relations

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// Joins chain up to four models. Each step can follow a relation of any model
// already in the query.
type Comment struct {
	ID      int64
	EntryID int64
	Text    string
	Entry   *Entry
	Likes   []Like
}

type Entry struct {
	ID       int64
	WriterID int64
	Title    string
	Writer   *Writer
}

type Writer struct {
	ID   int64
	Name string
}

type Like struct {
	ID        int64
	CommentID int64
	Who       string
}

type ForumDB struct {
	ant.Context
	Comments ant.Table[Comment]
	Entries  ant.Table[Entry]
	Writers  ant.Table[Writer]
	Likes    ant.Table[Like]
}

var (
	Co = ant.Fields[Comment]()
	En = ant.Fields[Entry]()
	Wr = ant.Fields[Writer]()
)

func TestMultiJoin(t *testing.T) {
	ddl := `
		CREATE TABLE writers (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL);
		CREATE TABLE entries (id BIGSERIAL PRIMARY KEY, writer_id BIGINT NOT NULL REFERENCES writers, title TEXT NOT NULL);
		CREATE TABLE comments (id BIGSERIAL PRIMARY KEY, entry_id BIGINT NOT NULL REFERENCES entries, text TEXT NOT NULL);
		CREATE TABLE likes (id BIGSERIAL PRIMARY KEY, comment_id BIGINT NOT NULL REFERENCES comments, who TEXT NOT NULL)`
	fixture.RunWith(t, ddl, func(t *testing.T, db *ForumDB) {
		ctx := t.Context()
		writer := &Writer{Name: "Ann"}
		require.NoError(t, db.Writers.Insert(ctx, writer))
		entry := &Entry{WriterID: writer.ID, Title: "Go"}
		require.NoError(t, db.Entries.Insert(ctx, entry))

		liked, quiet := &Comment{
			EntryID: entry.ID,
			Text:    "Nice",
		}, &Comment{
			EntryID: entry.ID,
			Text:    "Hm",
		}
		require.NoError(t, db.Comments.Insert(ctx, liked, quiet))
		require.NoError(t, db.Likes.Insert(ctx, &Like{CommentID: liked.ID, Who: "Bob"}))

		t.Run("three models", func(t *testing.T) {
			got, err := db.Comments.Join(&Co.Entry).Join(&En.Writer).
				Where(ant.Eq(&Wr.Name, "Ann"), ant.Eq(&Co.Text, "Nice")).
				Select(func(c *Comment, e *Entry, w *Writer) string {
					return w.Name + "/" + e.Title + "/" + c.Text
				}).
				All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Ann/Go/Nice"}, got)
		})

		t.Run("four models, with a left join", func(t *testing.T) {
			got, err := db.Comments.Join(&Co.Entry).Join(&En.Writer).LeftJoinMany(&Co.Likes).
				OrderBy(&Co.Text).
				Select(func(c *Comment, e *Entry, w *Writer, l *Like) string {
					if l == nil {
						return c.Text + ": none"
					}

					return c.Text + ": " + l.Who
				}).
				All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"Hm: none", "Nice: Bob"}, got)
		})

		t.Run("a relation of a model outside the query", func(t *testing.T) {
			//antvet:ignore another model's field, on purpose: the test checks the error
			_, err := db.Comments.Join(&En.Writer).
				Select(func(c *Comment, w *Writer) string { return "" }).All(ctx)
			require.ErrorIs(t, err, ant.ErrWrongModel)
		})
	})
}
