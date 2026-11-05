package schema_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant/dialect/postgres"
	"github.com/skudra-go/ant/schema"
)

func ddl(t *testing.T) schema.Dialect {
	t.Helper()

	d, err := schema.Of(postgres.Dialect)
	require.NoError(t, err)

	return d
}

func users() *schema.Table {
	return &schema.Table{
		Name: "users",
		Columns: []*schema.Column{
			{Name: "id", Type: "bigint", Identity: true},
			{Name: "name", Type: "text"},
			{Name: "email", Type: "text"},
			{Name: "bio", Type: "text", Nullable: true},
		},
		PrimaryKey: &schema.Key{Name: "users_pkey", Columns: []string{"id"}},
		Uniques:    []*schema.Key{{Name: "users_email_key", Columns: []string{"email"}}},
	}
}

func posts() *schema.Table {
	return &schema.Table{
		Name: "posts",
		Columns: []*schema.Column{
			{Name: "id", Type: "bigint", Identity: true},
			{Name: "user_id", Type: "bigint"},
		},
		PrimaryKey: &schema.Key{Name: "posts_pkey", Columns: []string{"id"}},
		Indexes: []*schema.Index{
			{Name: "posts_user_id_idx", Parts: []schema.IndexPart{{Column: "user_id"}}},
		},
		ForeignKeys: []*schema.ForeignKey{{
			Name:       "posts_user_id_fkey",
			Columns:    []string{"user_id"},
			RefTable:   "users",
			RefColumns: []string{"id"},
			OnDelete:   "CASCADE",
		}},
	}
}

func of(tables ...*schema.Table) *schema.Schema { return &schema.Schema{Tables: tables} }

// run returns the statements of the plan between two schemas.
func run(t *testing.T, from, to *schema.Schema, renames ...string) []string {
	t.Helper()
	d := ddl(t)
	plan, err := schema.Diff(from, to, renames...)
	require.NoError(t, err)
	schema.Backfill(d, plan)
	out, err := schema.Statements(d, plan)
	require.NoError(t, err)

	return out
}

func TestNewTablesComeBeforeTheirForeignKeys(t *testing.T) {
	// posts sorts first, and refers to users, which only works because foreign
	// keys come after every table.
	got := run(t, nil, of(posts(), users()))
	require.Len(t, got, 4)
	require.Contains(t, got[0], `CREATE TABLE "posts"`)
	require.Contains(t, got[1], `CREATE TABLE "users"`)
	require.Equal(t, `CREATE INDEX "posts_user_id_idx" ON "posts" ("user_id")`, got[2])

	require.Equal(
		t,
		`ALTER TABLE "posts" ADD CONSTRAINT "posts_user_id_fkey" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE CASCADE`,
		got[3],
	)
}

func TestDroppedTableLosesItsForeignKeysFirst(t *testing.T) {
	got := run(t, of(users(), posts()), of(users()))
	require.Equal(
		t,
		[]string{`ALTER TABLE "posts" DROP CONSTRAINT "posts_user_id_fkey"`, `DROP TABLE "posts"`},
		got,
	)
}

func TestNoChanges(t *testing.T) {
	require.Empty(t, run(t, of(users(), posts()), of(posts(), users())))
}

