package matrix

import (
	"cmp"
	"slices"
	"strings"

	"github.com/skudra-go/ant"
)

// tri is SQL's three-valued truth: a comparison with NULL is unknown, and a row
// is kept only when its condition is true.
type tri int8

const (
	no tri = iota
	yes
	unknown
)

func truth(b bool) tri {
	if b {
		return yes
	}

	return no
}

func and(a, b tri) tri {
	switch {
	case a == no || b == no:
		return no
	case a == yes && b == yes:
		return yes
	}

	return unknown
}

func or(a, b tri) tri {
	switch {
	case a == yes || b == yes:
		return yes
	case a == no && b == no:
		return no
	}

	return unknown
}

func not(a tri) tri {
	switch a {
	case yes:
		return no
	case no:
		return yes
	}

	return unknown
}

// cmpScore compares a nullable score with v: unknown when it is NULL.
func cmpScore(a *Author, f func(int) bool) tri {
	if a.Score == nil {
		return unknown
	}

	return truth(f(*a.Score))
}

func ptr[V any](v V) *V { return &v }

// condition is one point on the condition axis: how to write it in ant, and
// what it means.
type condition struct {
	name string
	cond func(db *MatrixDB) ant.Cond // nil for no condition
	eval func(w *world, a *Author) tri
	// all says the condition cannot leave a row out, as NotIn with no values,
	// so a bulk write counts it as none.
	all bool
}

func atom(name string, cond func() ant.Cond, eval func(a *Author) tri) condition {
	return condition{
		name: name,
		cond: func(*MatrixDB) ant.Cond { return cond() },
		eval: func(_ *world, a *Author) tri { return eval(a) },
	}
}

