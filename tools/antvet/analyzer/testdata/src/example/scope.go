package example

import (
	"context"

	"github.com/skudra-go/ant"

	"models"
)

// The functions that run a query judge it where they run it, and the field they
// read of it.
func aggregates(ctx context.Context, users *ant.Table[User], posts *ant.Table[Post]) {
	ant.SumOf(ctx, posts.Where(ant.Gt(&P.UserID, 1)), &P.UserID)
	ant.SumOf(ctx, users.Where(ant.Exists(posts.Where(ant.Gt(&U.Age, 18)))), &U.Age)

	ant.SumOf(ctx, posts.Where(ant.Gt(&U.Age, 18)), &P.UserID)                                 // want `condition on User in a query of Post`
	ant.SumOf[Post, int64](ctx, posts.Where(ant.Gt(&U.Age, 18)), &P.UserID)                    // want `condition on User in a query of Post`
	ant.Pluck(ctx, posts.Where(ant.Gt(&U.Age, 18)), &P.ID)                                     // want `condition on User in a query of Post`
	ant.SumOf(ctx, users.Where(ant.Exists(posts.Where(ant.Eq(&models.Co.Code, "x")))), &U.Age) // want `condition on Country in a query of Post`
	ant.SumOf(ctx, posts.Where(), &U.Age)                                                      // want `ant.SumOf on a field of User in a query of Post`

	post := &Post{}
	ant.Pluck(ctx, posts.Where(), &post.ID) // want `ant.Pluck needs a field of a probe`
}

// What a projection or a merge is built from runs with it, and is a subquery
// where the projection is one.
func wrapped(ctx context.Context, users *ant.Table[User], posts *ant.Table[Post]) {
	ant.Project[Post](posts.Where(ant.Gt(&P.UserID, 1))).All(ctx)
	users.Where(ant.Exists(ant.Project[Post](posts.Where(ant.Gt(&U.Age, 18))))).All(ctx)
	ant.Project[Post](posts.Where(ant.Gt(&P.UserID, 1)).GroupBy(&P.UserID)).All(ctx)
	users.Where(ant.Exists(ant.Project[Post](posts.Where(ant.Gt(&U.Age, 18)).GroupBy(&P.UserID)))).All(ctx)
	ant.Merge(users, posts.Where(ant.Gt(&P.UserID, 1)), ant.EqCol(&U.ID, &P.UserID)).
		WhenMatched(ant.Set(&U.Age, 1)).
		Exec(ctx)

	ant.Project[Post](posts.Where(ant.Gt(&U.Age, 18))).All(ctx)                                                             // want `condition on User in a query of Post`
	ant.Project[Post](posts.Where(ant.Gt(&U.Age, 18)).GroupBy(&P.UserID)).All(ctx)                                          // want `condition on User in a query of Post`
	ant.Project[Post](posts.Where(ant.Gt(&U.Age, 18)).GroupBy(&P.UserID).Having()).All(ctx)                                 // want `condition on User in a query of Post`
	ant.Project[Post](posts.Where().GroupBy(&U.Age)).All(ctx)                                                               // want `GroupBy on a field of User in a query of Post`
	ant.Merge(users, posts.Where(ant.Gt(&U.Age, 1)), ant.EqCol(&U.ID, &P.UserID)).WhenMatched(ant.Set(&U.Age, 1)).Exec(ctx) // want `condition on User in a query of Post`

	user := &User{}
	ant.Project[Post](posts.Where(), ant.Into(&user.Name, &U.Name)).All(ctx) // want `ant.Into needs a field of a probe`
}