func TestColumnChanges(t *testing.T) {
	t.Run("a nullable column has nothing to fill", func(t *testing.T) {
		next := users()
		next.Columns = append(
			next.Columns,
			&schema.Column{Name: "nick", Type: "text", Nullable: true},
		)

		require.Equal(
			t,
			[]string{`ALTER TABLE "users" ADD COLUMN "nick" text`},
			run(t, of(users()), of(next)),
		)
	})

	t.Run("a NOT NULL column fills existing rows, then stops having a default", func(t *testing.T) {
		next := users()
		next.Columns = append(next.Columns, &schema.Column{Name: "age", Type: "bigint"})

		require.Equal(
			t,
			[]string{
				`ALTER TABLE "users" ADD COLUMN "age" bigint NOT NULL DEFAULT 0`,
				`ALTER TABLE "users" ALTER COLUMN "age" DROP DEFAULT`,
			},
			run(t, of(users()), of(next)),
		)
	})

	t.Run("a column with its own default keeps it", func(t *testing.T) {
		next := users()
		next.Columns = append(
			next.Columns,
			&schema.Column{Name: "role", Type: "text", Default: "'member'"},
		)

		require.Equal(
			t,
			[]string{`ALTER TABLE "users" ADD COLUMN "role" text NOT NULL DEFAULT 'member'`},
			run(t, of(users()), of(next)),
		)
	})

	t.Run("a type with no zero value says so", func(t *testing.T) {
		next := users()
		next.Columns = append(next.Columns, &schema.Column{Name: "status", Type: "user_status"})
		plan, err := schema.Diff(of(users()), of(next))
		require.NoError(t, err)
		schema.Backfill(ddl(t), plan)
		require.Contains(t, plan.Warnings()[0], "fails if the table has rows")
	})

	t.Run("each facet alters alone", func(t *testing.T) {
		next := users()
		next.Column("name").Type = "varchar(100)"
		next.Column("bio").Nullable = false
		next.Column("email").Default = "''"

		require.Equal(
			t,
			[]string{
				`ALTER TABLE "users" ALTER COLUMN "name" TYPE varchar(100) USING "name"::varchar(100)`,
				`ALTER TABLE "users" ALTER COLUMN "email" SET DEFAULT ''`,
				`ALTER TABLE "users" ALTER COLUMN "bio" SET NOT NULL`,
			},
			run(t, of(users()), of(next)),
		)
	})

	t.Run("a generated column is replaced", func(t *testing.T) {
		next := users()
		next.Column("name").Generated = "upper(email)"

		require.Equal(
			t,
			[]string{
				`ALTER TABLE "users" DROP COLUMN "name"`,
				`ALTER TABLE "users" ADD COLUMN "name" text NOT NULL GENERATED ALWAYS AS (upper(email)) STORED`,
			},
			run(t, of(users()), of(next)),
		)
	})
}