// conditions covers each comparison, NULL handling, membership, patterns,
// relations, subqueries, and the three connectives over them.
func conditions() []condition {
	age := func(f func(int) bool) func(a *Author) tri {
		return func(a *Author) tri { return truth(f(a.Age)) }
	}

	ageGt18 := atom(
		"age>18",
		func() ant.Cond {
			return ant.Gt(&A.Age, 18)
		},
		age(func(v int) bool {
			return v > 18
		}),
	)

	scoreSet := atom(
		"score set",
		func() ant.Cond {
			return ant.IsNotNull(&A.Score)
		},
		func(a *Author) tri {
			return truth(a.Score != nil)
		},
	)

	score5 := atom(
		"score=5",
		func() ant.Cond {
			return ant.Eq(&A.Score, ptr(5))
		},
		func(a *Author) tri {
			return cmpScore(a, func(v int) bool { return v == 5 })
		},
	)

	ageNeg := atom(
		"age<0",
		func() ant.Cond {
			return ant.Lt(&A.Age, 0)
		},
		age(func(v int) bool {
			return v < 0
		}),
	)

	age30 := atom(
		"age=30",
		func() ant.Cond {
			return ant.Eq(&A.Age, 30)
		},
		age(func(v int) bool {
			return v == 30
		}),
	)

	active := atom(
		"active",
		func() ant.Cond {
			return ant.Eq(&A.Active, true)
		},
		func(a *Author) tri {
			return truth(a.Active)
		},
	)

	hasPost := func(name string, conds func() []ant.Cond, keep func(p *Post) bool, none bool) condition {
		return condition{
			name: name,
			cond: func(*MatrixDB) ant.Cond {
				if none {
					return ant.HasNone(&A.Posts, conds()...)
				}

				return ant.Has(&A.Posts, conds()...)
			},
			eval: func(w *world, a *Author) tri {
				found := slices.ContainsFunc(w.postsOf(a.ID), keep)

				return truth(found != none)
			},
		}
	}

	all := func(*Post) bool { return true }

	return []condition{
		{name: "none"},
		age30,
		atom(
			"age<>30",
			func() ant.Cond {
				return ant.Ne(&A.Age, 30)
			},
			age(func(v int) bool {
				return v != 30
			}),
		),
		ageGt18,
		atom(
			"age>=18",
			func() ant.Cond {
				return ant.Gte(&A.Age, 18)
			},
			age(func(v int) bool {
				return v >= 18
			}),
		),
		ageNeg,
		atom(
			"age<=0",
			func() ant.Cond {
				return ant.Lte(&A.Age, 0)
			},
			age(func(v int) bool {
				return v <= 0
			}),
		),
		atom(
			"age 18..42",
			func() ant.Cond {
				return ant.Between(&A.Age, 18, 42)
			},
			age(func(v int) bool {
				return v >= 18 && v <= 42
			}),
		),
		atom(
			"age in 0,30,65",
			func() ant.Cond {
				return ant.In(&A.Age, 0, 30, 65)
			},
			age(func(v int) bool {
				return v == 0 || v == 30 || v == 65
			}),
		),
		atom(
			"age not in 0,30",
			func() ant.Cond {
				return ant.NotIn(&A.Age, 0, 30)
			},
			age(func(v int) bool {
				return v != 0 && v != 30
			}),
		),
		atom(
			"age in ()",
			func() ant.Cond {
				return ant.In(&A.Age)
			},
			func(*Author) tri {
				return no
			},
		),
		func() condition {
			c := atom(
				"age not in ()",
				func() ant.Cond {
					return ant.NotIn(&A.Age)
				},
				func(*Author) tri {
					return yes
				},
			)
			c.all = true

			return c
		}(),
		atom(
			"score null",
			func() ant.Cond {
				return ant.IsNull(&A.Score)
			},
			func(a *Author) tri {
				return truth(a.Score == nil)
			},
		),
		scoreSet,
		score5,
		atom("score<>5", func() ant.Cond { return ant.Ne(&A.Score, ptr(5)) }, func(a *Author) tri {
			return cmpScore(a, func(v int) bool { return v != 5 })
		}),
		atom(
			"score=nil",
			func() ant.Cond {
				return ant.Eq(&A.Score, (*int)(nil))
			},
			func(a *Author) tri {
				return truth(a.Score == nil)
			},
		),
		atom(
			"score<>nil",
			func() ant.Cond {
				return ant.Ne(&A.Score, (*int)(nil))
			},
			func(a *Author) tri {
				return truth(a.Score != nil)
			},
		),
		active,
		atom(
			"country empty",
			func() ant.Cond {
				return ant.Eq(&A.Country, "")
			},
			func(a *Author) tri {
				return truth(a.Country == "")
			},
		),
		atom(
			"name like ann%",
			func() ant.Cond {
				return ant.Like(&A.Name, "ann%")
			},
			func(a *Author) tri {
				return truth(strings.HasPrefix(a.Name, "ann"))
			},
		),
		atom(
			"name like _o%",
			func() ant.Cond {
				return ant.Like(&A.Name, "_o%")
			},
			func(a *Author) tri {
				return truth(len(a.Name) >= 2 && a.Name[1] == 'o')
			},
		),
		hasPost("has a post", func() []ant.Cond { return nil }, all, false),
		hasPost("has no post", func() []ant.Cond { return nil }, all, true),
		hasPost(
			"has a post over 500 views",
			func() []ant.Cond {
				return []ant.Cond{ant.Gt(&P.Views, 500)}
			},
			func(p *Post) bool { return p.Views > 500 },
			false,
		),
		hasPost(
			"has no rated post",
			func() []ant.Cond {
				return []ant.Cond{ant.IsNotNull(&P.Rating)}
			},
			func(p *Post) bool { return p.Rating != nil },
			true,
		),
		{
			name: "in a published post's authors",
			cond: func(db *MatrixDB) ant.Cond {
				return ant.InSub(
					&A.ID,
					ant.SubOf(db.Posts.Where(ant.Eq(&P.Draft, false)), &P.AuthorID),
				)
			},
			eval: func(w *world, a *Author) tri {
				return truth(slices.ContainsFunc(w.postsOf(a.ID), func(p *Post) bool {
					return !p.Draft
				}))
			},
		},
		{
			name: "not in an unrated post's authors",
			cond: func(db *MatrixDB) ant.Cond {
				return ant.NotInSub(
					&A.ID,
					ant.SubOf(db.Posts.Where(ant.IsNull(&P.Rating)), &P.AuthorID),
				)
			},
			eval: func(w *world, a *Author) tri {
				return truth(!slices.ContainsFunc(w.postsOf(a.ID), func(p *Post) bool {
					return p.Rating == nil
				}))
			},
		},
		{
			name: "exists a post with its age in views",
			cond: func(db *MatrixDB) ant.Cond {
				return ant.Exists(
					db.Posts.Where(ant.EqCol(&P.AuthorID, &A.ID), ant.Gt(&P.Views, 0)),
				)
			},
			eval: func(w *world, a *Author) tri {
				return truth(slices.ContainsFunc(w.postsOf(a.ID), func(p *Post) bool {
					return p.Views > 0
				}))
			},
		},
		compound("age>18 and score set", ant.And, and, ageGt18, scoreSet),
		compound("age<0 or score=5", ant.Or, or, ageNeg, score5),
		negated("not score=5", score5),
		negated("not (age=30 or active)", compound("", ant.Or, or, age30, active)),
		compound(
			"(age<0 or score=5) and not active",
			ant.And,
			and,
			compound("", ant.Or, or, ageNeg, score5),
			negated("", active),
		),
	}
}

