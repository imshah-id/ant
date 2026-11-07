package example

import (
	"context"
	"time"

	"github.com/skudra-go/ant"

	"models"
)

type User struct {
	ID    int64
	Age   int
	Name  string
	At    time.Time
	Posts []Post
}

type Post struct {
	ID     string
	UserID int64
	User   *User
}

// Member is User under another name, as a test package might alias a fixture.
type Member = User

// Ignored's Configure takes an argument ant never gives, so ant never calls it,
// and its key stays ID.
type Ignored struct {
	ID   int64
	Code string
}

func (Ignored) Configure(b *ant.ModelBuilder[Ignored], also int) { b.Key(&Ig.Code) }

// Keyed is embedded in Embeds, which gets its Configure, and whatever key that
// sets, which the analyzer does not follow.
type Keyed struct{} // want Keyed:"key Code"

func (Keyed) Configure(b *ant.ModelBuilder[Embeds]) { b.Key(&Em.Code) }

type Embeds struct {
	Keyed
	ID   int64
	Code string
}

// CountryAlias and DocAlias stand for models whose keys Configure sets, in
// another package and in this one.
type (
	CountryAlias = models.Country
	DocAlias     = Doc
)

// Doc's key is a slug filled in Go.
type Doc struct { // want Doc:"key Slug"
	ID   int64
	Slug string
}

func (Doc) Configure(b *ant.ModelBuilder[Doc]) {
	ant.GenerateKey(b, &D.Slug, func() string { return "x" })
}

// Pair has a composite key, which Find cannot take.
type Pair struct { // want Pair:"key "
	ID int64
	A  int64
	B  int64
}

func (*Pair) Configure(b *ant.ModelBuilder[Pair]) {
	b.CompositeKey(ant.F(&Pa.A), ant.F(&Pa.B))
}

// useCode chooses the key at run time, so the analyzer cannot know it.
var useCode bool

// Branchy sets its key in a branch.
type Branchy struct { // want Branchy:"key "
	ID   int64
	Code string
}

func (Branchy) Configure(b *ant.ModelBuilder[Branchy]) {
	if useCode {
		b.Key(&Br.Code)
	} else {
		b.Key(&Br.ID)
	}
}

// Helped has its key set by a function given the builder.
type Helped struct { // want Helped:"key "
	ID   int64
	Code string
}

func (Helped) Configure(b *ant.ModelBuilder[Helped]) { keyOnCode(b) }

func keyOnCode(b *ant.ModelBuilder[Helped]) { b.Key(&He.Code) }

// Ident is embedded in Coded, with a Code of its own.
type Ident struct{ Code int64 }

// Coded's key is Identity.Code, an integer, not its own Code, a string.
type Coded struct { // want Coded:"key Identity.Code"
	ID       int64
	Code     string
	Identity Ident
}

func (Coded) Configure(b *ant.ModelBuilder[Coded]) { b.Key(&Cd.Identity.Code) }

// Fluent sets its key in a chain, which runs every time.
type Fluent struct { // want Fluent:"key Code"
	ID   int64
	Code string
}

func (Fluent) Configure(b *ant.ModelBuilder[Fluent]) { b.Key(&Fl.Code).NotGenerated() }

// Early may return before it sets its key.
type Early struct { // want Early:"key "
	ID   int64
	Code string
}

func (Early) Configure(b *ant.ModelBuilder[Early]) {
	if useCode {
		return
	}

	b.Key(&Ea.Code)
}

// Jumpy may jump past its key.
type Jumpy struct { // want Jumpy:"key "
	ID   int64
	Code string
}

func (Jumpy) Configure(b *ant.ModelBuilder[Jumpy]) {
	if useCode {
		goto done
	}

	b.Key(&Ju.Code)

done:
	b.Table("jumpy")
}

// Expressed sets its key through a method expression, which takes the builder
// first.
type Expressed struct { // want Expressed:"key Code"
	ID   int64
	Code string
}

func (Expressed) Configure(b *ant.ModelBuilder[Expressed]) {
	(*ant.ModelBuilder[Expressed]).Key(b, &Ex.Code)
}

// Aliased hands its builder on under another name.
type Aliased struct { // want Aliased:"key "
	ID   int64
	Code string
}

func (Aliased) Configure(b *ant.ModelBuilder[Aliased]) {
	alias := b
	keyOnAliased(alias)
}

func keyOnAliased(b *ant.ModelBuilder[Aliased]) { b.Key(&Al.Code) }

// Captured takes its key setter as a method value, which could run anywhere.
type Captured struct { // want Captured:"key "
	ID   int64
	Code string
}

func (Captured) Configure(b *ant.ModelBuilder[Captured]) {
	setKey := b.Key[string]
	setKey(&Ca.Code)
}