// A join's first query may name the model it joins, and a lateral join's
// subquery the query it joins, while another join's subquery stands alone. A
// join reads both its models, and is an outer query to its subqueries.
func joined(
	ctx context.Context,
	users *ant.Table[User],
	posts *ant.Table[Post],
	countries *ant.Table[models.Country],
) {
	title := func(p *Post, u *User) string { return "" }
	byUser := func(u *User, p *Post) string { return "" }
	four := func(p *Post, u *User, d *Doc, c *models.Country) string { return "" }

	posts.Where().LeftJoin(&P.User).JoinOn[Doc]().JoinOn[models.Country]().
		Where(ant.Eq(&D.Slug, "x"), ant.Eq(&models.Co.Code, "x")).
		Select(four).
		All(ctx)

	ant.JoinSub(posts.Where(ant.Eq(&U.Name, "x")), users.Where(), ant.EqCol(&P.UserID, &U.ID)).
		Select(title).
		All(ctx)
	ant.JoinLateral(users.Where(), posts.Where(ant.Gt(&U.Age, 18)), ant.EqCol(&P.UserID, &U.ID)).
		Select(byUser).
		All(ctx)
	posts.Where().LeftJoin(&P.User).Where(ant.Gt(&U.Age, 18), ant.Eq(&P.ID, "x")).Select(title).All(ctx)
	posts.Where().LeftJoin(&P.User).
		Where(ant.Exists(countries.Where(ant.Gt(&U.Age, 1)))).
		Select(title).
		All(ctx)

	// JoinUsing reads only the column's name, which the joined model has too,
	// and a subquery in a join's conditions may name either side.
	posts.Where().JoinUsing[User](&U.ID).Select(title).All(ctx)
	ant.JoinLateral(users.Where(), posts.Where(), ant.Exists(countries.Where(ant.Gt(&P.UserID, 1)))).
		Select(byUser).
		All(ctx)
	ant.JoinSub(posts.Where(), users.Where(), ant.Exists(countries.Where(ant.Gt(&U.Age, 1)))).
		Select(title).
		All(ctx)

	ant.JoinSub(posts.Where(), users.Where(ant.Eq(&P.ID, "x")), ant.EqCol(&P.UserID, &U.ID)).Select(title).All(ctx)                // want `condition on Post in a query of User`
	posts.Where().LeftJoin(&P.User).JoinOn[Doc]().JoinOn[models.Country]().Where(ant.Eq(&Pa.A, int64(1))).Select(four).All(ctx)    // want `condition on Pair in a query of Post and User and Doc and Country`
	ant.JoinLateral(users.Where(), posts.Where(ant.Eq(&models.Co.Code, "x")), ant.EqCol(&P.UserID, &U.ID)).Select(byUser).All(ctx) // want `condition on Country in a query of Post`
	posts.Where().LeftJoin(&P.User).Where(ant.Eq(&models.Co.Code, "x")).Select(title).All(ctx)                                     // want `condition on Country in a query of Post and User`
	posts.Where().LeftJoin(&P.User).OrderBy(&models.Co.Code).Select(title).All(ctx)                                                // want `OrderBy on a field of Country in a query of Post and User`
	posts.Where().LeftJoin(&P.User).
		Where(ant.Exists(countries.Where(ant.Eq(&D.Slug, "x")))). // want `condition on Doc in a query of Country`
		Select(title).
		All(ctx)
}

