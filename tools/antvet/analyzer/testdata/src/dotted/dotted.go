// Package dotted imports ant with a dot, so its calls have bare names.
package dotted

import (
	"context"

	. "github.com/skudra-go/ant"
)

// Slugged's key is a slug, set through a dot import.
type Slugged struct { // want Slugged:"key Slug"
	ID   int64
	Slug string
}

func (Slugged) Configure(b *ModelBuilder[Slugged]) {
	GenerateKey(b, &S.Slug, func() string { return "x" })
}

var S = Fields[Slugged]() // want S:"probe of dotted.Slugged"

func use(ctx context.Context, slugged *Table[Slugged]) {
	slugged.Find(ctx, "a-slug")
	slugged.Find(ctx, 1) // want `Find key is int, and Slugged has a key of type string`
}