// A replaced column takes along every index and constraint that uses it, so the
// plan drops them first and makes them again after.
func TestReplacedColumns(t *testing.T) {
	// slugged has a generated name, and objects that use it in every way an
	// object can, beside some that do not.
	slugged := func(expr string) *schema.Table {
		t := users()
		t.Columns = append(t.Columns, &schema.Column{Name: "username", Type: "text"})
		t.Column("name").Generated = expr
		t.Column("name").Comment = "shown to others"

		t.Checks = []*schema.Check{
			{Name: "users_name_check", Expr: "length(name) > 0"},
			{Name: "users_username_check", Expr: "username <> ''"},
		}

		t.Indexes = []*schema.Index{
			{Name: "users_lower_idx", Parts: []schema.IndexPart{{Expr: "lower(NAME)"}}},
			{
				Name:  "users_bio_idx",
				Parts: []schema.IndexPart{{Column: "bio"}},
				Where: `"name" IS NOT NULL`,
			},
			{
				Name:       "users_email_idx",
				Parts:      []schema.IndexPart{{Column: "email"}},
				Include:    []string{"name"},
				Concurrent: true,
			},
			{Name: "users_username_idx", Parts: []schema.IndexPart{{Column: "username"}}},
		}

		return t
	}

	t.Run("a new expression rebuilds what uses the column", func(t *testing.T) {
		require.Equal(
			t,
			[]string{
				`ALTER TABLE "users" DROP CONSTRAINT "users_name_check"`,
				`DROP INDEX "users_lower_idx"`,
				`DROP INDEX "users_bio_idx"`,
				`DROP INDEX "users_email_idx"`,
				`ALTER TABLE "users" DROP COLUMN "name"`,
				`ALTER TABLE "users" ADD COLUMN "name" text NOT NULL GENERATED ALWAYS AS (upper(email)) STORED`,
				`ALTER TABLE "users" ADD CONSTRAINT "users_name_check" CHECK (length(name) > 0)`,
				`CREATE INDEX "users_lower_idx" ON "users" ((lower(NAME)))`,
				`CREATE INDEX "users_bio_idx" ON "users" ("bio") WHERE "name" IS NOT NULL`,
				`CREATE INDEX "users_email_idx" ON "users" ("email") INCLUDE ("name")`,
				`COMMENT ON COLUMN "users"."name" IS 'shown to others'`,
			},
			run(t, of(slugged("lower(email)")), of(slugged("upper(email)"))),
		)
	})

	t.Run("every index in the plan stays in its transaction", func(t *testing.T) {
		// The concurrent index stops including name, and another table gets a
		// concurrent index of its own. Built online, either would take the
		// transaction away from the replaced column.
		next := slugged("upper(email)")
		next.Indexes[2].Include = nil
		other := posts()
		other.Indexes[0].Concurrent = true

		other.Indexes = append(other.Indexes, &schema.Index{
			Name:       "posts_id_idx",
			Parts:      []schema.IndexPart{{Column: "id"}},
			Concurrent: true,
		})

		plan, err := schema.Diff(of(slugged("lower(email)"), posts()), of(next, other))
		require.NoError(t, err)
		require.False(t, plan.Online())

		got := run(t, of(slugged("lower(email)"), posts()), of(next, other))
		require.Contains(t, got, `DROP INDEX "users_email_idx"`)
		require.Contains(t, got, `CREATE INDEX "users_email_idx" ON "users" ("email")`)
		require.Contains(t, got, `CREATE INDEX "posts_id_idx" ON "posts" ("id")`)

		require.Contains(
			t,
			strings.Join(plan.Warnings(), "\n"),
			"posts_id_idx is built without CONCURRENTLY, which blocks writes to posts while it builds",
		)
	})

	t.Run("without a replaced column, indexes stay online", func(t *testing.T) {
		next := posts()
		next.Indexes = append(next.Indexes, &schema.Index{
			Name:       "posts_id_idx",
			Parts:      []schema.IndexPart{{Column: "id"}},
			Concurrent: true,
		})

		require.Equal(
			t,
			[]string{`CREATE INDEX CONCURRENTLY "posts_id_idx" ON "posts" ("id")`},
			run(t, of(posts()), of(next)),
		)
	})

	t.Run("a column that becomes generated does too", func(t *testing.T) {
		got := run(t, of(slugged("")), of(slugged("upper(email)")))
		require.Contains(t, got, `ALTER TABLE "users" DROP COLUMN "name"`)
		require.Contains(t, got, `CREATE INDEX "users_lower_idx" ON "users" ((lower(NAME)))`)
	})

	t.Run("a column that stops being generated keeps its values and indexes", func(t *testing.T) {
		require.Equal(
			t,
			[]string{`ALTER TABLE "users" ALTER COLUMN "name" DROP EXPRESSION`},
			run(t, of(slugged("lower(email)")), of(slugged(""))),
		)
	})

	t.Run("a renamed index is rebuilt under its new name", func(t *testing.T) {
		next := slugged("upper(email)")
		next.Indexes[0].Name = "users_name_lower_idx"
		got := run(t, of(slugged("lower(email)")), of(next))
		require.Contains(t, got, `DROP INDEX "users_lower_idx"`)
		require.Contains(t, got, `CREATE INDEX "users_name_lower_idx" ON "users" ((lower(NAME)))`)
		require.NotContains(t, strings.Join(got, "\n"), "RENAME")
	})

	t.Run("keys on the column are rebuilt, in this table and others", func(t *testing.T) {
		keyed := func(expr string) *schema.Table {
			t := users()
			t.Column("name").Generated = expr
			t.PrimaryKey.Columns = []string{"id", "name"}
			t.Uniques = []*schema.Key{{Name: "users_name_key", Columns: []string{"name"}}}

			return t
		}

		cards := &schema.Table{
			Name:    "cards",
			Columns: []*schema.Column{{Name: "user_name", Type: "text"}},
			ForeignKeys: []*schema.ForeignKey{{
				Name:       "cards_user_name_fkey",
				Columns:    []string{"user_name"},
				RefTable:   "users",
				RefColumns: []string{"name"},
			}},
		}

		require.Equal(
			t,
			[]string{
				`ALTER TABLE "cards" DROP CONSTRAINT "cards_user_name_fkey"`,
				`ALTER TABLE "users" DROP CONSTRAINT "users_pkey"`,
				`ALTER TABLE "users" DROP CONSTRAINT "users_name_key"`,
				`ALTER TABLE "users" DROP COLUMN "name"`,
				`ALTER TABLE "users" ADD COLUMN "name" text NOT NULL GENERATED ALWAYS AS (upper(email)) STORED`,
				`ALTER TABLE "users" ADD CONSTRAINT "users_pkey" PRIMARY KEY ("id", "name")`,
				`ALTER TABLE "users" ADD CONSTRAINT "users_name_key" UNIQUE ("name")`,
				`ALTER TABLE "cards" ADD CONSTRAINT "cards_user_name_fkey" FOREIGN KEY ("user_name") REFERENCES "users" ("name")`,
			},
			run(t, of(keyed("lower(email)"), cards), of(keyed("upper(email)"), cards)),
		)
	})

	t.Run("a name with a double quote in it is found quoted", func(t *testing.T) {
		quoted := func(expr string) *schema.Table {
			return &schema.Table{
				Name: "odd",
				Columns: []*schema.Column{
					{Name: "label", Type: "text"},
					{Name: `a"b`, Type: "text", Generated: expr},
				},
				Checks: []*schema.Check{{Name: "odd_check", Expr: `"a""b" <> ''`}},
			}
		}

		require.Contains(
			t,
			run(t, of(quoted("lower(label)")), of(quoted("upper(label)"))),
			`ALTER TABLE "odd" ADD CONSTRAINT "odd_check" CHECK ("a""b" <> '')`,
		)
	})

	t.Run("a name spelled in Unicode escapes is found", func(t *testing.T) {
		escaped := func(expr string) *schema.Table {
			return &schema.Table{
				Name: "tags",
				Columns: []*schema.Column{
					{Name: "label", Type: "text"},
					{Name: "slug", Type: "text", Generated: expr},
					{Name: "🐌", Type: "text", Generated: expr},
				},
				// One way each to write an escape, around a string that looks
				// like one, and with an escape letter that the name has too.
				Checks: []*schema.Check{
					{Name: "tags_slug_check", Expr: `'U&"' <> U&"sl\0075g"`},
					{Name: "tags_case_check", Expr: `U&"slL0075g" UESCAPE 'L' <> ''`},
					{Name: "tags_snail_check", Expr: `U&"\D83D\DC0C" <> ''`},
				},
				Indexes: []*schema.Index{
					{
						Name: "tags_lower_idx",
						Parts: []schema.IndexPart{
							{Expr: `lower(U&"sl!0075g" /* any escape */ UESCAPE '!')`},
						},
					},
					{
						Name:  "tags_label_idx",
						Parts: []schema.IndexPart{{Column: "label"}},
						Where: `u&"s\+00006Cug" IS NOT NULL`,
					},
				},
			}
		}

		require.Equal(
			t,
			[]string{
				`ALTER TABLE "tags" DROP CONSTRAINT "tags_slug_check"`,
				`ALTER TABLE "tags" DROP CONSTRAINT "tags_case_check"`,
				`ALTER TABLE "tags" DROP CONSTRAINT "tags_snail_check"`,
				`DROP INDEX "tags_lower_idx"`,
				`DROP INDEX "tags_label_idx"`,
				`ALTER TABLE "tags" DROP COLUMN "slug"`,
				`ALTER TABLE "tags" ADD COLUMN "slug" text NOT NULL GENERATED ALWAYS AS (upper(label)) STORED`,
				`ALTER TABLE "tags" DROP COLUMN "🐌"`,
				`ALTER TABLE "tags" ADD COLUMN "🐌" text NOT NULL GENERATED ALWAYS AS (upper(label)) STORED`,
				`ALTER TABLE "tags" ADD CONSTRAINT "tags_slug_check" CHECK ('U&"' <> U&"sl\0075g")`,
				`ALTER TABLE "tags" ADD CONSTRAINT "tags_case_check" CHECK (U&"slL0075g" UESCAPE 'L' <> '')`,
				`ALTER TABLE "tags" ADD CONSTRAINT "tags_snail_check" CHECK (U&"\D83D\DC0C" <> '')`,
				`CREATE INDEX "tags_lower_idx" ON "tags" ((lower(U&"sl!0075g" /* any escape */ UESCAPE '!')))`,
				`CREATE INDEX "tags_label_idx" ON "tags" ("label") WHERE u&"s\+00006Cug" IS NOT NULL`,
			},
			run(t, of(escaped("lower(label)")), of(escaped("upper(label)"))),
		)
	})
}

