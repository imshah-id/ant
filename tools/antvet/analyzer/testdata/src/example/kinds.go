package example

import (
	"context"

	"github.com/skudra-go/ant"

	"models"
)

// Aliases anywhere in a model's type arguments name the same model.
var (
	Ra = ant.Fields[Row[[4]Key]]()          // want Ra:"probe of example.Row\\[\\[4\\]int64\\]"
	Rc = ant.Fields[Row[chan Key]]()        // want Rc:"probe of example.Row\\[chan int64\\]"
	Rf = ant.Fields[Row[func(Key) Key]]()   // want Rf:"probe of example.Row\\[func\\(int64\\) int64\\]"
	Rs = ant.Fields[Row[struct{ K Key }]]() // want Rs:"probe of example.Row\\[struct\\{K int64\\}\\]"
	Rm = ant.Fields[Row[map[Key][]*Key]]()  // want Rm:"probe of example.Row\\[map\\[int64\\]\\[\\]\\*int64\\]"
	Rq = ant.Fields[Row[models.Code]]()     // want Rq:"probe of example.Row\\[models.Code\\]"
)

func deep(
	ctx context.Context,
	arrays *ant.Table[Row[[4]int64]],
	chans *ant.Table[Row[chan int64]],
	funcs *ant.Table[Row[func(int64) int64]],
	structs *ant.Table[Row[struct{ K int64 }]],
	maps *ant.Table[Row[map[int64][]*int64]],
	codes *ant.Table[Row[string]],
) {
	arrays.Where(ant.Eq(&Ra.Name, "x")).All(ctx)
	chans.Where(ant.Eq(&Rc.Name, "x")).All(ctx)
	funcs.Where(ant.Eq(&Rf.Name, "x")).All(ctx)
	structs.Where(ant.Eq(&Rs.Name, "x")).All(ctx)
	maps.Where(ant.Eq(&Rm.Name, "x")).All(ctx)
	arrays.Find(ctx, [4]Key{})

	arrays.Where(ant.Eq(&Rc.Name, "x")).All(ctx) // want `condition on Row\[chan int64\] in a query of Row\[\[4\]int64\]`
	codes.Where(ant.Eq(&Rq.Name, "x")).All(ctx)  // want `condition on Row\[Code\] in a query of Row\[string\]`
	arrays.Find(ctx, [3]int64{})                 // want `Find key is \[3\]int64, and Row\[\[4\]int64\] has a key of type \[4\]int64`
}

// Ri is a probe of a model whose type argument is an interface.
var Ri = ant.Fields[Row[interface{ Get() int64 }]]() // want Ri:"probe of example.Row\\[interface\\{Get\\(\\) int64\\}\\]"

// A type parameter inside an interface leaves the model open, as anywhere else,
// so the query is not judged until an instantiation fills it in.
func opened[T any](ctx context.Context, rows *ant.Table[Row[interface{ Get() T }]]) {
	rows.Where(ant.Eq(&Ri.Name, "x")).All(ctx)
}

// Two models of one name are still two models.
func shadowed(ctx context.Context) {
	type Thing struct {
		ID   int64
		Name string
	}

	outer := ant.Fields[Thing]()

	{
		type Thing struct {
			ID   int64
			Name string
		}

		var things *ant.Table[Thing]
		things.Where(ant.Eq(&outer.Name, "x")).All(ctx) // want `condition on Thing in a query of Thing`
	}
}

// A type parameter anywhere in a model leaves it unknown.
func genericDeep[T any](
	ctx context.Context,
	rows *ant.Table[Row[func(T)]],
	maps *ant.Table[Row[map[string]T]],
	structs *ant.Table[Row[struct{ K T }]],
) {
	rows.Where(ant.Eq(&Rf.Name, "x")).All(ctx)
	maps.Where(ant.Eq(&Rm.Name, "x")).All(ctx)
	structs.Where(ant.Eq(&Rs.Name, "x")).All(ctx)
	rows.Find(ctx, 1)
}

// An aggregate of a query whose model is a type parameter is left alone too.
func genericSum[T any](ctx context.Context, q *ant.Query[T]) {
	ant.SumOf(ctx, q, &U.Age)
	ant.Pluck(ctx, q.Where(ant.Eq(&P.ID, "x")), &U.Name)
}
