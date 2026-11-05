package querying

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

type Branch struct {
	ID       int64
	ParentID *int64
	Name     string
	Parent   *Branch
	Children []Branch
}

type TreeDB struct {
	ant.Context
	Branches ant.Table[Branch]
}

var Br = ant.Fields[Branch]()

// FromCTE names a subquery in a WITH clause. Recursive follows a relation of a
// model to itself as far as it goes.
func TestCTE(t *testing.T) {
	ddl := `CREATE TABLE branches (id BIGSERIAL PRIMARY KEY, parent_id BIGINT REFERENCES branches, name TEXT NOT NULL)`
	fixture.RunWith(t, ddl, func(t *testing.T, db *TreeDB) {
		ctx := t.Context()
		add := func(name string, parent *Branch) *Branch {
			b := &Branch{Name: name}

			if parent != nil {
				b.ParentID = &parent.ID
			}

			require.NoError(t, db.Branches.InsertOnly(ctx, b))

			return b
		}
		root := add("root", nil)
		a := add("a", root)
		add("b", root)
		leaf := add("a1", a)
		add("other", nil)

		names := func(items []*Branch) []string {
			out := make([]string, len(items))

			for i, b := range items {
				out[i] = b.Name
			}

			return out
		}

		t.Run("FromCTE", func(t *testing.T) {
			named := db.Branches.Where(ant.Like(&Br.Name, "a%"))
			got, err := db.Branches.Query().FromCTE("named", named).OrderBy(&Br.Name).All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"a", "a1"}, names(got))
		})

		t.Run("Recursive goes down", func(t *testing.T) {
			got, err := db.Branches.Where(ant.Eq(&Br.ID, root.ID)).
				Recursive(&Br.Children).
				OrderBy(&Br.Name).
				All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"a", "a1", "b", "root"}, names(got))
		})

		t.Run("RecursiveOne goes up", func(t *testing.T) {
			got, err := db.Branches.Where(ant.Eq(&Br.ID, leaf.ID)).
				RecursiveOne(&Br.Parent).
				OrderBy(&Br.Name).
				All(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{"a", "a1", "root"}, names(got))
		})

		// The starting rows keep their own ordering and paging: here the last
		// root by name, and the second, which are both root.
		t.Run("a paged start", func(t *testing.T) {
			roots := db.Branches.Where(ant.IsNull(&Br.ParentID))

			for _, start := range []*ant.Query[Branch]{
				roots.OrderByDesc(&Br.Name).Limit(1),
				roots.OrderBy(&Br.Name).Offset(1).Limit(1),
			} {
				got, err := start.Recursive(&Br.Children).OrderBy(&Br.Name).All(ctx)
				require.NoError(t, err)
				require.Equal(t, []string{"a", "a1", "b", "root"}, names(got))
			}

			got, err := db.Branches.Where(ant.Eq(&Br.ID, leaf.ID)).
				OrderBy(&Br.Name).
				RecursiveOne(&Br.Parent).
				OrderBy(&Br.Name).
				All(ctx)
			require.NoError(t, err, "an ordering without paging changes nothing")
			require.Equal(t, []string{"a", "a1", "root"}, names(got))
		})

		t.Run("the result is a normal query", func(t *testing.T) {
			n, err := db.Branches.Where(ant.Eq(&Br.ID, root.ID)).
				Recursive(&Br.Children).
				Where(ant.IsNotNull(&Br.ParentID)).
				Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 3, n)
		})

		// Update and Delete change only the rows the query found: here the tmp
		// under archive, not the one under inbox.
		t.Run("Update and Delete keep to the tree", func(t *testing.T) {
			archive := add("archive", nil)
			year := add("2025", archive)
			add("tmp", year)
			inbox := add("inbox", nil)
			add("tmp", inbox)

			tree := db.Branches.Where(ant.Eq(&Br.ID, archive.ID)).Recursive(&Br.Children)

			n, err := tree.Where(ant.Eq(&Br.Name, "tmp")).Delete(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)

			n, err = tree.Update(ctx, ant.Set(&Br.Name, "archived"))
			require.NoError(t, err)
			require.EqualValues(t, 2, n)

			kept, err := db.Branches.Where(ant.Eq(&Br.Name, "tmp")).All(ctx)
			require.NoError(t, err)
			require.Len(t, kept, 1)
			require.Equal(t, inbox.ID, *kept[0].ParentID)
		})

		// UNION drops rows already seen, so a loop in the data ends.
		t.Run("a cycle in the data terminates", func(t *testing.T) {
			_, err := db.Branches.Where(ant.Eq(&Br.ID, root.ID)).
				Update(ctx, ant.Set(&Br.ParentID, &leaf.ID))
			require.NoError(t, err)
			got, err := db.Branches.Where(ant.Eq(&Br.ID, root.ID)).Recursive(&Br.Children).All(ctx)
			require.NoError(t, err)
			require.Len(t, got, 4)
		})
	})
}
