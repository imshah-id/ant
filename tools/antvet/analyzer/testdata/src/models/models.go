// Package models defines models in another package, whose keys reach the
// analyzer as facts.
package models

import "github.com/skudra-go/ant"

// Country's key is its code, not its ID.
type Country struct { // want Country:"key Code"
	ID   int64
	Code string
}

func (Country) Configure(b *ant.ModelBuilder[Country]) {
	b.Key(&Co.Code)
}

var Co = ant.Fields[Country]() // want Co:"probe of models.Country"

// Code is a type of this package that another package's model takes as a type
// argument.
type Code string