// PostgreSQL will not drop a unique key while a foreign key depends on it, so a
// foreign key goes before the unique key it points at, and comes back after.
func TestForeignKeysFollowTheirUniqueKey(t *testing.T) {
	codes := func(include ...string) *schema.Table {
		return &schema.Table{
			Name: "codes",
			Columns: []*schema.Column{
				{Name: "region", Type: "text"},
				{Name: "code", Type: "text"},
				{Name: "label", Type: "text"},
			},
			Indexes: []*schema.Index{
				{
					Name:    "codes_key_idx",
					Parts:   []schema.IndexPart{{Column: "region"}, {Column: "code"}},
					Unique:  true,
					Include: include,
				},
				{
					Name:   "codes_label_idx",
					Parts:  []schema.IndexPart{{Column: "label"}},
					Unique: true,
				},
			},
		}
	}

	// The key names its columns in another order than the index, which still
	// counts, as it does for PostgreSQL.
	uses := &schema.Table{
		Name: "uses",
		Columns: []*schema.Column{
			{Name: "code", Type: "text"},
			{Name: "region", Type: "text"},
			{Name: "label", Type: "text"},
		},
		ForeignKeys: []*schema.ForeignKey{
			{
				Name:       "uses_code_fkey",
				Columns:    []string{"code", "region"},
				RefTable:   "codes",
				RefColumns: []string{"code", "region"},
			},
			{
				Name:       "uses_label_fkey",
				Columns:    []string{"label"},
				RefTable:   "codes",
				RefColumns: []string{"label"},
			},
		},
	}

	require.Equal(
		t,
		[]string{
			`ALTER TABLE "uses" DROP CONSTRAINT "uses_code_fkey"`,
			`DROP INDEX "codes_key_idx"`,
			`CREATE UNIQUE INDEX "codes_key_idx" ON "codes" ("region", "code") INCLUDE ("label")`,
			`ALTER TABLE "uses" ADD CONSTRAINT "uses_code_fkey" FOREIGN KEY ("code", "region") REFERENCES "codes" ("code", "region")`,
		},
		run(t, of(codes(), uses), of(codes("label"), uses)),
	)
}

