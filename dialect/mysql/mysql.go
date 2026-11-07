// Package mysql holds the MySQL and MariaDB dialects. It imports the driver,
// which has no other way to expose its error numbers, so importing this package
// also registers it.
package mysql

import (
	"errors"

	"github.com/go-sql-driver/mysql"

	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/qb"
)

var (
	// Dialect is the MySQL dialect, for version 8.0 or later. Keys come from
	// LastInsertId, as MySQL has no RETURNING.
	Dialect dialect.Dialect = impl{name: "mysql", syntax: &mysqlSyntax}
	// MariaDB is the MariaDB dialect, for version 10.5 or later, which adds
	// INSERT and DELETE ... RETURNING.
	MariaDB dialect.Dialect = impl{name: "mariadb", returning: true, syntax: &mariaSyntax}
)

var (
	mysqlSyntax = selfReads(shared(dialect.ReturningInsert, dialect.ReturningDelete))
	mariaSyntax = shared(dialect.ShareLocks, dialect.LateralJoin)
)

// selfReads marks MySQL, which refuses a write whose subquery reads the table
// being written (error 1093). MariaDB allows it.
func selfReads(s dialect.Syntax) dialect.Syntax {
	s.WrapSelfReads = true

	return s
}

func shared(missing ...dialect.Feature) dialect.Syntax {
	return dialect.Syntax{
		Missing: append(
			missing,
			dialect.FullJoin,
			dialect.Merge,
			dialect.ConflictWhere,
			dialect.ReturningUpdate,
			dialect.DatePart,
		),
		Unlimited:     "18446744073709551615",
		DuplicateKey:  true,
		EmptyValues:   true,
		NoILike:       true,
		NoFilter:      true,
		NoNullsOrder:  true,
		ValuesUnion:   true,
		WrapLimitedIn: true,
		HintInSelect:  true,
		Functions:     map[string]string{"RANDOM": "RAND"},
	}
}

type impl struct {
	name      string
	returning bool
	syntax    *dialect.Syntax
}

func (d impl) Name() string     { return d.name }
func (impl) DriverName() string { return "mysql" }

func (impl) Placeholder(n int) string { return "?" }

func (impl) Quote(ident string) string { return dialect.QuoteWith(ident, "`", "`") }

func (d impl) SupportsReturning() bool { return d.returning }

func (d impl) Syntax() *dialect.Syntax { return d.syntax }

func (d impl) Render(stmt qb.Statement) (string, []any, error) {
	return dialect.Render(d, stmt)
}

func init() { dialect.RegisterErrors(state) }

// state translates the driver's error numbers. Its SQLSTATE is too coarse: 23000
// covers every constraint.
func state(err error) string {
	var e *mysql.MySQLError
	if !errors.As(err, &e) {
		return ""
	}

	switch e.Number {
	case 1062:
		return "23505"
	case 1216, 1217, 1451, 1452:
		return "23503"
	case 1048:
		return "23502"
	case 3819, 4025: // MySQL, MariaDB
		return "23514"
	case 1213:
		return "40P01"
	}

	return ""
}
