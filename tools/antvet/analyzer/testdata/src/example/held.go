package example

import (
	"context"

	"github.com/skudra-go/ant"
)

// A parameter holds what the caller gives it, so a probe assigned to it later
// does not make it one.
func param(ctx context.Context, users *ant.Table[User], p *User) {
	users.Where(ant.Gt(&p.Age, 1)).All(ctx) // want `not of an ordinary variable`
	p = ant.Fields[User]()
}

// Nor does it make a receiver one.
func (u *User) adults(ctx context.Context, users *ant.Table[User]) {
	users.Where(ant.Gt(&u.Age, 18)).All(ctx) // want `not of an ordinary variable`
	u = ant.Fields[User]()
}

// A closure's parameter, a range variable, and a variable whose address is
// taken hold values the analyzer cannot see either.
func unseen(ctx context.Context, users *ant.Table[User]) {
	each := func(p *User) {
		users.Where(ant.Gt(&p.Age, 1)).All(ctx) // want `not of an ordinary variable`
		p = ant.Fields[User]()
	}
	each(&User{})

	for _, r := range []*User{{}} {
		users.Where(ant.Gt(&r.Age, 1)).All(ctx) // want `not of an ordinary variable`
		r = ant.Fields[User]()
	}

	a := ant.Fields[User]()
	fill(&a)
	users.Where(ant.Gt(&a.Age, 1)).All(ctx) // want `not of an ordinary variable`
}

func fill(p **User) { *p = &User{} }

// Taken is a probe until its address is taken, through which anything may be
// stored, so no fact tells other packages it is one.
var Taken = ant.Fields[User]()

func take() **User { return &Taken }

// A named result starts out nil, which is no value, so one set from ant.Fields
// is a probe.
func namedResult(ctx context.Context, users *ant.Table[User]) (n *User) {
	n = ant.Fields[User]()
	users.Where(ant.Gt(&n.Age, 1), ant.Gt(&Taken.Age, 1)).All(ctx) // want `not of an ordinary variable`

	return n
}

// A probe held in a field, or returned by a function, may be one, so neither is
// judged, while an element of a slice or a value declared with no probe is an
// ordinary variable.
var holder = struct{ U *User }{ant.Fields[User]()}

func probe[T any]() *T { return ant.Fields[T]() }

func unknowable(ctx context.Context, users *ant.Table[User], posts *ant.Table[Post]) {
	users.Where(ant.Gt(&holder.U.Age, 1)).All(ctx)
	users.Where(ant.Gt(&probe[User]().Age, 1)).All(ctx)
	posts.Where(ant.Gt(&holder.U.Age, 1)).All(ctx)

	list := []*User{{}}
	users.Where(ant.Gt(&list[0].Age, 1)).All(ctx) // want `not of an ordinary variable`

	var value User
	users.Where(ant.Gt(&value.Age, 1)).All(ctx) // want `not of an ordinary variable`
}
