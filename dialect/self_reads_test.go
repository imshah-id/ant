package dialect_test

import (
	"context"
	"errors"
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
			"columns of one name get names of their own",
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
				"SELECT `users`.`id` AS `ant_c1`, `posts`.`id` AS `ant_c2` FROM `users` JOIN `posts` ON " +
				"`posts`.`user_id` = `users`.`id`) AS materialized)",
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
			"a generated name skips one the query has",
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
				"SELECT MAX(`users`.`id`) AS `ant_c2`, `users`.`ant_c1` FROM `users`) AS materialized)",
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