// Methods given their type arguments are followed past them, and the fields
// that ordering, grouping, columns, includes, and updates take are judged too:
// ordering and grouping as conditions are, the rest by the query's own model.
func methods(ctx context.Context, users *ant.Table[User], posts *ant.Table[Post]) {
	posts.Where(ant.Gt(&P.UserID, 1)).OrderBy[string](&P.ID).All(ctx)
	users.Where(ant.Exists(posts.Where(ant.Gt(&U.Age, 18)).OrderBy[string](&P.ID))).All(ctx)
	users.Where(ant.Exists(posts.Where().OrderBy(&U.Name))).All(ctx)
	users.Where(ant.Exists(posts.Where().GroupBy(&U.Name))).All(ctx)
	// A subquery inside a shape may name the shape's query, and When hands its
	// function the same query, whose scope it keeps.
	users.Where().Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
		return q.Where(ant.Exists(users.Where(ant.Gt(&P.UserID, 1))))
	}).All(ctx)
	users.Where(ant.Exists(posts.Where().When(useCode, func(q *ant.Query[Post]) *ant.Query[Post] {
		return q.Where(ant.Gt(&U.Age, 18))
	}))).All(ctx)

	// A query built apart inside a shape may end up inside the shape's query,
	// so it is not judged by the query the shape is given to.
	users.Where().Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
		inner := users.Where(ant.Gt(&P.UserID, 1))

		return q.Where(ant.Exists(inner))
	}).All(ctx)

	posts.Where(ant.Gt(&U.Age, 18)).OrderBy[string](&P.ID).All(ctx)          // want `condition on User in a query of Post`
	posts.Where().OrderBy[int](&U.Age).All(ctx)                              // want `OrderBy on a field of User in a query of Post`
	posts.OrderBy(&U.Name).All(ctx)                                          // want `OrderBy on a field of User in a query of Post`
	users.Where(ant.Exists(posts.Where().GroupBy(&models.Co.Code))).All(ctx) // want `GroupBy on a field of Country in a query of Post`
	posts.Where().Column(&U.Name).All(ctx)                                   // want `Column on a field of User in a query of Post`
	users.Where(ant.Exists(posts.Where().Column(&U.Name))).All(ctx)          // want `Column on a field of User in a query of Post`
	posts.Where().With(&U.Age, ant.Col(&U.Age)).All(ctx)                     // want `With on a field of User in a query of Post`
	posts.Where().Include(&U.Posts).All(ctx)                                 // want `Include on a field of User in a query of Post`
	posts.Where().Update(ctx, ant.Set(&P.UserID, 1), ant.Set(&U.Age, 1))     // want `ant.Set on a field of User in a query of Post`
	posts.Where(ant.Gt(&U.Age, 1)).Update(ctx, ant.Set(&P.UserID, 1))        // want `condition on User in a query of Post`
	users.Where().Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
		return q.Where(ant.Eq(&models.Co.Code, "x")) // want `condition on Country in a query of Post`
	}).All(ctx)

	// An include's shape is a query of the related model alone, which ant
	// runs apart, so it cannot name the query it is given to.
	users.Where().Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
		return q.Where(ant.Gt(&U.Age, 18)) // want `condition on User in a query of Post`
	}).All(ctx)
	posts.Where().IncludeOne(&P.User, func(q *ant.Query[User]) *ant.Query[User] {
		return q.Where(ant.Gt(&P.UserID, 1)) // want `condition on Post in a query of User`
	}).All(ctx)
	posts.Where().When(useCode, func(q *ant.Query[Post]) *ant.Query[Post] {
		return q.Where(ant.Gt(&U.Age, 18)) // want `condition on User in a query of Post`
	}).All(ctx)
	users.Where().Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
		if useCode {
			return q.Where(ant.Eq(&models.Co.Code, "x")) // want `condition on Country in a query of Post`
		}

		users.Where(ant.Gt(&P.UserID, 1)).All(ctx) // want `condition on Post in a query of User`

		return q
	}).All(ctx)

	user := &User{}
	posts.Where().JoinUsing[User](&user.ID)     // want `JoinUsing needs a field of a probe`
	posts.Where().OrderBy(&user.Age).All(ctx)   // want `OrderBy needs a field of a probe`
	users.Where().Include(&user.Posts).All(ctx) // want `Include needs a field of a probe`
}

// Conditions that take several fields judge each of them.
func several(ctx context.Context, users *ant.Table[User], posts *ant.Table[Post]) {
	posts.Where(ant.AnyOf("x", &P.ID)).All(ctx)
	users.Where(ant.Exists(posts.Where(ant.AnyOf("x", &P.ID, &U.Name)))).All(ctx)

	posts.Where(ant.AnyOf("x", &P.ID, &U.Name)).All(ctx)                  // want `condition on User in a query of Post`
	posts.Where(ant.Not(ant.NoneOf(int64(1), &P.UserID, &U.ID))).All(ctx) // want `condition on User in a query of Post`

	user := &User{}
	ant.AnyOf("x", &P.ID, &user.Name) // want `ant.AnyOf needs a field of a probe`
	ant.EqCol(&U.ID, &user.ID)        // want `ant.EqCol needs a field of a probe`
	ant.Has(&user.Posts)              // want `ant.Has needs a field of a probe`
}