// Row is generic, and Key an alias, so Row[Key] is Row[int64].
type Row[K any] struct {
	ID   K
	Name string
}

type Key = int64

var (
	Ca = ant.Fields[Captured]() // want Ca:"probe of example.Captured"
	Ro = ant.Fields[Row[Key]]() // want Ro:"probe of example.Row\\[int64\\]"
)

var (
	Fl = ant.Fields[Fluent]()  // want Fl:"probe of example.Fluent"
	Ea = ant.Fields[Early]()   // want Ea:"probe of example.Early"
	Al = ant.Fields[Aliased]() // want Al:"probe of example.Aliased"
)

var (
	Br = ant.Fields[Branchy]() // want Br:"probe of example.Branchy"
	He = ant.Fields[Helped]()  // want He:"probe of example.Helped"
	Cd = ant.Fields[Coded]()   // want Cd:"probe of example.Coded"
)

var (
	U     = ant.Fields[User]()      // want U:"probe of example.User"
	P     = ant.Fields[Post]()      // want P:"probe of example.Post"
	Alias = U                       // want Alias:"probe of example.User"
	M     = ant.Fields[Member]()    // want M:"probe of example.User"
	D     = ant.Fields[Doc]()       // want D:"probe of example.Doc"
	Pa    = ant.Fields[Pair]()      // want Pa:"probe of example.Pair"
	Ig    = ant.Fields[Ignored]()   // want Ig:"probe of example.Ignored"
	Ju    = ant.Fields[Jumpy]()     // want Ju:"probe of example.Jumpy"
	Ex    = ant.Fields[Expressed]() // want Ex:"probe of example.Expressed"
	Em    = ant.Fields[Embeds]()    // want Em:"probe of example.Embeds"
)

func good(
	ctx context.Context,
	users *ant.Table[User],
	members *ant.Table[Member],
	posts *ant.Table[Post],
	countries *ant.Table[models.Country],
	docs *ant.Table[Doc],
	pairs *ant.Table[Pair],
) {
	users.Where(ant.Eq(&U.Age, 18), ant.Gt(&Alias.Age, 1)).All(ctx)
	users.Where(ant.EqCol(&U.ID, &P.UserID))          // two columns may refer to an outer query
	posts.Where(ant.Eq(&U.Name, "x")).Join().All(ctx) // a condition can come before its join
	local := ant.Fields[User]()
	users.Where(ant.Gt(&local.Age, 1)).All(ctx)
	users.Find(ctx, int64(1))
	users.Find(ctx, 1)
	posts.Find(ctx, "abc")

	// An alias names the same model as what it stands for.
	members.Where(ant.Eq(&U.Name, "x")).All(ctx)
	users.Where(ant.Eq(&M.Name, "x")).Count(ctx)
	members.Find(ctx, int64(1))

	// A subquery may name its outer query's model, inline or built apart.
	users.Where(ant.Exists(posts.Where(ant.Gt(&U.Age, 18)))).All(ctx)
	inner := posts.Where(ant.Gt(&U.Age, 18))
	users.Where(ant.Exists(inner)).All(ctx)
	_ = posts.Where(ant.Gt(&U.Age, 18)).OrderBy(&P.ID)

	// Keys that Configure sets, in this package and another.
	countries.Find(ctx, "LV")
	docs.Find(ctx, "a-slug")
	pairs.Find(ctx, "anything")

	// GroupBy builds on, so a grouped subquery is still a subquery.
	users.Where(ant.Exists(posts.Where(ant.Gt(&U.Age, 18)).GroupBy(&P.UserID))).All(ctx)

	// A grouped query's model is unknown here, so what sits inside it is not
	// judged.
	users.Where(ant.Exists(posts.Where().GroupBy(&P.UserID).Having(
		ant.Exists(countries.Where(ant.Gt(&P.UserID, int64(0))))))).All(ctx)

	// A query built apart may end up inside another, so it is not judged.
	middle := users.Where(ant.Exists(posts.Where(ant.Eq(&models.Co.Code, "LV"))))
	countries.Where(ant.Exists(middle)).All(ctx)
}

// A key the analyzer cannot be sure of is not checked, and a nested one is
// followed to its own field.
func more(
	ctx context.Context,
	users *ant.Table[User],
	posts *ant.Table[Post],
	countries *ant.Table[models.Country],
	captured *ant.Table[Captured],
	rows *ant.Table[Row[int64]],
) {
	captured.Find(ctx, "LV")
	rows.Where(ant.Eq(&Ro.Name, "x")).All(ctx)

	// Has opens a scope of the related model, which is not followed.
	users.Where(ant.Has(&U.Posts, ant.Exists(countries.Where(ant.Eq(&P.ID, "x"))))).All(ctx)

	(posts.Where(ant.Gt(&U.Age, 18))).All(ctx) // want `condition on User in a query of Post`
}