func TestWarnings(t *testing.T) {
	warnings := func(t *testing.T, from, to *schema.Schema) string {
		plan, err := schema.Diff(from, to)
		require.NoError(t, err)

		return strings.Join(plan.Warnings(), "\n")
	}
	t.Run("dropping data", func(t *testing.T) {
		next := users()
		next.Columns = next.Columns[:3]
		require.Contains(t, warnings(t, of(users()), of(next)), "drops column users.bio")
		require.Contains(t, warnings(t, of(users()), of()), "drops table users")
	})

	t.Run("changing a type", func(t *testing.T) {
		next := users()
		next.Column("name").Type = "integer"
		require.Contains(t, warnings(t, of(users()), of(next)), "may fail or lose data")
	})

	t.Run("forbidding NULL", func(t *testing.T) {
		next := users()
		next.Column("bio").Nullable = false
		require.Contains(t, warnings(t, of(users()), of(next)), "fails if any row holds NULL")
	})

	t.Run("replacing a column", func(t *testing.T) {
		generated := users()
		generated.Column("name").Generated = "lower(email)"
		trimmed := users()
		trimmed.Column("name").Generated = "lower(trim(email))"

		require.Contains(
			t,
			warnings(t, of(users()), of(generated)),
			"stored values are replaced by computed ones",
		)

		require.Contains(
			t,
			warnings(t, of(generated), of(trimmed)),
			"users.name gets a new expression, so it is dropped and added again",
		)

		require.Empty(t, warnings(t, of(generated), of(users())))
	})

	t.Run("a new table has no rows to worry about", func(t *testing.T) {
		require.Empty(t, warnings(t, nil, of(users(), posts())))
	})
}