// The functions of ant that take an entity, not a field, take any pointer.
func entities() {
	user := User{}
	ant.Add(nil, &user)
	ant.Add(nil, &User{Name: "x"})
}

// A method expression takes its receiver first, and the method's own arguments
// after it, which are judged as in a call on the receiver.
func expressions(
	ctx context.Context,
	users *ant.Table[User],
	posts *ant.Table[Post],
	countries *ant.Table[models.Country],
) {
	q := posts.Where()
	(*ant.Query[Post]).OrderBy[string](q, &P.ID).All(ctx)
	(*ant.Query[Post]).Where(q, ant.Gt(&P.UserID, 1)).All(ctx)
	(*ant.Table[models.Country]).Find(countries, ctx, "LV")

	(*ant.Query[Post]).OrderBy[int](q, &U.Age).All(ctx)     // want `OrderBy on a field of User in a query of Post`
	(*ant.Query[Post]).Where(q, ant.Gt(&U.Age, 1)).All(ctx) // want `condition on User in a query of Post`
	(*ant.Table[models.Country]).Find(countries, ctx, 1)    // want `Find key is int, and Country has a key of type string`

	// A query written as a method expression's receiver runs along its chain.
	(*ant.Query[Post]).All(posts.Where(ant.Gt(&P.UserID, 1)), ctx)
	users.Where(ant.Exists((*ant.Query[Post]).Where(posts.Where(), ant.Gt(&U.Age, 18)))).All(ctx)
	(*ant.Query[Post]).All(posts.Where(ant.Gt(&U.Age, 18)), ctx)                             // want `condition on User in a query of Post`
	(*ant.Query[Post]).All((*ant.Query[Post]).Where(posts.Where(), ant.Gt(&U.Age, 18)), ctx) // want `condition on User in a query of Post`

	user := &User{}
	(*ant.Query[Post]).OrderBy[int](q, &user.Age).All(ctx) // want `OrderBy needs a field of a probe`
}

// UserQuery, CountryRepo, and Repo wrap ant's builders by embedding them, and
// the methods they get from those are judged as the builders' own.
type (
	UserQuery   struct{ *ant.Query[User] }
	CountryRepo struct{ *ant.Table[models.Country] }
	Repo[T any] struct{ *ant.Table[T] }
)

func wrappers(
	ctx context.Context,
	uq UserQuery,
	countries CountryRepo,
	repo Repo[User],
	posts *ant.Table[Post],
) {
	uq.Where(ant.Gt(&U.Age, 1)).All(ctx)
	uq.Where(ant.Exists(posts.Where(ant.Gt(&U.Age, 1)))).All(ctx)
	countries.Find(ctx, "LV")
	repo.Where(ant.Gt(&U.Age, 1)).All(ctx)

	uq.Where(ant.Gt(&P.UserID, 1)).All(ctx)                                  // want `condition on Post in a query of User`
	repo.Where(ant.Gt(&P.UserID, 1)).All(ctx)                                // want `condition on Post in a query of User`
	countries.Find(ctx, 1)                                                   // want `Find key is int, and Country has a key of type string`
	uq.Where(ant.Exists(posts.Where(ant.Eq(&models.Co.Code, "x")))).All(ctx) // want `condition on Country in a query of Post`

	user := &User{}
	uq.OrderBy(&user.Age).All(ctx) // want `OrderBy needs a field of a probe`
}

// A wrapper of a model a type parameter stands for is judged once instantiated.
func openWrapper[T any](ctx context.Context, repo Repo[T]) {
	repo.Where(ant.Gt(&P.UserID, 1)).All(ctx)
}
