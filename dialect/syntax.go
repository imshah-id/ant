package dialect

import (
	"slices"
	"sync"
)

// Feature names something that not every database has.
type Feature string

// The features a dialect can lack. Render reports one with ant.ErrUnsupported,
// rather than leaving the database to refuse the SQL.
const (
	FullJoin        Feature = "FULL JOIN"
	LateralJoin     Feature = "lateral joins"
	Merge           Feature = "MERGE"
	RowLocks        Feature = "row locks"
	ShareLocks      Feature = "FOR SHARE"
	OnConflict      Feature = "ON CONFLICT"
	ConflictWhere   Feature = "a condition on an upsert"
	ReturningInsert Feature = "returning rows from INSERT"
	ReturningUpdate Feature = "returning rows from UPDATE"
	ReturningDelete Feature = "returning rows from DELETE"
	DefaultKeyword  Feature = "DEFAULT in a VALUES list"
	DateCast        Feature = "CAST to date with CURRENT_DATE"
	DatePart        Feature = "DATE_PART"
	SubqueryWith    Feature = "WITH inside a subquery"
)

// Syntax says where a dialect's SQL differs from what Render writes, which is
// PostgreSQL's. The zero value changes nothing, so a dialect states only its
// differences.
type Syntax struct {
	// Missing lists the features the database lacks.
	Missing []Feature

	// OffsetFetch writes OFFSET n ROWS FETCH NEXT m ROWS ONLY for paging, which
	// needs an ORDER BY, so one is added if the query has none.
	OffsetFetch bool
	// Unlimited is the LIMIT to write when a query has only an OFFSET, for a
	// database that cannot have one without the other.
	Unlimited string
	// Output writes OUTPUT INSERTED.x in place of RETURNING x.
	Output bool
	// DuplicateKey writes ON DUPLICATE KEY UPDATE in place of ON CONFLICT.
	DuplicateKey bool
	// EmptyValues writes VALUES () in place of DEFAULT VALUES.
	EmptyValues bool

	// NoILike turns ILIKE into a LIKE between lowered values.
	NoILike bool
	// NoFilter turns an aggregate's FILTER into a CASE inside it.
	NoFilter bool
	// NoNullsOrder turns NULLS FIRST and NULLS LAST into a CASE before the value.
	NoNullsOrder bool
	// NoBooleans turns EXISTS used as a value into a CASE.
	NoBooleans bool
	// NoRowIn spells out multi-column IN as ORs.
	NoRowIn bool
	// BareSets leaves the parentheses off UNION members.
	BareSets bool
	// ValuesUnion writes a VALUES source as SELECTs joined by UNION ALL, for a
	// database that cannot name its columns.
	ValuesUnion bool
	// WrapLimitedIn puts a derived table around a subquery with a LIMIT inside
	// IN, for a database that refuses it directly.
	WrapLimitedIn bool
	// WrapSelfReads puts a derived table around a subquery of an UPDATE or DELETE
	// that reads the table being written, for a database that refuses such a read
	// but reads a derived table, which it materializes first.
	WrapSelfReads bool
	// HintInSelect moves an optimizer hint to just after SELECT.
	HintInSelect bool
	// MergeSemicolon ends MERGE with the semicolon the database demands.
	MergeSemicolon bool
	// TransactSavepoints uses SAVE TRANSACTION, which has no release.
	TransactSavepoints bool

	// UTCTimes binds times in UTC, for a database whose columns drop the zone, so
	// that every stored time is on one clock.
	UTCTimes bool

	// Functions renames functions, such as LENGTH to LEN.
	Functions map[string]string
	// MaxParameters is the most bound values in one statement. The default is
	// PostgreSQL's 65535.
	MaxParameters int
}

// Lacks reports whether the database is missing f.
func (s *Syntax) Lacks(f Feature) bool { return slices.Contains(s.Missing, f) }

// ParameterLimit returns the most bound values in one statement.
func (s *Syntax) ParameterLimit() int {
	if s.MaxParameters == 0 {
		return 65535
	}

	return s.MaxParameters
}

// SavepointCommands returns the commands that take a savepoint's name. An empty
// release means the database has no such command, and nothing needs running.
func (s *Syntax) SavepointCommands() (save, rollback, release string) {
	if s.TransactSavepoints {
		return "SAVE TRANSACTION ", "ROLLBACK TRANSACTION ", ""
	}

	return "SAVEPOINT ", "ROLLBACK TO SAVEPOINT ", "RELEASE SAVEPOINT "
}

// Syntactic is implemented by a dialect whose SQL differs from PostgreSQL's.
type Syntactic interface {
	Syntax() *Syntax
}

var standard Syntax

// SyntaxOf returns d's syntax.
func SyntaxOf(d Dialect) *Syntax {
	if s, ok := d.(Syntactic); ok {
		return s.Syntax()
	}

	return &standard
}

// Supports reports whether d's database has f.
func Supports(d Dialect, f Feature) bool { return !SyntaxOf(d).Lacks(f) }

var (
	statesMu sync.RWMutex
	states   []func(error) string
)

// RegisterErrors adds a function that turns a driver's error into a SQLSTATE
// code, or "", for a database whose driver does not report one. A dialect calls
// it when imported, which is what lets ant.IsUniqueViolation work everywhere.
func RegisterErrors(state func(error) string) {
	statesMu.Lock()
	defer statesMu.Unlock()

	states = append(states, state)
}

// StateOf returns the SQLSTATE code a registered dialect finds in err, or "".
func StateOf(err error) string {
	statesMu.RLock()
	defer statesMu.RUnlock()

	for _, state := range states {
		if code := state(err); code != "" {
			return code
		}
	}

	return ""
}