func TestRenames(t *testing.T) {
	t.Run("a rename keeps the data, and constraints follow it", func(t *testing.T) {
		next := users()
		next.Column("email").Name = "mail"
		next.Uniques[0] = &schema.Key{Name: "users_mail_key", Columns: []string{"mail"}}

		require.Equal(
			t,
			[]string{
				`ALTER TABLE "users" RENAME COLUMN "email" TO "mail"`,
				`ALTER TABLE "users" RENAME CONSTRAINT "users_email_key" TO "users_mail_key"`,
			},
			run(t, of(users()), of(next), "users.email=mail"),
		)
	})

	t.Run("a table rename updates foreign keys that point at it", func(t *testing.T) {
		people := users()
		people.Name = "people"
		people.PrimaryKey.Name = "people_pkey"
		people.Uniques[0].Name = "people_email_key"
		child := posts()
		child.ForeignKeys[0].RefTable = "people"

		require.Equal(
			t,
			[]string{
				`ALTER TABLE "users" RENAME TO "people"`,
				`ALTER TABLE "people" RENAME CONSTRAINT "users_pkey" TO "people_pkey"`,
				`ALTER TABLE "people" RENAME CONSTRAINT "users_email_key" TO "people_email_key"`,
			},
			run(t, of(users(), posts()), of(people, child), "users=people"),
		)
	})

	t.Run(
		"a rename that names nothing is an error, because it was meant to prevent a drop",
		func(t *testing.T) {
			_, err := schema.Diff(of(users()), of(users()), "users.nope=x")
			require.ErrorContains(t, err, "no column nope")
			_, err = schema.Diff(of(users()), of(users()), "ghosts=x")
			require.ErrorContains(t, err, "no table ghosts")
			_, err = schema.Diff(of(users()), of(users()), "users.email=x")
			require.ErrorContains(t, err, "models have no column users.x")
			_, err = schema.Diff(of(users()), of(users()), "nonsense")
			require.ErrorContains(t, err, "want table=new_name")
		},
	)

	// A rename is there to keep data. One whose new name the models do not have
	// would rename a table and then drop it as unknown, so it is refused, and so
	// is one onto a name that is already taken.
	t.Run(
		"a rename must land on a name the models have and the snapshot lacks",
		func(t *testing.T) {
			people := users()
			people.Name = "people"

			withMail := users()
			withMail.Column("email").Name = "mail"
			withMail.Column("bio").Name = "about"

			orders := &schema.Table{
				Schema:  "sales",
				Name:    "orders",
				Columns: []*schema.Column{{Name: "id", Type: "bigint"}},
			}

			purchases := &schema.Table{
				Schema:  "sales",
				Name:    "purchases",
				Columns: []*schema.Column{{Name: "id", Type: "bigint"}},
			}

			cases := []struct {
				name    string
				from    *schema.Schema
				to      *schema.Schema
				renames []string
				err     string
			}{
				{
					"a table, misspelled",
					of(users(), posts()),
					of(people, posts()),
					[]string{"users=peple"},
					"models have no table peple; new tables in the models: people",
				},
				{
					"a table, onto a table the snapshot has",
					of(users(), posts()),
					of(people, posts()),
					[]string{"users=posts"},
					"the snapshot already has a table posts",
				},
				{
					"a table, to its own name",
					of(users()),
					of(users()),
					[]string{"users=users"},
					"the snapshot already has a table users",
				},
				{
					"two tables to one name",
					of(users(), posts()),
					of(people),
					[]string{"users=people", "posts=people"},
					"the snapshot already has a table people",
				},
				{
					"a table onto one migrations leave alone",
					&schema.Schema{
						Tables:    []*schema.Table{users()},
						Unmanaged: []schema.Ref{{Name: "people"}},
					},
					of(people),
					[]string{"users=people"},
					"a table people already exists, outside migrations",
				},
				{
					"a table, where the only new one exists outside migrations",
					&schema.Schema{
						Tables:    []*schema.Table{users()},
						Unmanaged: []schema.Ref{{Name: "people"}},
					},
					of(people),
					[]string{"users=peple"},
					"models have no table peple; the models have no new table",
				},
				{
					"a table into another schema",
					of(orders),
					of(purchases),
					[]string{"sales.orders=archive.orders"},
					"models have no table archive.orders in schema sales",
				},
				{
					"a table, with nothing new to rename it to",
					of(users()),
					of(users()),
					[]string{"users=people"},
					"models have no table people; the models have no new table",
				},
				{
					"a column, onto a column the snapshot has",
					of(users()),
					of(withMail),
					[]string{"users.email=name"},
					"table users already has a column name",
				},
				{
					"a column, to its own name",
					of(users()),
					of(users()),
					[]string{"users.email=email"},
					"table users already has a column email",
				},
				{
					"two columns to one name",
					of(users()),
					of(withMail),
					[]string{"users.email=mail", "users.bio=mail"},
					"table users already has a column mail",
				},
				{
					"a column, misspelled",
					of(users()),
					of(withMail),
					[]string{"users.email=mial"},
					"models have no column users.mial; new columns of users in the models: mail, about",
				},
			}

			for _, tt := range cases {
				t.Run(tt.name, func(t *testing.T) {
					plan, err := schema.Diff(tt.from, tt.to, tt.renames...)
					require.ErrorContains(t, err, tt.err)
					require.Nil(t, plan)
				})
			}

			valid := []struct {
				name    string
				from    *schema.Schema
				to      *schema.Schema
				renames []string
				want    []string
			}{
				{
					"a table in a schema",
					of(orders),
					of(purchases),
					[]string{"sales.orders=purchases"},
					[]string{`ALTER TABLE "sales"."orders" RENAME TO "purchases"`},
				},
				{
					"a column of a table renamed before it",
					of(users()),
					of(func() *schema.Table {
						t := users()
						t.Name = "people"
						t.Column("email").Name = "mail"

						return t
					}()),
					[]string{"users=people", "people.email=mail"},
					[]string{
						`ALTER TABLE "users" RENAME TO "people"`,
						`ALTER TABLE "people" RENAME COLUMN "email" TO "mail"`,
					},
				},
			}

			for _, tt := range valid {
				t.Run("still allowed: "+tt.name, func(t *testing.T) {
					got := run(t, tt.from, tt.to, tt.renames...)
					require.Subset(t, got, tt.want)
				})
			}
		},
	)

	t.Run("a drop and add of one type is a hint, not a guess", func(t *testing.T) {
		next := users()
		next.Column("bio").Name = "about"
		plan, err := schema.Diff(of(users()), of(next))
		require.NoError(t, err)
		require.Len(t, plan.Hints, 1)
		require.Equal(t, "users.bio=about", plan.Hints[0].Flag())
		text, err := schema.Script(ddl(t), plan)
		require.NoError(t, err)
		require.Contains(t, text, `--   ALTER TABLE "users" RENAME COLUMN "bio" TO "about";`)
		require.Contains(t, text, `ALTER TABLE "users" DROP COLUMN "bio";`)
	})

	t.Run("a table with the same columns under a new name is a hint", func(t *testing.T) {
		people := users()
		people.Name = "people"
		plan, err := schema.Diff(of(users()), of(people))
		require.NoError(t, err)
		require.Len(t, plan.Hints, 1)
		require.Equal(t, "users=people", plan.Hints[0].Flag())
	})
}

