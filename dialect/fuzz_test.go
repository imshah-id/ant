package dialect_test

import (
	"strings"
	"testing"

	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/dialect/mysql"
	"github.com/skudra-go/ant/dialect/postgres"
	"github.com/skudra-go/ant/dialect/sqlite"
	"github.com/skudra-go/ant/dialect/sqlserver"
	"github.com/skudra-go/ant/qb"
)

var everyDialect = []dialect.Dialect{
	postgres.Dialect,
	mysql.Dialect,
	mysql.MariaDB,
	sqlite.Dialect,
	sqlserver.Dialect,
}

// No comment or hint text can end the comment ant wraps it in, or open another,
// on any dialect: the statement holds exactly the comments ant wrote. go test
// runs the seeds; go test -fuzz FuzzCommentsStayComments searches further.
func FuzzCommentsStayComments(f *testing.F) {
	for _, seed := range []string{
		"",
		"plain",
		"*/",
		"/*",
		"*/ DROP TABLE users; /*",
		"*//*",
		"/*/",
		"**//",
		"a*/b",
		"--",
		"-- */",
		"\n*/\n",
		"\x00",
		"é*/é",
		"*",
		"/",
		"/**/",
		"*/*/*/",
		"+ */ x /*+",
	} {
		f.Add(seed, seed)
	}

	f.Fuzz(func(t *testing.T, comment, hint string) {
		users := &qb.TableRef{Name: "users"}
		stmt := &qb.Select{
			Comment: comment,
			Hint:    hint,
			Columns: []qb.Selected{{Value: qb.Column{Table: users, Name: "id"}}},
			From:    qb.Source{Table: users},
		}

		want := 0

		if comment != "" {
			want++
		}

		if hint != "" {
			want++
		}

		for _, d := range everyDialect {
			text, _, err := d.Render(stmt)
			if err != nil {
				t.Fatal(err)
			}

			if opens, closes := strings.Count(
				text,
				"/*",
			), strings.Count(
				text,
				"*/",
			); opens != want ||
				closes != want {
				t.Fatalf(
					"%s: %d openings and %d closings, want %d of each: %q",
					d.Name(),
					opens,
					closes,
					want,
					text,
				)
			}

			if !strings.HasSuffix(text, `FROM "users"`) &&
				!strings.HasSuffix(text, "FROM `users`") &&
				!strings.HasSuffix(text, "FROM [users]") {
				t.Fatalf("%s: the statement does not end as written: %q", d.Name(), text)
			}
		}
	})
}

// Quoting an identifier and reading it back gives the identifier, whatever it
// holds, so a name cannot leave its quotes.
func FuzzQuotingRoundTrips(f *testing.F) {
	for _, seed := range []string{"users", `a"b`, "a`b", "a]b", "[x]", `""`, "``", "]]", " ", "é", "\x00"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		for _, d := range everyDialect {
			quoted := d.Quote(name)
			if len(quoted) < 2 {
				t.Fatalf("%s: %q quoted as %q", d.Name(), name, quoted)
			}

			open, closing := quoted[:1], quoted[len(quoted)-1:]
			inner := quoted[1 : len(quoted)-1]

			if strings.ReplaceAll(
				inner,
				closing+closing,
				"",
			) != strings.ReplaceAll(
				strings.ReplaceAll(inner, closing+closing, ""),
				closing,
				"",
			) {
				t.Fatalf("%s: %q has a lone %s inside: %q", d.Name(), name, closing, quoted)
			}

			if got := strings.ReplaceAll(inner, closing+closing, closing); got != name {
				t.Fatalf(
					"%s: %q quoted as %q reads back as %q (opened with %s)",
					d.Name(),
					name,
					quoted,
					got,
					open,
				)
			}
		}
	})
}
