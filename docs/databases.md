# Databases

ant runs on five databases. PostgreSQL has everything. The others run queries, writes, relations, change tracking, and transactions, but have no migrations yet.

| | PostgreSQL | MySQL | MariaDB | SQLite | SQL Server |
| --- | --- | --- | --- | --- | --- |
| Tested on | 13 to 18 | 8.4 | 11.8 | modernc 1.61 | 2022 |
| Dialect | `postgres.Dialect` | `mysql.Dialect` | `mysql.MariaDB` | `sqlite.Dialect` | `sqlserver.Dialect` |
| Driver name | `pgx` | `mysql` | `mysql` | `sqlite` | `sqlserver` |
| Queries, relations, tracking, transactions | Yes | Yes | Yes | Yes | Yes |
| Migrations, `EnsureCreated`, `SchemaOf` | Yes | No | No | No | No |
| Whole feature suite (`tests/features`) | Yes | No | No | No | No |
| Conformance suite (`tests/conformance_test.go`) | Yes | Yes | Yes | Yes | Yes |
| Upsert | Yes | Yes | Yes | Yes | No |
| `MERGE` | From 15 | No | No | No | Yes |
| Row locks | Yes | Yes | `ForUpdate` only | No | No |
| `FULL JOIN` | Yes | No | No | Yes | Yes |
| Lateral joins | Yes | Yes | No | No | No |
| `UpdateReturning` | Yes | No | No | Yes | Yes |
| `DeleteReturning`, `Toggle` | Yes | No | Yes | Yes | Yes |
| `Today` | Yes | Yes | Yes | No | No |
| `DatePart` | Yes | No | No | No | No |
| `WITH` in a subquery, as in `Update` or `Delete` after `FromCTE` or `Recursive` | Yes | Yes | Yes | Yes | No |
| `ILike`, `FILTER`, `NULLS FIRST` | Native | Emulated | Emulated | `ILike` emulated | Emulated |

Only PostgreSQL runs the whole feature suite, whose tables are written in its DDL. The conformance suite covers what the dialects write differently, on every database, but it is smaller, so expect rough edges off PostgreSQL that nobody has hit yet.

## When something is missing

ant refuses before sending the SQL, so the error says what is wrong:

```go
_, err := ant.Merge(&db.Stock, source, on).WhenMatched(set).Exec(ctx)
errors.Is(err, ant.ErrUnsupported) // true on MySQL
// ant: not supported by this database: MERGE on mysql
```

To check first, ask the dialect:

```go
if dialect.Supports(db.Dialect(), dialect.Merge) {
    // ...
}
```

Error helpers mean the same everywhere. `ant.IsUniqueViolation` works on all five, and each dialect translates its own error numbers.

## What differs

**Generated keys.** MySQL has no `RETURNING`, so a key comes from `LastInsertId`, and other database-filled columns are read back with a second query. Rows with generated keys go in one at a time there, in both `SaveChanges` and `InsertMany`.

**`ON DUPLICATE KEY` on MySQL and MariaDB.** It has no target: any unique key triggers it, not only the columns you name. If a different key is the one that collides, `Upsert` cannot find the row by your columns, and reports that it wrote nothing. Name the only unique key that can collide. It also takes no `WHERE`, so `Conflict.Where` is an error, and an `Upsert` of a copy that carries a version cannot check that version: the version moves on, but the copy's update goes ahead.

**`OUTPUT` on SQL Server.** SQL Server refuses it on a table with triggers, so a model that needs a key back cannot be inserted into one.

**Upserts on SQL Server.** There is no `ON CONFLICT`. Use `ant.Merge`.

**Paging on SQL Server.** `OFFSET ... FETCH` needs an `ORDER BY`, so ant adds a meaningless one if the query has none. SQL Server refuses a `Limit` of zero.

**Times.** SQLite stores times as text, and SQL Server's `DATETIME2` has no zone, so ant binds UTC on both to keep one clock. MySQL's driver does the same. `ant.Past` and `ant.Future` compare with the server's own clock, so run the server in UTC. On SQLite and MySQL, `CURRENT_TIMESTAMP` has whole seconds.

**Parameters.** Limits differ: 65,535 on PostgreSQL and MySQL, 32,766 on SQLite, and about 2,100 on SQL Server, where ant stays at 2,000. `InsertMany`, `SaveChanges`, and `Include` split their work to fit. Your own `In` with a long list does not.

**Types.** `postgres.Array`, `postgres.JSONSet`, `postgres.Vector`, and the rest of `dialect/postgres` need PostgreSQL. `Collate` quotes its name, which SQL Server does not accept.

## Drivers

Import a driver in your application. ant does not choose one for you, with one exception: `dialect/mysql` imports `go-sql-driver/mysql`, because it has no other way to read the driver's error numbers.

```go
import _ "github.com/jackc/pgx/v5/stdlib"     // PostgreSQL
import _ "github.com/go-sql-driver/mysql"     // MySQL and MariaDB
import _ "modernc.org/sqlite"                 // SQLite
import _ "github.com/microsoft/go-mssqldb"    // SQL Server
```

Use `parseTime=true` in a MySQL DSN, or times come back as bytes. Use `_pragma=foreign_keys(1)` in a SQLite DSN, or foreign keys are not enforced.

## Adding a dialect

A dialect implements `dialect.Dialect`. Its `Render` calls `dialect.Render`, and its `Syntax` method lists what differs from PostgreSQL, so it states only its differences. See `dialect/sqlite/sqlite.go`, which is about 70 lines. `dialect.RegisterErrors` connects its error codes. Migrations are a separate interface, `schema.Dialect`.
