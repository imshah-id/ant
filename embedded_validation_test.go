package ant_test

import (
	"strings"
	"testing"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/dialect/sqlite"
)

type embeddedAddress struct{ City string }
type invalidEmbeddedModel struct {
	ID      int64
	Address *embeddedAddress `ant:"embedded,prefix=addr_"`
}
type invalidEmbeddedDB struct {
	ant.Context
	Rows ant.Table[invalidEmbeddedModel]
}

func TestWrapRejectsEmbeddedPointer(t *testing.T) {
	db, err := ant.Wrap[invalidEmbeddedDB](sqlite.Dialect, nil)
	if db != nil || err == nil || !strings.Contains(err.Error(), "invalidEmbeddedModel.Address") {
		t.Fatalf("Wrap = (%v, %v), want named embedded field error", db, err)
	}
}