func TestObjectsWithANewNameAreRenamed(t *testing.T) {
	t.Run("an index is not rebuilt for its name", func(t *testing.T) {
		next := posts()
		next.Indexes[0].Name = "posts_author_idx"

		require.Equal(
			t,
			[]string{`ALTER INDEX "posts_user_id_idx" RENAME TO "posts_author_idx"`},
			run(t, of(users(), posts()), of(users(), next)),
		)
	})

	t.Run("a changed index is replaced", func(t *testing.T) {
		next := posts()
		next.Indexes[0].Parts[0].Desc = true

		require.Equal(
			t,
			[]string{
				`DROP INDEX "posts_user_id_idx"`,
				`CREATE INDEX "posts_user_id_idx" ON "posts" ("user_id" DESC)`,
			},
			run(t, of(users(), posts()), of(users(), next)),
		)
	})

	t.Run("a changed foreign key is replaced", func(t *testing.T) {
		next := posts()
		next.ForeignKeys[0].OnDelete = "SET NULL"
		got := run(t, of(users(), posts()), of(users(), next))
		require.Equal(t, `ALTER TABLE "posts" DROP CONSTRAINT "posts_user_id_fkey"`, got[0])
		require.Contains(t, got[1], "ON DELETE SET NULL")
	})

	t.Run("building an index concurrently is not a schema change", func(t *testing.T) {
		next := posts()
		next.Indexes[0].Concurrent = true
		require.Empty(t, run(t, of(users(), posts()), of(users(), next)))
	})
}

func TestConcurrentIndex(t *testing.T) {
	index := &schema.Index{
		Name:       "users_bio_idx",
		Parts:      []schema.IndexPart{{Column: "bio"}},
		Concurrent: true,
	}

	next := users()
	next.Indexes = []*schema.Index{index}

	t.Run(
		"on an existing table it builds online, in a file with no transaction",
		func(t *testing.T) {
			plan, err := schema.Diff(of(users()), of(next))
			require.NoError(t, err)
			require.True(t, plan.Online())
			text, err := schema.Script(ddl(t), plan)
			require.NoError(t, err)
			require.Contains(t, text, schema.NoTransaction)
			require.Contains(t, text, "CREATE INDEX CONCURRENTLY")
		},
	)

	t.Run("on a new table there is nothing to wait for", func(t *testing.T) {
		plan, err := schema.Diff(nil, of(next))
		require.NoError(t, err)
		require.False(t, plan.Online())
	})
}