func keys(
	ctx context.Context,
	branchy *ant.Table[Branchy],
	helped *ant.Table[Helped],
	coded *ant.Table[Coded],
	fluent *ant.Table[Fluent],
	early *ant.Table[Early],
	aliased *ant.Table[Aliased],
	countryAliases *ant.Table[CountryAlias],
	docAliases *ant.Table[DocAlias],
	ignored *ant.Table[Ignored],
	embeds *ant.Table[Embeds],
	jumpy *ant.Table[Jumpy],
	expressed *ant.Table[Expressed],
) {
	fluent.Find(ctx, "LV")
	fluent.Find(ctx, 1) // want `Find key is int, and Fluent has a key of type string`
	early.Find(ctx, 1)
	early.Find(ctx, "LV")
	aliased.Find(ctx, "LV")

	branchy.Find(ctx, "LV")
	branchy.Find(ctx, 1)
	helped.Find(ctx, "LV")
	coded.Find(ctx, int64(7))
	coded.Find(ctx, "7") // want `Find key is string, and Coded has a key of type int64`

	// An alias has the key of the model it stands for.
	countryAliases.Find(ctx, "LV")
	docAliases.Find(ctx, "a-slug")
	countryAliases.Find(ctx, 1) // want `Find key is int, and Country has a key of type string`
	docAliases.Find(ctx, 1)     // want `Find key is int, and Doc has a key of type string`

	// A Configure ant never calls sets no key, and one a model embeds may set
	// any.
	ignored.Find(ctx, 1)
	ignored.Find(ctx, "LV") // want `Find key is string, and Ignored has a key of type int64`
	embeds.Find(ctx, 1)
	embeds.Find(ctx, "LV")

	// ant checks the key by Find's type argument, which the key converts to.
	countryAliases.Find[models.Code](ctx, "LV")
	countryAliases.Find[any](ctx, "LV") // want `Find key is (any|interface\{\}), and Country has a key of type string`
	docAliases.Find[any](ctx, "a-slug") // want `Find key is (any|interface\{\}), and Doc has a key of type string`

	// A key set past a goto may not be set, and one set by a method expression
	// is read from the method's own argument.
	jumpy.Find(ctx, 1)
	jumpy.Find(ctx, "LV")
	expressed.Find(ctx, "LV")
	expressed.Find(ctx, 1) // want `Find key is int, and Expressed has a key of type string`
}

func bad(
	ctx context.Context,
	users *ant.Table[User],
	members *ant.Table[Member],
	posts *ant.Table[Post],
	countries *ant.Table[models.Country],
	docs *ant.Table[Doc],
) {
	x := 5
	ant.Gt(&x, 3) // want `ant.Gt needs a field of a probe`
	user := &User{}
	ant.Eq(&user.Age, 18) // want `not of an ordinary variable`

	posts.Where(ant.Gt(&U.Age, 18)).All(ctx)                                    // want `condition on User in a query of Post`
	posts.Where(ant.Gt(&U.Age, 18)).Where(ant.Eq(&P.ID, "x")).All(ctx)          // want `condition on User in a query of Post`
	posts.Where(ant.Gt(&U.Age, 18)).OrderBy(&P.ID).Count(ctx)                   // want `condition on User in a query of Post`
	users.Where(ant.Exists(posts.Where(ant.Eq(&models.Co.Code, "x")))).All(ctx) // want `condition on Country in a query of Post`
	users.Find(ctx, "1")                                                        // want `Find key is string, and User has a key of type int64`
	posts.Find(ctx, 1)                                                          // want `Find key is int, and Post has a key of type string`
	members.Find(ctx, "1")                                                      // want `Find key is string, and User has a key of type int64`
	countries.Find(ctx, 1)                                                      // want `Find key is int, and Country has a key of type string`
	docs.Find(ctx, 7)                                                           // want `Find key is int, and Doc has a key of type string`
}

// Code that is wrong on purpose, such as a test of the error, says so.
func onPurpose(ctx context.Context, users *ant.Table[User]) {
	x := 5
	ant.Gt(&x, 3) //antvet:ignore the error is what this tests

	//antvet:ignore so is this one
	users.Find(ctx, "1")
}

