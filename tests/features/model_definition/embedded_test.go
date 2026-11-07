package model_definition

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/dialect/postgres"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// A struct groups columns. Anonymous embedding flattens it, and the embedded tag
// does the same for a named field, with an optional column prefix.
type Address struct {
	City string
	Zip  string
}

type Audit struct {
	CreatedBy string
}

type Shop struct {
	Audit
	ID       int64
	Name     string
	Billing  Address `ant:"embedded,prefix=billing_"`
	Shipping Address `ant:"embedded,prefix=shipping_"`
}

type ShopsDB struct {
	ant.Context
	Shops ant.Table[Shop]
}

var Sh = ant.Fields[Shop]()

func TestEmbedded(t *testing.T) {
	ddl := `CREATE TABLE shops (
		created_by TEXT NOT NULL, id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL,
		billing_city TEXT NOT NULL, billing_zip TEXT NOT NULL,
		shipping_city TEXT NOT NULL, shipping_zip TEXT NOT NULL)`
	fixture.RunTrackedWith(t, ddl, func(t *testing.T, db *ShopsDB) {
		ctx := t.Context()

		shop := &Shop{
			Name:     "a",
			Audit:    Audit{"ann"},
			Billing:  Address{"Riga", "1"},
			Shipping: Address{"Oslo", "2"},
		}
		require.NoError(t, db.Shops.Insert(ctx, shop))

		t.Run("values round trip", func(t *testing.T) {
			got, err := db.Shops.NoTracking().Find(ctx, shop.ID)
			require.NoError(t, err)
			require.Equal(t, *shop, *got)
		})

		t.Run("probes reach inside", func(t *testing.T) {
			got, err := db.Shops.NoTracking().
				Where(ant.Eq(&Sh.Shipping.City, "Oslo"), ant.Eq(&Sh.CreatedBy, "ann")).
				Single(ctx)
			require.NoError(t, err)
			require.Equal(t, "Riga", got.Billing.City)
		})

		t.Run("two fields of one struct stay apart", func(t *testing.T) {
			n, err := db.Shops.Where(ant.Eq(&Sh.Billing.City, "Oslo")).Count(ctx)
			require.NoError(t, err)
			require.Zero(t, n)
		})

		t.Run("a tracked change writes one column", func(t *testing.T) {
			shop.Billing.Zip = "9"

			require.NoError(t, db.SaveChanges(ctx))
			got, err := db.Shops.NoTracking().Find(ctx, shop.ID)
			require.NoError(t, err)
			require.Equal(t, "9", got.Billing.Zip)
			require.Equal(t, "2", got.Shipping.Zip)
		})
	})
}

// A field tagged embedded must be a struct value: a pointer, which could be nil,
// is refused with an error that names the field, when the context opens, and so
// is a raw query or a projection that would scan into such a struct, rather than
// leaving the fields after it empty.
type Courier struct {
	ID    int64
	Depot *Address `ant:"embedded,prefix=depot_"`
	Name  string
}

type CouriersDB struct {
	ant.Context
	Couriers ant.Table[Courier]
}

func TestEmbeddedPointerIsRefused(t *testing.T) {
	ctx := t.Context()

	const named = "embedded field Courier.Depot must be a struct value"

	_, err := ant.Wrap[CouriersDB](postgres.Dialect, nil)
	require.ErrorContains(t, err, named)

	db, err := ant.Wrap[ShopsDB](postgres.Dialect, nil)
	require.NoError(t, err)

	_, err = ant.Raw[Courier](db, "SELECT 1 AS id, 'x' AS name").All(ctx)
	require.ErrorContains(t, err, named)

	_, err = ant.Raw[Courier](db, "SELECT 1 AS id, 'x' AS name").First(ctx)
	require.ErrorContains(t, err, named)

	co := ant.Fields[Courier]()

	_, err = ant.Project[Courier](db.Shops.Query(), ant.Into(&co.ID, &Sh.ID)).All(ctx)
	require.ErrorContains(t, err, named)
}
