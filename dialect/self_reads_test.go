package dialect_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/dialect/mysql"
	"github.com/skudra-go/ant/dialect/postgres"
	"github.com/skudra-go/ant/qb"
)

// MySQL refuses a write whose subquery reads the table being written, so such a
// subquery is wrapped in a derived table there, once, at whatever depth the read
// is. Subqueries of other tables, reads, and other databases are left alone.
func TestSelfReadsAreWrappedOnMySQL(t *testing.T) {
	users := &qb.TableRef{Name: "users"}
	posts := &qb.TableRef{Name: "posts"}
	id := qb.Column{Table: users, Name: "id"}
	userID := qb.Column{Table: posts, Name: "user_id"}
	age := qb.Column{Table: users, Name: "age"}

	sel := func(column qb.Expr, from *qb.TableRef, where ...qb.Cond) *qb.Select {
		return &qb.Select{
			Columns: []qb.Selected{{Value: column}},
			From:    qb.Source{Table: from},
			Where:   where,
		}
	}

	ofUsers := sel(id, users)
	ofPosts := sel(userID, posts)
	set := []qb.Assignment{{Column: "age", Value: qb.Param{Value: 1}}}

	update := func(where ...qb.Cond) *qb.Update {
		return &qb.Update{Table: users, Set: set, Where: where}
	}

	const wrap = "SELECT /*+ NO_MERGE(materialized) */ * FROM ("

	cases := []struct {
		name string
		stmt qb.Statement
		want string
	}{
		{
			"IN the same table",
			update(qb.In{Left: id, Sub: ofUsers}),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (" + wrap +
				"SELECT `users`.`id` FROM `users`) AS materialized)",
		},
		{
			"IN another table",
			update(qb.In{Left: id, Sub: ofPosts}),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (SELECT `posts`.`user_id` FROM `posts`)",
		},
		{
			"a read nested in another table's subquery, wrapped once, outside",
			update(qb.In{Left: id, Sub: sel(userID, posts, qb.In{Left: userID, Sub: ofUsers})}),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (" + wrap +
				"SELECT `posts`.`user_id` FROM `posts` WHERE `posts`.`user_id` IN " +
				"(SELECT `users`.`id` FROM `users`)) AS materialized)",
		},
		{
			"EXISTS",
			&qb.Delete{Table: users, Where: []qb.Cond{qb.Exists{Select: ofUsers, Negate: true}}},
			"DELETE FROM `users` WHERE NOT EXISTS (" + wrap +
				"SELECT `users`.`id` FROM `users`) AS materialized)",
		},
		{
			"a value in SET",
			&qb.Update{
				Table: users,
				Set:   []qb.Assignment{{Column: "age", Value: qb.Subquery{Select: ofUsers}}},
			},
			"UPDATE `users` SET `age` = (" + wrap + "SELECT `users`.`id` FROM `users`) AS materialized)",
		},
		{
			"a read the table's other spelling names",
			update(qb.In{Left: id, Sub: sel(id, &qb.TableRef{Schema: "app", Name: "Users"})}),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (" + wrap +
				"SELECT `users`.`id` FROM `app`.`Users`) AS materialized)",
		},
		{
			"raw SQL that names the table",
			update(
				qb.In{
					Left: id,
					Sub:  sel(userID, posts, qb.Fragment{SQL: "user_id IN (SELECT id FROM USERS)"}),
				},
			),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (" + wrap +
				"SELECT `posts`.`user_id` FROM `posts` WHERE (user_id IN (SELECT id FROM USERS))) AS materialized)",
		},
		{
			"raw SQL that quotes the table",
			update(
				qb.In{
					Left: id,
					Sub: sel(
						userID,
						posts,
						qb.Fragment{SQL: "user_id IN (SELECT id FROM `Users`)"},
					),
				},
			),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (" + wrap +
				"SELECT `posts`.`user_id` FROM `posts` WHERE (user_id IN (SELECT id FROM `Users`))) " +
				"AS materialized)",
		},
		{
			"raw SQL that names the table outside ASCII",
			&qb.Update{
				Table: &qb.TableRef{Name: "Øl"},
				Set:   set,
				Where: []qb.Cond{
					qb.In{
						Left: id,
						Sub: sel(
							userID,
							posts,
							qb.Fragment{SQL: "user_id IN (SELECT id FROM øl)"},
						),
					},
				},
			},
			"UPDATE `Øl` SET `age` = ? WHERE `users`.`id` IN (" + wrap +
				"SELECT `posts`.`user_id` FROM `posts` WHERE (user_id IN (SELECT id FROM øl))) " +
				"AS materialized)",
		},
		{
			"raw SQL that only holds the name in a literal, a comment, or a longer name",
			update(
				qb.In{
					Left: id,
					Sub: sel(userID, posts, qb.Fragment{
						SQL: "posts.kind = 'users' /* users */ AND user_id IN (SELECT id FROM users_old)",
					}),
				},
			),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (SELECT `posts`.`user_id` FROM `posts` " +
				"WHERE (posts.kind = 'users' /* users */ AND user_id IN (SELECT id FROM users_old)))",
		},
		{
			"a filter built at render time",
			update(qb.In{
				Left: id,
				Sub: sel(userID, posts, qb.Lazy{
					Build: func(context.Context) (qb.Cond, error) {
						return qb.In{Left: userID, Sub: ofUsers}, nil
					},
				}),
			}),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (" + wrap +
				"SELECT `posts`.`user_id` FROM `posts` WHERE `posts`.`user_id` IN " +
				"(SELECT `users`.`id` FROM `users`)) AS materialized)",
		},
		{
			"a limited subquery is a derived table already",
			update(qb.In{
				Left: id,
				Sub: &qb.Select{
					Columns: []qb.Selected{{Value: id}},
					From:    qb.Source{Table: users},
					Limit:   new(1),
				},
			}),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (SELECT * FROM " +
				"(SELECT `users`.`id` FROM `users` LIMIT ?) AS limited)",
		},
		{
			"raw SQL outside a subquery does not mark the next one",
			update(qb.Fragment{SQL: "users.age > 1"}, qb.In{Left: id, Sub: ofPosts}),
			"UPDATE `users` SET `age` = ? WHERE (users.age > 1) AND `users`.`id` IN " +
				"(SELECT `posts`.`user_id` FROM `posts`)",
		},
		{
			"columns of one name keep it, as the derived table names its own",
			update(qb.In{
				Left: qb.Row{Values: []qb.Expr{id, id}},
				Sub: &qb.Select{
					Columns: []qb.Selected{
						{Value: id},
						{Value: qb.Column{Table: posts, Name: "id"}},
					},
					From: qb.Source{Table: users},
					Joins: []qb.Join{
						{
							Source: qb.Source{Table: posts},
							On:     []qb.Cond{qb.Compare{Left: userID, Op: qb.OpEq, Right: id}},
						},
					},
				},
			}),
			"UPDATE `users` SET `age` = ? WHERE (`users`.`id`, `users`.`id`) IN (" + wrap +
				"SELECT `users`.`id`, `posts`.`id` FROM `users` JOIN `posts` ON " +
				"`posts`.`user_id` = `users`.`id`) AS materialized(`ant_c1`, `ant_c2`))",
		},
		{
			"a source named like the table",
			update(qb.In{
				Left: id,
				Sub: &qb.Select{
					Columns: []qb.Selected{{Value: qb.Column{Name: "id"}}},
					From:    qb.Source{Name: "users"},
				},
			}),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (" + wrap +
				"SELECT `id` FROM `users`) AS materialized)",
		},
		{
			"a column's own name stays, for HAVING to use",
			&qb.Update{
				Table: users,
				Set: []qb.Assignment{{
					Column: "age",
					Value: qb.Subquery{
						Select: &qb.Select{
							Columns: []qb.Selected{
								{
									Value: qb.Func{Name: "MAX", Args: []qb.Expr{age}},
									Alias: "oldest",
								},
							},
							From: qb.Source{Table: users},
							Having: []qb.Cond{
								qb.Compare{
									Left:  qb.Column{Name: "oldest"},
									Op:    qb.OpGt,
									Right: qb.Param{Value: 28},
								},
							},
						},
					},
				}},
			},
			"UPDATE `users` SET `age` = (" + wrap + "SELECT MAX(`users`.`age`) AS `oldest` FROM `users` " +
				"HAVING `oldest` > ?) AS materialized)",
		},
		{
			"a star in EXISTS becomes 1, which names nothing twice",
			&qb.Delete{
				Table: users,
				Where: []qb.Cond{qb.Exists{
					Select: &qb.Select{
						Columns: []qb.Selected{{Value: qb.Star{}}},
						From:    qb.Source{Table: users},
						Joins: []qb.Join{
							{
								Source: qb.Source{Table: posts},
								On:     []qb.Cond{qb.Compare{Left: userID, Op: qb.OpEq, Right: id}},
							},
						},
					},
				}},
			},
			"DELETE FROM `users` WHERE EXISTS (" + wrap + "SELECT 1 FROM `users` JOIN `posts` ON " +
				"`posts`.`user_id` = `users`.`id`) AS materialized)",
		},
		{
			"DISTINCT does not change whether a row exists",
			&qb.Delete{
				Table: users,
				Where: []qb.Cond{qb.Exists{
					Select: &qb.Select{
						Distinct: true,
						Columns:  []qb.Selected{{Value: qb.Star{}}},
						From:     qb.Source{Table: users},
					},
				}},
			},
			"DELETE FROM `users` WHERE EXISTS (" + wrap + "SELECT DISTINCT 1 FROM `users`) AS materialized)",
		},
		{
			"a star that ordering reads stays, on one table",
			&qb.Delete{
				Table: users,
				Where: []qb.Cond{qb.Exists{
					Select: &qb.Select{
						Columns: []qb.Selected{{Value: qb.Star{}}},
						From:    qb.Source{Table: users},
						Order:   []qb.Order{{Value: age}},
						Limit:   new(1),
					},
				}},
			},
			"DELETE FROM `users` WHERE EXISTS (" + wrap +
				"SELECT * FROM `users` ORDER BY `users`.`age` LIMIT ?) AS materialized)",
		},
		{
			"a column named like one the derived table gives keeps its name",
			update(qb.In{
				Left: id,
				Sub: &qb.Select{
					Columns: []qb.Selected{
						{Value: qb.Func{Name: "MAX", Args: []qb.Expr{id}}},
						{Value: qb.Column{Table: users, Name: "ant_c1"}},
					},
					From: qb.Source{Table: users},
				},
			}),
			"UPDATE `users` SET `age` = ? WHERE `users`.`id` IN (" + wrap +
				"SELECT MAX(`users`.`id`), `users`.`ant_c1` FROM `users`) AS materialized(`ant_c1`, `ant_c2`))",
		},
		{
			"columns given one name with AS keep it",
			update(qb.In{
				Left: qb.Row{Values: []qb.Expr{id, id}},
				Sub: &qb.Select{
					Columns: []qb.Selected{
						{Value: id, Alias: "x"},
						{Value: qb.Column{Table: posts, Name: "id"}, Alias: "x"},
					},
					From: qb.Source{Table: users},
					Joins: []qb.Join{
						{
							Source: qb.Source{Table: posts},
							On:     []qb.Cond{qb.Compare{Left: userID, Op: qb.OpEq, Right: id}},
						},
					},
				},
			}),
			"UPDATE `users` SET `age` = ? WHERE (`users`.`id`, `users`.`id`) IN (" + wrap +
				"SELECT `users`.`id` AS `x`, `posts`.`id` AS `x` FROM `users` JOIN `posts` ON " +
				"`posts`.`user_id` = `users`.`id`) AS materialized(`ant_c1`, `ant_c2`))",
		},
		{
			// MySQL reads ORDER BY ant_c1 as the select list's column of that
			// name, so the subquery's select list is left as it was, and what it
			// refers to, in its own terms or in raw SQL, keeps its meaning.
			"what the subquery refers to by name keeps its meaning",
			&qb.Update{
				Table: users,
				Set: []qb.Assignment{{
					Column: "age",
					Value: qb.Subquery{
						Select: &qb.Select{
							Columns: []qb.Selected{
								{Value: qb.Func{Name: "ABS", Args: []qb.Expr{age}}},
							},
							From: qb.Source{Table: users},
							Order: []qb.Order{
								{Value: qb.Column{Name: "ant_c1"}},
								{Value: qb.Fragment{SQL: "ant_c2"}},
							},
						},
					},
				}},
			},
			"UPDATE `users` SET `age` = (" + wrap + "SELECT ABS(`users`.`age`) FROM `users` " +
				"ORDER BY `ant_c1`, ant_c2) AS materialized)",
		},
		{
			"raw expressions in the select list, alike, stay as they are",
			update(qb.In{
				Left: qb.Row{Values: []qb.Expr{id, id}},
				Sub: &qb.Select{
					Columns: []qb.Selected{
						{Value: qb.Fragment{SQL: "MAX(id)"}},
						{Value: qb.Fragment{SQL: "MAX(id)"}},
					},
					From: qb.Source{Table: users},
				},
			}),
			"UPDATE `users` SET `age` = ? WHERE (`users`.`id`, `users`.`id`) IN (" + wrap +
				"SELECT MAX(id), MAX(id) FROM `users`) AS materialized(`ant_c1`, `ant_c2`))",
		},
		{
			"a SELECT reads freely",
			&qb.Select{
				Columns: []qb.Selected{{Value: id}},
				From:    qb.Source{Table: users},
				Where:   []qb.Cond{qb.In{Left: id, Sub: ofUsers}},
			},
			"SELECT `users`.`id` FROM `users` WHERE `users`.`id` IN (SELECT `users`.`id` FROM `users`)",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := mysql.Dialect.Render(tt.stmt)
			if err != nil {
				t.Fatal(err)
			}

			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}

			for _, d := range []dialect.Dialect{mysql.MariaDB, postgres.Dialect} {
				other, _, err := d.Render(tt.stmt)
				if err != nil {
					t.Fatal(err)
				}

				if strings.Contains(other, "materialized") {
					t.Errorf("%s wrapped a subquery: %s", d.Name(), other)
				}
			}
		})
	}
}

// Raw SQL is read for the table as MySQL reads it, in any SQL mode, since the
// renderer cannot know which one the server has: a backslash escapes a quote
// unless NO_BACKSLASH_ESCAPES is set, ANSI_QUOTES makes double quotes a name, and
// MySQL's comments differ from standard SQL's. A name that any mode reads as the
// table wraps the subquery.
func TestSelfReadInRawSQLAsMySQLReadsIt(t *testing.T) {
	users := &qb.TableRef{Name: "users"}
	posts := &qb.TableRef{Name: "posts"}

	cases := []struct {
		name, sql string
		wrapped   bool
	}{
		{
			"an escaped quote ends no literal",
			`posts.kind = 'it\'s' AND user_id IN (SELECT id FROM users)`,
			true,
		},
		{
			"an escaped quote in double quotes ends no literal",
			`posts.kind = "it\"s" AND user_id IN (SELECT id FROM users)`,
			true,
		},
		{
			"a backslash before a quote with NO_BACKSLASH_ESCAPES",
			`posts.kind = 'C:\' AND user_id IN (SELECT id FROM users) AND posts.note <> '\'`,
			true,
		},
		{
			"a doubled quote",
			`posts.kind = 'it''s' AND user_id IN (SELECT id FROM users)`,
			true,
		},
		{
			"a quote in a # comment",
			"posts.kind = 'x' # it's\nAND user_id IN (SELECT id FROM users)",
			true,
		},
		{
			"a quote in a -- comment",
			"posts.kind = 'x' -- it's\nAND user_id IN (SELECT id FROM users)",
			true,
		},
		{
			"-- without a space is two minus signs",
			`posts.id = 1--1 AND user_id IN (SELECT id FROM users)`,
			true,
		},
		{
			"an executable comment runs",
			`/*!80000 user_id IN (SELECT id FROM users) AND */ 1 = 1`,
			true,
		},
		{
			"an executable comment's version may run into its SQL",
			`/*!80000users */ 1 = 1 AND /*!800340users */ 1 = 1`,
			true,
		},
		{
			"a five-digit version runs into the table",
			`user_id IN (SELECT id FROM /*!80000users*/)`,
			true,
		},
		{
			"a six-digit version, as a later MySQL may read, runs into the table",
			`user_id IN (SELECT id FROM /*!800340users*/)`,
			true,
		},
		{
			"double quotes name the table with ANSI_QUOTES",
			`user_id IN (SELECT id FROM "users")`,
			true,
		},
		{
			"the name only in literals, comments, and a longer name",
			"posts.kind = 'users' AND posts.note <> 'it''s users' /* users */ # users\n" +
				"AND -- users\nuser_id IN (SELECT id FROM users_old)",
			false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			stmt := &qb.Update{
				Table: users,
				Set:   []qb.Assignment{{Column: "age", Value: qb.Param{Value: 1}}},
				Where: []qb.Cond{qb.In{
					Left: qb.Column{Table: users, Name: "id"},
					Sub: &qb.Select{
						Columns: []qb.Selected{{Value: qb.Column{Table: posts, Name: "user_id"}}},
						From:    qb.Source{Table: posts},
						Where:   []qb.Cond{qb.Fragment{SQL: tt.sql}},
					},
				}},
			}

			got, _, err := mysql.Dialect.Render(stmt)
			if err != nil {
				t.Fatal(err)
			}

			if wrapped := strings.Contains(got, "AS materialized"); wrapped != tt.wrapped {
				t.Errorf("wrapped %v, want %v: %s", wrapped, tt.wrapped, got)
			}
		})
	}
}

// A Lazy condition in a subquery that is wrapped is built once, though the
// subquery is written twice.
func TestSelfReadBuildsLazyOnce(t *testing.T) {
	users := &qb.TableRef{Name: "users"}
	id := qb.Column{Table: users, Name: "id"}
	builds := 0

	stmt := &qb.Update{
		Table: users,
		Set:   []qb.Assignment{{Column: "age", Value: qb.Param{Value: 1}}},
		Where: []qb.Cond{qb.In{
			Left: id,
			Sub: &qb.Select{
				Columns: []qb.Selected{{Value: id}},
				From:    qb.Source{Table: users},
				Where: []qb.Cond{qb.Lazy{
					Build: func(context.Context) (qb.Cond, error) {
						builds++

						return qb.Compare{
							Left:  id,
							Op:    qb.OpGt,
							Right: qb.Param{Value: builds},
						}, nil
					},
				}},
			},
		}},
	}

	text, args, err := mysql.Dialect.Render(stmt)
	if err != nil {
		t.Fatal(err)
	}

	if builds != 1 {
		t.Errorf("built %d times", builds)
	}

	if !strings.Contains(text, "materialized") || len(args) != 2 || args[1] != 1 {
		t.Errorf("rendered %s with %v", text, args)
	}
}

// A star over a join that cannot become SELECT 1 may name a column twice, which
// a derived table refuses, so the write says so before reaching MySQL.
func TestSelfReadStarOverJoinIsRefused(t *testing.T) {
	users := &qb.TableRef{Name: "users"}
	posts := &qb.TableRef{Name: "posts"}
	id := qb.Column{Table: users, Name: "id"}

	_, _, err := mysql.Dialect.Render(&qb.Delete{
		Table: users,
		Where: []qb.Cond{qb.Exists{
			Select: &qb.Select{
				Columns: []qb.Selected{{Value: qb.Star{}}},
				From:    qb.Source{Table: users},
				Joins: []qb.Join{
					{
						Source: qb.Source{Table: posts},
						On: []qb.Cond{
							qb.Compare{
								Left:  qb.Column{Table: posts, Name: "user_id"},
								Op:    qb.OpEq,
								Right: id,
							},
						},
					},
				},
				Order:  []qb.Order{{Value: id}},
				Offset: new(1),
			},
		}},
	})
	if !errors.Is(err, ant.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

// A star beside another column may name a column twice too, so that write is
// refused as well.
func TestSelfReadStarBesideColumnsIsRefused(t *testing.T) {
	users := &qb.TableRef{Name: "users"}
	id := qb.Column{Table: users, Name: "id"}

	_, _, err := mysql.Dialect.Render(&qb.Delete{
		Table: users,
		Where: []qb.Cond{qb.Exists{
			Select: &qb.Select{
				Columns: []qb.Selected{
					{Value: qb.Star{}},
					{Value: id},
				},
				From: qb.Source{Table: users},
			},
		}},
	})
	if !errors.Is(err, ant.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

// A wrapped subquery is written again over what it wrote first, not after a
// copy of the statement so far, so a write with many of them renders in space
// that grows with their number, not its square.
func TestManySelfReadsRenderInLinearSpace(t *testing.T) {
	users := &qb.TableRef{Name: "users"}
	id := qb.Column{Table: users, Name: "id"}

	allocated := func(n int) uint64 {
		where := make([]qb.Cond, n)

		for i := range where {
			where[i] = qb.In{
				Left: id,
				Sub: &qb.Select{
					Columns: []qb.Selected{{Value: id}},
					From:    qb.Source{Table: users},
				},
			}
		}

		stmt := &qb.Update{
			Table: users,
			Set:   []qb.Assignment{{Column: "age", Value: qb.Param{Value: 1}}},
			Where: where,
		}

		var before, after runtime.MemStats

		runtime.GC()
		runtime.ReadMemStats(&before)

		if _, _, err := mysql.Dialect.Render(stmt); err != nil {
			t.Fatal(err)
		}

		runtime.ReadMemStats(&after)

		return after.TotalAlloc - before.TotalAlloc
	}

	small, large := allocated(500), allocated(2000)
	if large > 8*small {
		t.Errorf("4 times the subqueries took %.1f times the memory", float64(large)/float64(small))
	}
}

// A table read in another schema than the written one's is another table, and
// a read only one side gives a schema for may be the same one, as may a name in
// raw SQL. A read the renderer is sure of refuses a star it cannot wrap, while
// one it is not sure of leaves the subquery as it was, for MySQL to judge.
func TestSelfReadsAndSchemas(t *testing.T) {
	written := &qb.TableRef{Schema: "app", Name: "users"}
	posts := &qb.TableRef{Name: "posts"}
	userID := qb.Column{Table: posts, Name: "user_id"}

	subOf := func(from *qb.TableRef, where ...qb.Cond) *qb.Select {
		return &qb.Select{
			Columns: []qb.Selected{{Value: qb.Column{Table: from, Name: "id"}}},
			From:    qb.Source{Table: from},
			Where:   where,
		}
	}

	update := func(table *qb.TableRef, sub *qb.Select) *qb.Update {
		return &qb.Update{
			Table: table,
			Set:   []qb.Assignment{{Column: "age", Value: qb.Param{Value: 1}}},
			Where: []qb.Cond{qb.In{Left: qb.Column{Table: table, Name: "id"}, Sub: sub}},
		}
	}

	raw := func(sql string) *qb.Select { return subOf(posts, qb.Fragment{SQL: sql}) }

	cases := []struct {
		name    string
		stmt    *qb.Update
		wrapped bool
	}{
		{
			"the same schema",
			update(written, subOf(&qb.TableRef{Schema: "APP", Name: "users"})),
			true,
		},
		{
			"another schema",
			update(written, subOf(&qb.TableRef{Schema: "other", Name: "users"})),
			false,
		},
		{"no schema read", update(written, subOf(&qb.TableRef{Name: "users"})), true},
		{
			"no schema written",
			update(&qb.TableRef{Name: "users"}, subOf(&qb.TableRef{Schema: "app", Name: "users"})),
			true,
		},
		{
			// In raw SQL any spelling of the table counts, whatever schema
			// qualifies it, as raw SQL is not parsed.
			"raw SQL in another schema",
			update(written, raw("user_id IN (SELECT id FROM other.users)")),
			true,
		},
		{
			"raw SQL in a quoted other schema",
			update(written, raw("user_id IN (SELECT id FROM `other` . `users`)")),
			true,
		},
		{
			"raw SQL in the same schema",
			update(written, raw("user_id IN (SELECT id FROM `app`.users)")),
			true,
		},
		{"raw SQL with no schema", update(written, raw("user_id IN (SELECT id FROM users)")), true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := mysql.Dialect.Render(tt.stmt)
			if err != nil {
				t.Fatal(err)
			}

			if wrapped := strings.Contains(got, "AS materialized"); wrapped != tt.wrapped {
				t.Errorf("wrapped %v, want %v: %s", wrapped, tt.wrapped, got)
			}
		})
	}

	t.Run("a star over a join that may read the table stays as it was", func(t *testing.T) {
		for name, from := range map[string]*qb.TableRef{
			"a schema only one side gives": {Name: "users"},
			"raw SQL":                      posts,
		} {
			sub := &qb.Select{
				Columns: []qb.Selected{{Value: qb.Star{}}},
				From:    qb.Source{Table: from},
				Joins: []qb.Join{{
					Source: qb.Source{Table: &qb.TableRef{Name: "tags"}},
					On:     []qb.Cond{qb.Fragment{SQL: "1 = 1"}},
				}},
				// Ordering and an offset read the star's columns, so it stays.
				Order:  []qb.Order{{Value: qb.Column{Table: from, Name: "id"}}},
				Offset: new(1),
			}

			if name == "raw SQL" {
				sub.Where = []qb.Cond{qb.Fragment{SQL: "user_id IN (SELECT id FROM users)"}}
			}

			got, _, err := mysql.Dialect.Render(&qb.Delete{
				Table: written,
				Where: []qb.Cond{qb.Exists{Select: sub, Negate: false}, qb.Compare{
					Left:  userID,
					Op:    qb.OpGt,
					Right: qb.Param{Value: 0},
				}},
			})
			if err != nil {
				t.Errorf("%s: %v, want the write left to MySQL", name, err)
			}

			if strings.Contains(got, "materialized") {
				t.Errorf("%s: wrapped a star it cannot: %s", name, got)
			}
		}
	})
}

// A read MySQL may not need wrapped does not refuse a star the wrapper cannot
// take: one of a table whose name differs only in case, which is another table
// unless lower_case_table_names folds them, and one inside a derived table,
// which MySQL materializes, LIMITed IN's included. The subquery stays as it was.
func TestSelfReadsThatAreNotSureAreNotRefused(t *testing.T) {
	users := &qb.TableRef{Name: "users"}
	posts := &qb.TableRef{Name: "posts"}
	userID := qb.Column{Table: posts, Name: "user_id"}

	// A star over a join, ordered and offset, so the wrapper cannot name it.
	starOverJoin := func(where qb.Cond) *qb.Select {
		return &qb.Select{
			Columns: []qb.Selected{{Value: qb.Star{}}},
			From:    qb.Source{Table: posts},
			Joins: []qb.Join{{
				Source: qb.Source{Table: &qb.TableRef{Name: "tags"}},
				On:     []qb.Cond{qb.Fragment{SQL: "1 = 1"}},
			}},
			Where:  []qb.Cond{where},
			Order:  []qb.Order{{Value: qb.Column{Table: posts, Name: "id"}}},
			Offset: new(1),
		}
	}

	ids := func(from qb.Source) *qb.Select {
		return &qb.Select{Columns: []qb.Selected{{Value: qb.Column{Name: "id"}}}, From: from}
	}

	cases := map[string]qb.Cond{
		"a name that differs in case": qb.In{
			Left: userID,
			Sub:  ids(qb.Source{Table: &qb.TableRef{Name: "Users"}}),
		},
		"a read inside a derived table": qb.In{
			Left: userID,
			Sub:  ids(qb.Source{Sub: ids(qb.Source{Table: users}), Alias: "d"}),
		},
		"a read inside a LIMITed IN": qb.In{
			Left: userID,
			Sub: &qb.Select{
				Columns: []qb.Selected{{Value: qb.Column{Table: users, Name: "id"}}},
				From:    qb.Source{Table: users},
				Limit:   new(5),
			},
		},
	}

	for name, read := range cases {
		t.Run(name, func(t *testing.T) {
			got, _, err := mysql.Dialect.Render(&qb.Delete{
				Table: users,
				Where: []qb.Cond{qb.Exists{Select: starOverJoin(read)}},
			})
			if err != nil {
				t.Fatalf("%v, want the write left to MySQL", err)
			}

			if strings.Contains(got, "materialized") {
				t.Errorf("wrapped a star it cannot: %s", got)
			}
		})
	}

	t.Run("a sure read beside a derived one still refuses", func(t *testing.T) {
		both := qb.And{
			Children: []qb.Cond{
				cases["a read inside a derived table"],
				qb.In{Left: userID, Sub: ids(qb.Source{Table: users})},
			},
		}

		_, _, err := mysql.Dialect.Render(&qb.Delete{
			Table: users,
			Where: []qb.Cond{qb.Exists{Select: starOverJoin(both)}},
		})
		if !errors.Is(err, ant.ErrUnsupported) {
			t.Fatalf("got %v, want ErrUnsupported", err)
		}
	})
}
