package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
)

// Member is a table that a later version calls people.
type Member struct {
	ID   int64
	Name string
}

func (Member) Configure(b *ant.ModelBuilder[Member]) { b.Table("members") }

type MembersDB struct {
	ant.Context
	Members ant.Table[Member]
}

// Person is Member under its new name.
type Person struct {
	ID   int64
	Name string
}

func (Person) Configure(b *ant.ModelBuilder[Person]) { b.Table("people") }

type PeopleDB struct {
	ant.Context
	People ant.Table[Person]
}

// A rename keeps a table's rows. A misspelled new name used to rename the table
// to the typo and then drop it as a table the models no longer have, so now a
// rename must name a table or column the models have and the snapshot does not,
// and diff writes nothing otherwise.
func TestRenameTargets(t *testing.T) {
	each(t, func(t *testing.T, p *project) {
		must[MembersDB](p, "diff", "start")
		must[MembersDB](p, "up")
		p.sql("INSERT INTO members (name) VALUES ('ann')")

		refused := []struct {
			name   string
			rename string
			err    string
		}{
			{
				"a misspelled table",
				"members=peple",
				"models have no table peple; new tables in the models: people",
			},
			{"a table onto itself", "members=members", "the snapshot already has a table members"},
			{"a misspelled column", "members.name=nmae", "models have no table members"},
		}

		for _, tt := range refused {
			t.Run(tt.name, func(t *testing.T) {
				_, err := run[PeopleDB](p, "diff", "typo", "--rename", tt.rename)
				require.ErrorContains(t, err, tt.err)
				require.False(t, p.exists("versions/0002_typo.up.sql"), "nothing was written")
			})
		}

		t.Run("the right name keeps the rows", func(t *testing.T) {
			must[PeopleDB](p, "diff", "rename", "--rename", "members=people")

			text := p.read("versions/0002_rename.up.sql")
			require.Contains(t, text, `ALTER TABLE "members" RENAME TO "people";`)
			require.NotContains(t, text, "DROP TABLE")

			must[PeopleDB](p, "up")
			require.Equal(t, "ann", scalar[string](p, "SELECT name FROM people"))
		})
	})
}