func TestSchemas(t *testing.T) {
	billing := &schema.Table{
		Schema:  "billing",
		Name:    "invoices",
		Columns: []*schema.Column{{Name: "id", Type: "bigint"}},
	}
	got := run(
		t,
		nil,
		&schema.Schema{Schemas: []string{"billing"}, Tables: []*schema.Table{billing}},
	)
	require.Equal(t, `CREATE SCHEMA IF NOT EXISTS "billing"`, got[0])
	require.Contains(t, got[1], `CREATE TABLE "billing"."invoices"`)
}

func TestSnapshot(t *testing.T) {
	s := of(users(), posts())
	s.Dialect = "postgres"

	t.Run("round trip", func(t *testing.T) {
		data, err := s.Encode()
		require.NoError(t, err)
		back, err := schema.Decode(data)
		require.NoError(t, err)
		require.Empty(t, run(t, s, back))
		require.Equal(t, schema.Format, back.Format)
	})

	t.Run("the same models always make the same bytes", func(t *testing.T) {
		a, err := of(users(), posts()).Encode()
		require.NoError(t, err)
		b, err := of(posts(), users()).Encode()
		require.NoError(t, err)
		require.Equal(t, string(a), string(b))
	})

	t.Run("a newer format is refused, not misread", func(t *testing.T) {
		_, err := schema.Decode([]byte(`{"format": 99}`))
		require.ErrorIs(t, err, schema.ErrNewerFormat)
	})

	t.Run("an empty file is an empty schema", func(t *testing.T) {
		got, err := schema.Decode(nil)
		require.NoError(t, err)
		require.Empty(t, got.Tables)
	})
}

func TestLongNames(t *testing.T) {
	d := ddl(t)
	long := strings.Repeat("x", 80)
	a, b := schema.IndexName(d, long, "a"), schema.IndexName(d, long, "b")
	require.LessOrEqual(t, len(a), 63)
	require.NotEqual(t, a, b, "two long names must not become one")
	require.Equal(t, "users_email_idx", schema.IndexName(d, "users", "email"))
	require.Equal(t, "users_pkey", schema.PrimaryKeyName(d, "users"))
	require.Equal(t, "users_email_key", schema.UniqueName(d, "users", "email"))
	require.Equal(t, "posts_user_id_fkey", schema.ForeignKeyName(d, "posts", "user_id"))
}

func TestUnsupportedDialect(t *testing.T) {
	_, err := schema.Of(struct{ named }{})
	require.ErrorIs(t, err, schema.ErrUnsupported)
	require.ErrorContains(t, err, "mysql")
}

type named struct{}

func (named) Name() string { return "mysql" }

func TestZeroValues(t *testing.T) {
	d := ddl(t)

	for columnType, want := range map[string]string{
		"bigint":        "0",
		"numeric(10,2)": "0",
		"text":          "''",
		"varchar(10)":   "''",
		"boolean":       "false",
		"timestamptz":   "now()",
		"jsonb":         "'null'",
		"text[]":        "'{}'",
		"uuid":          "gen_random_uuid()",
		"my_enum":       "",
	} {
		require.Equal(t, want, d.Zero(columnType), columnType)
	}
}

func TestColumnTypes(t *testing.T) {
	d := ddl(t)

	type Status string

	check := func(want string, value any, facets ...schema.Facets) {
		t.Helper()

		var f schema.Facets

		if len(facets) > 0 {
			f = facets[0]
		}

		got, ok := d.ColumnType(reflect.TypeOf(value), f)
		require.True(t, ok)
		require.Equal(t, want, got)
	}
	check("text", "")
	check("text", Status(""))
	check("varchar(20)", "", schema.Facets{Length: 20})
	check("bigint", 1)
	check("bigint", int64(1))
	check("integer", int32(1))
	check("smallint", int16(1))
	check("boolean", true)
	check("real", float32(1))
	check("double precision", 1.0)
	check("numeric(10,2)", 1.0, schema.Facets{Precision: 10, Scale: 2})
	check("bytea", []byte{})
	check("uuid", [16]byte{})

	_, ok := d.ColumnType(reflect.TypeOf(struct{}{}), schema.Facets{})
	require.False(t, ok)
}