// Conditions that And, Or, and Not combine are on the query they are given to,
// and so are those of Past and InSub. A joined include brings in no model the
// conditions can name.
func scopes(ctx context.Context, users *ant.Table[User], posts *ant.Table[Post]) {
	posts.Where(ant.Or(ant.Gt(&P.UserID, 1), ant.Eq(&P.ID, "x"))).All(ctx)
	users.Where(ant.Or(ant.Exists(posts.Where(ant.Gt(&U.Age, 18))))).All(ctx)
	users.Where(ant.Exists(posts.Where(ant.Not(ant.Gt(&U.Age, 18))))).All(ctx)
	users.Where(ant.Or(ant.Has(&U.Posts, ant.Eq(&P.ID, "x")))).All(ctx)
	users.Where(ant.Past(&U.At), ant.InSub(&U.ID, ant.SubOf(posts.Where(), &P.UserID))).All(ctx)

	posts.Where(ant.Or(ant.Gt(&U.Age, 18), ant.Eq(&P.ID, "x"))).All(ctx)    // want `condition on User in a query of Post`
	posts.Where(ant.Not(ant.And(ant.Gt(&U.Age, 18)))).Count(ctx)            // want `condition on User in a query of Post`
	posts.Where(ant.Gt(&U.Age, 18)).IncludeJoined(&P.User).All(ctx)         // want `condition on User in a query of Post`
	posts.Where(ant.Past(&U.At)).All(ctx)                                   // want `condition on User in a query of Post`
	posts.Where(ant.InSub(&U.ID, ant.SubOf(users.Where(), &U.ID))).All(ctx) // want `condition on User in a query of Post`
	user := &User{}
	ant.Past(&user.At) // want `not of an ordinary variable`
}

// early copies Late, which init sets further down.
func early(ctx context.Context, users *ant.Table[User]) {
	local := Late
	users.Where(ant.Gt(&local.Age, 1)).All(ctx)
}

// Late is a probe set in init.
var Late *User // want Late:"probe of example.User"

func init() { Late = ant.Fields[User]() }

// Probes held in variables of a function, however they are declared, are
// probes.
func held(ctx context.Context, users *ant.Table[User]) {
	var declared = ant.Fields[User]()
	users.Where(ant.Gt(&declared.Age, 1)).All(ctx)

	var assigned *User
	assigned = ant.Fields[User]()
	users.Where(ant.Gt(&assigned.Age, 1)).All(ctx)

	copied := declared
	users.Where(ant.Gt(&copied.Age, 1), ant.Eq(&Late.Name, "x")).All(ctx)
}

// A variable that is ever given anything but a probe may hold that at any use,
// as the analyzer does not follow the order code runs in, so it is an ordinary
// variable throughout.
func reassigned(ctx context.Context, users *ant.Table[User]) {
	p := &User{}
	users.Where(ant.Gt(&p.Age, 1)).All(ctx) // want `not of an ordinary variable`
	p = ant.Fields[User]()
	users.Where(ant.Gt(&p.Age, 1)).All(ctx) // want `not of an ordinary variable`

	q := ant.Fields[User]()
	q, _ = loaded()
	users.Where(ant.Gt(&q.Age, 1)).All(ctx) // want `not of an ordinary variable`
}

func loaded() (*User, error) { return &User{}, nil }

// Variables that copy each other hold a probe if one reaches them from
// ant.Fields and nothing else is ever assigned to them.
func cycles(ctx context.Context, users *ant.Table[User]) {
	p := ant.Fields[User]()
	q := p
	p = q
	users.Where(ant.Gt(&p.Age, 1), ant.Eq(&q.Name, "x")).All(ctx)

	r := ant.Fields[User]()
	s := r
	r = s
	s = &User{}
	users.Where(ant.Gt(&r.Age, 1)).All(ctx)    // want `not of an ordinary variable`
	users.Where(ant.Eq(&s.Name, "x")).All(ctx) // want `not of an ordinary variable`

	var a, b *User
	a = b
	b = a
	users.Where(ant.Gt(&a.Age, 1)).All(ctx) // want `not of an ordinary variable`
}

// A type parameter's model is known only where the function is instantiated.
func generic[T any](ctx context.Context, q *ant.Query[T], rows *ant.Table[Row[T]]) {
	q.Where(ant.Gt(&U.Age, 1)).All(ctx)
	rows.Find(ctx, 1)
}

// Ref is a key of its own type, which ant takes a key of the same kind for.
type Ref [4]byte

type Tagged struct {
	ID Ref
}

func findKinds(ctx context.Context, tagged *ant.Table[Tagged], users *ant.Table[User]) {
	tagged.Find(ctx, [4]byte{})
	tagged.Find(ctx, "x")        // want `Find key is string, and Tagged has a key of type example.Ref`
	users.Find[string](ctx, "1") // want `Find key is string, and User has a key of type int64`

	x := 5
	ant.Gt(&x, 2)        //antvet:ignore a comment at the end of a line covers that line only
	users.Find(ctx, "2") // want `Find key is string, and User has a key of type int64`
}