func compound(
	name string,
	join func(...ant.Cond) ant.Cond,
	op func(a, b tri) tri,
	left, right condition,
) condition {
	return condition{
		name: name,
		cond: func(db *MatrixDB) ant.Cond { return join(left.cond(db), right.cond(db)) },
		eval: func(w *world, a *Author) tri { return op(left.eval(w, a), right.eval(w, a)) },
	}
}

func negated(name string, c condition) condition {
	return condition{
		name: name,
		cond: func(db *MatrixDB) ant.Cond { return ant.Not(c.cond(db)) },
		eval: func(w *world, a *Author) tri { return not(c.eval(w, a)) },
	}
}

// source is one point on the source axis: what rows a query starts from.
type source struct {
	name  string
	build func(db *MatrixDB) *ant.Query[Author]
	rows  func(w *world) []*Author
	// filtered says whether the source alone can leave rows out, which lets a
	// bulk write without a condition of its own pass the guard.
	filtered bool
}

func where(w *world, keep func(a *Author) bool) []*Author {
	var out []*Author

	for _, a := range w.authors {
		if keep(a) {
			out = append(out, a)
		}
	}

	return out
}

// descendants returns the unmentored authors under 30 and everyone they mentor,
// all the way down.
func descendants(w *world) []*Author {
	var out []*Author

	for _, a := range w.authors {
		if a.MentorID == nil && a.Age < 30 {
			out = append(out, a)
		}
	}

	for i := 0; i < len(out); i++ {
		for _, a := range w.authors {
			if a.MentorID != nil && *a.MentorID == out[i].ID && !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}

	return out
}

func sources() []source {
	adult := func(a *Author) bool { return a.Age >= 18 }
	scored := func(a *Author) bool { return a.Score != nil }

	return []source{
		{
			name:  "table",
			build: func(db *MatrixDB) *ant.Query[Author] { return db.Authors.Query() },
			rows:  func(w *world) []*Author { return w.authors },
		},
		{
			name: "From",
			build: func(db *MatrixDB) *ant.Query[Author] {
				return db.Authors.Query().From(db.Authors.Where(ant.Gte(&A.Age, 18)))
			},
			rows:     func(w *world) []*Author { return where(w, adult) },
			filtered: true,
		},
		{
			name: "FromCTE",
			build: func(db *MatrixDB) *ant.Query[Author] {
				return db.Authors.Query().
					FromCTE("scored", db.Authors.Where(ant.IsNotNull(&A.Score)))
			},
			rows:     func(w *world) []*Author { return where(w, scored) },
			filtered: true,
		},
		{
			name: "Union",
			build: func(db *MatrixDB) *ant.Query[Author] {
				return db.Authors.Where(ant.Lt(&A.Age, 18)).
					Union(db.Authors.Where(ant.Eq(&A.Active, true)))
			},
			rows: func(w *world) []*Author {
				return where(w, func(a *Author) bool { return a.Age < 18 || a.Active })
			},
			filtered: true,
		},
		{
			name: "Intersect",
			build: func(db *MatrixDB) *ant.Query[Author] {
				return db.Authors.Where(ant.Gte(&A.Age, 18)).
					Intersect(db.Authors.Where(ant.IsNotNull(&A.Score)))
			},
			rows: func(w *world) []*Author {
				return where(w, func(a *Author) bool { return adult(a) && scored(a) })
			},
			filtered: true,
		},
		{
			name: "Except",
			build: func(db *MatrixDB) *ant.Query[Author] {
				return db.Authors.Query().Except(db.Authors.Where(ant.Eq(&A.Country, "lv")))
			},
			rows: func(w *world) []*Author {
				return where(w, func(a *Author) bool { return a.Country != "lv" })
			},
		},
		{
			name: "Recursive",
			build: func(db *MatrixDB) *ant.Query[Author] {
				return db.Authors.Where(ant.IsNull(&A.MentorID), ant.Lt(&A.Age, 30)).
					Recursive(&A.Mentees)
			},
			rows:     descendants,
			filtered: true,
		},
		{
			name: "Union of paged sides",
			build: func(db *MatrixDB) *ant.Query[Author] {
				return db.Authors.Query().OrderBy(&A.Name).Limit(3).
					Union(db.Authors.Query().OrderByDesc(&A.Name).Limit(2))
			},
			rows: func(w *world) []*Author {
				byName := slices.SortedFunc(slices.Values(w.authors), func(a, b *Author) int {
					return cmp.Compare(a.Name, b.Name)
				})

				return append(slices.Clone(byName[:3]), byName[len(byName)-2:]...)
			},
		},
	}
}

// order is one point on the ordering axis. less is a total order, ending in ID,
// so that the oracle and the database agree on every position. A nil less
// means no order, and the rows are compared as a set.
type order struct {
	name  string
	apply func(q *ant.Query[Author]) *ant.Query[Author]
	less  func(a, b *Author) int
}

// nullsFirst orders scores with NULL first.
func nullsFirst(a, b *Author, desc bool) int {
	switch {
	case a.Score == nil && b.Score == nil:
		return 0
	case a.Score == nil:
		return -1
	case b.Score == nil:
		return 1
	case desc:
		return cmp.Compare(*b.Score, *a.Score)
	}

	return cmp.Compare(*a.Score, *b.Score)
}

func orders() []order {
	byID := func(a, b *Author) int { return cmp.Compare(a.ID, b.ID) }

	return []order{
		{name: "unordered", apply: func(q *ant.Query[Author]) *ant.Query[Author] { return q }},
		{
			name: "age, id",
			apply: func(q *ant.Query[Author]) *ant.Query[Author] {
				return q.OrderBy(&A.Age).ThenBy(&A.ID)
			},
			less: func(a, b *Author) int { return cmp.Or(cmp.Compare(a.Age, b.Age), byID(a, b)) },
		},
		{
			name: "age desc, id",
			apply: func(q *ant.Query[Author]) *ant.Query[Author] {
				return q.OrderByDesc(&A.Age).ThenBy(&A.ID)
			},
			less: func(a, b *Author) int { return cmp.Or(cmp.Compare(b.Age, a.Age), byID(a, b)) },
		},
		{
			name: "score nulls first, id",
			apply: func(q *ant.Query[Author]) *ant.Query[Author] {
				return q.Order(ant.Col(&A.Score).Asc().NullsFirst(), ant.Col(&A.ID).Asc())
			},
			less: func(a, b *Author) int { return cmp.Or(nullsFirst(a, b, false), byID(a, b)) },
		},
		{
			name: "score desc nulls last, id",
			apply: func(q *ant.Query[Author]) *ant.Query[Author] {
				return q.Order(ant.Col(&A.Score).Desc().NullsLast(), ant.Col(&A.ID).Asc())
			},
			less: func(a, b *Author) int { return cmp.Or(-nullsFirst(a, b, false), byID(a, b)) },
		},
		{
			name:  "name",
			apply: func(q *ant.Query[Author]) *ant.Query[Author] { return q.OrderBy(&A.Name) },
			less:  func(a, b *Author) int { return cmp.Compare(a.Name, b.Name) },
		},
	}
}

// paging is one point on the paging axis.
type paging struct {
	name          string
	limit, offset int // -1 when not set
}

func pagings() []paging {
	return []paging{
		{"all", -1, -1},
		{"limit 5", 5, -1},
		{"offset 3", -1, 3},
		{"limit 4 offset 2", 4, 2},
	}
}

func (p paging) apply(q *ant.Query[Author]) *ant.Query[Author] {
	if p.limit >= 0 {
		q = q.Limit(p.limit)
	}

	if p.offset >= 0 {
		q = q.Offset(p.offset)
	}

	return q
}

// page cuts rows as the paging would, once they are in order.
func (p paging) page(rows []*Author) []*Author {
	if p.offset > 0 {
		rows = rows[min(p.offset, len(rows)):]
	}

	if p.limit >= 0 {
		rows = rows[:min(p.limit, len(rows))]
	}

	return rows
}
