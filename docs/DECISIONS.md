# Decisions

This file records where the implementation brief was ambiguous or needed refining, and what was chosen. Where the brief left a choice open, the option closest to EF Core was taken.

The signatures were compiled with Go 1.27.3 against a stub on 2026-10-31.

## 1. `ant.Ordered` replaces `cmp.Ordered`

The brief declares `Gt[V cmp.Ordered]` and `OrderBy[V cmp.Ordered]`. `time.Time` does not satisfy `cmp.Ordered`, so `OrderBy(&U.CreatedAt)` and `Gt(&U.CreatedAt, t)` do not compile, although `ant.Base` supplies those columns. EF Core orders by dates.

Decision: define `type Ordered interface{ cmp.Ordered | time.Time }` and use it for ordering and comparison conditions. Other orderable types, such as UUID or decimal types, remain an open extension.

## 2. `Tx` is a generic method on `Context`

`db.Tx(ctx, func(tx *AppDB) error {...})` must hand back the user's type, but `Tx` is promoted from the embedded `ant.Context`, which does not know that type.

Decision: `func (c *Context) Tx[DB any](ctx context.Context, f func(*DB) error) error`. `DB` is inferred from the closure, and promotion through embedding compiles. A closure with the wrong context type is a runtime error returned by `Tx`.

## 3. `Find` takes a generic key

An `any` key would break the rule against `any` in the query API, and `Table[T]` has no type parameter for the key.

Decision: `func (t *Table[T]) Find[K comparable](ctx context.Context, key K) (*T, error)`. A key of the wrong type is returned as an error at runtime. This is a known limit.

## 4. Conditions are not tied to a model

The target API uses `ant.Gt(&U.Age, 18)` with no model type, and `Where` takes plain `Cond` values. Adding a model parameter to `Cond` would make condition calls uglier.

Decision: keep `Cond` untyped by model. Probes from the wrong model, and non-probe pointers, are returned as errors at the terminal call. A `go vet` analyzer is already planned for v0.9.

## 5. `Select` closures run client-side

EF Core translates projections to SQL through expression trees. Go has no equivalent, so a closure such as `func(p *Post, u *User) Summary` cannot be inspected.

Decision: load the mapped columns of each table in the join and call the closure once per row. Column pruning needs a separate explicit API, to be proposed on the roadmap.

## 6. Return shapes and tracking

Change tracking requires a stable pointer to the loaded entity, and the target API mutates `user.Age++` after `Find`.

Decision: `Find` and `First` return `*T`. `All` returns `[]T`, and the tracker holds pointers into that slice's backing array. `Each` yields `T` by value and is not tracked. EF Core tracks streamed entities, but a value copy in Go cannot be tracked.

## 7. `Eq` with a nil pointer

`ant.Eq(&U.Bio, nil)` compiles with `V = *string`.

Decision: nil renders as `IS NULL`, as EF Core does for null comparisons. A non-nil pointer compares the pointed-to value, not the pointer.

## 8. Step 1 file count

Step 1 of the brief says "three Markdown files" and lists four. All four were written.

## 9. Owner review of Step 1

Decisions 4 and 5 were accepted by the project owner on 2026-10-31. In decision 5, only the closure runs in Go. The join, filters, and ordering run in the database.

## 10. Module path

The module path is `github.com/skudra-go/ant`, taken from the repository remote. The owner gave `skudra-go/ant`.

## 11. Migration model

Status: approved by the owner on 2026-10-31. The owner asked for structs as the source of truth, a CLI, two folders, and no lock-in.

- **Source of truth:** the Go structs. A command compares them with a stored snapshot and writes the difference as plain SQL, which the developer can edit.
- **Snapshot, not a shadow database:** like EF Core, diffing uses a snapshot file in source control. Generating a migration needs no database and no permission to create one.
- **Two folders:** `migrations/versions` holds sequential files that run once. `migrations/code` holds repeatable SQL that reruns when its checksum changes, after all versioned files, as in Evolve and Flyway.
- **Plain SQL only:** no proprietary format. Any tool or person can read the files, and leaving ant keeps them usable.
- **Pluggable storage:** the runner reads from an `io/fs.FS` by default, so `go:embed` works. A small interface replaces file handling entirely. Unset options use defaults.
- **The CLI lives in the user's module:** Go cannot load structs from another module at runtime. A small `main` calls `migrate.Main[AppDB]`, so commands run as `go run ./cmd/migrate diff add_bio`.
- **Commands:** `diff`, `up`, `status`, and `check`. `check` fails when models and snapshot differ, for use in CI.
- **Renames:** a diff cannot tell a rename from a drop and add, so it writes a drop and add with a warning comment, and the developer edits the SQL.
- **No down migrations:** rollback is out of scope. To undo a change, write a new forward migration.

## 12. Additions to the `Dialect` contract

The brief lists five `Dialect` methods. `Open` also needs the `database/sql` driver name, so the interface adds `DriverName() string`. The application still imports the driver, so the core has no driver dependency.

## 13. MariaDB shares the `mysql` package

The brief's layout has no `dialect/mariadb`. `mysql.MariaDB` lives beside `mysql.Dialect`. It differs in `SupportsReturning`, which was confirmed against live servers: `INSERT ... RETURNING` works on MariaDB 11.8 and is a syntax error on MySQL 8.4.

## 14. The `Dialect` interface exposes an internal type

`Render` takes a `qb.Statement`, and the brief places `qb` under `internal`. The interface is pluggable inside this module, but a third-party module cannot implement it. The roadmap makes the AST public in v0.4.

## 15. Integration failures do not fail CI yet

The brief expects API tests to fail with `ErrNotImplemented`, so the integration jobs use `continue-on-error`. Remove this when v0.1 starts passing them.

SQLite has its own job, because it needs no service container.

## 16. Test schema is hand-written

`internal/testdb` creates the fixture tables with per-database SQL. Migrations exist now, but only on PostgreSQL, and the harness covers all five databases, so it stays hand-written until v0.4.

## 17. Implemented ahead of the skeleton

Some bodies are real because later behavior or the acceptance criteria depend on them: `Open`, table binding, probe resolution, condition and assignment construction, and builder error accumulation. Everything that runs SQL against a model returns `ErrNotImplemented`.

## 18. Wrong-model detection

`Where`, ordering, relations, and assignments compare each probe's model with the models in the query. A mismatch is `ErrWrongModel` at the terminal call.

## 19. A zero generated key means "assign one"

Decision 8 of the brief forbids reading meaning into zero values, but an integer primary key needs a way to ask for a generated value. As EF Core does, ant treats zero in a generated key as "assign one". This is the only exception. `b.Key(&T.ID).NotGenerated()` removes it for a model, and non-integer keys are never generated.

## 20. Queries are immutable

Every `Query[T]` builder method returns a new query, so a query can safely be the base of several others. GORM's documentation warns about reusing chained statements, and this removes that risk.

A later `OrderBy` replaces earlier ones, and `ThenBy` adds to them, as in EF Core. `ThenBy` with no `OrderBy` is `ErrThenWithoutOrder`, because Go has no equivalent of C#'s `IOrderedQueryable` to catch it at compile time.

## 21. Configuration runs when a context opens

`Configure` cannot run inside `ant.Fields`, because it refers to probes that may not be initialized yet. It runs once, from `Open`, on first query, or from `Raw`. Errors in it stop `Open`.

AST nodes share a pointer to the table name, so a condition built before `Open` still renders the configured table.

## 22. Inserts run one statement per row

Several entities are written one at a time in a transaction, which keeps generated keys matched to entities and makes the call all or nothing. Generated keys are cleared again if the call fails. Faster batched inserts are planned in v0.5.

Until v0.3, `Insert` returns `ErrNotImplemented` if an entity has related entities set, rather than dropping them. `Include`, `IncludeOne`, and `Join` do the same.

## 23. Guarding bulk writes

`Update` and `Delete` return `ErrNoCondition` unless a condition can exclude rows or `Unfiltered` was called. Conditions that cannot exclude a row do not count, such as `ant.And()` or `ant.NotIn` with an empty list, because they usually come from an empty slice. The roadmap's original `q.All()` name was replaced, since `All` is already a terminal call.

## 24. Raw SQL arguments

The rules are in the `dialect.Bind` documentation. PostgreSQL has a `?` jsonb operator, so `?` is only a placeholder when the query has no `$n` and exactly one `?` per argument. A slice expands only when it fills an `IN (...)` list, so `= ANY(@ids)` keeps passing an array. An empty slice becomes `NULL`, so `NOT IN (?)` with an empty slice matches nothing, unlike `ant.NotIn`.

## 25. Scanning

Result columns with no field are ignored, and fields with no column are left alone, so `SELECT name` into a model works. `internal/scan` uses `Rows.Scan`, which calls `sql.ConvertAssign` itself, instead of the direct calls the brief describes.

`ant.Base` timestamps are written as they are until automatic timestamps arrive in v0.6, so a zero time is stored as `0001-01-01`.

## 26. Feature examples run on PostgreSQL only

`tests/features` holds one small example per feature. It uses PostgreSQL SQL for its tables, and skips other databases until v0.4. Each PostgreSQL test gets its own schema, so test packages can run in parallel against one server.

## 27. `All` returns `[]*T`

Decision 6 said `All` returns `[]T`. That made `for _, u := range users { u.Age++ }` silently lose the change, because `u` is a copy, and it made an identity map impossible, because one row could not be one instance. `All`, `First`, `Single`, and `Find` now all return pointers. `Each` and `Raw` still return values, and neither is tracked.

## 28. Identity and relation slices

As in EF Core, a tracked query returns the existing instance for a row, and `Find` returns it without a query. The exception is `[]Post` relation fields, whose elements are values. A row in one of those is a second tracked instance with its own snapshot, and changes to either are saved. Field-level updates mean the two do not overwrite each other unless both change the same column.

## 29. Sessions

Applications usually keep one `db` for the life of the process, and a tracker on it would grow forever and mix up concurrent requests. `ant.Session(db)` returns a context with its own tracker on the same pool, which is the Go form of EF Core's short-lived `DbContext`. Use one per unit of work, or `ant.WithTracking(false)` on contexts that only use explicit updates.

## 30. Tracking follows keys, not addresses

Appending to a relation slice can move its elements, and `slices.Delete` shifts them in place, so an address says nothing about which row it holds. Every relation is reconciled by key before anything else in `SaveChanges`. A removed row is deleted or unlinked using a copy of its snapshot, and its key comes from the entry, never from memory.

## 31. Failed saves

`SaveChanges` runs in one transaction, or a savepoint inside one. On failure nothing is saved, generated keys are cleared, and pending changes remain, so the call can be repeated. A rollback of an enclosing transaction also reverses what the tracker recorded as saved, which EF Core does not do. An entity whose row vanished is `ErrConcurrency`, and a changed primary key is `ErrKeyChanged`, rather than a write to the wrong row.

## 32. Navigation and foreign key changes

A relation that still holds what was loaded does not touch the foreign key, so `post.UserID = 5` works with `post.User` loaded. A row removed from one loaded relation and added to another is moved, not deleted. A row in a new position only updates its foreign key.

## 33. Include shape functions

`Include` takes an optional function over the related query, instead of separate APIs for filtering, ordering, limiting, and nesting. `Limit` and `Offset` apply per parent, using `ROW_NUMBER`. `Each` refuses includes with `ErrIncludeInEach`, because a second query cannot run on a connection that is streaming rows inside a transaction.

## 34. Self-joins

The second occurrence of a table gets an alias, so each side scans into its own row. Probes cannot tell the two sides apart, so a condition on a self-joined model applies to the first side.

## 35. Typed expressions

`Expr[V]` is the one abstraction for anything with a value: a column, an aggregate, a subquery, or raw SQL. Its methods build conditions and orderings, and `IntoExpr` maps it to a result field, so `V` is checked by the compiler throughout. `Project` is how grouped and computed results are read.

## 36. Derived tables keep the model's name

`From`, `FromCTE`, `FromRaw`, `Union`, and `Recursive` alias their source as the model's table, so probes work on the outer query unchanged. Their results are never tracked, because a derived row may not match a stored row. `Recursive` uses `UNION`, not `UNION ALL`, so a cycle in the data ends.

## 37. References to outer queries

Probe checks run at the terminal call, not as conditions are added. That lets `Where` come before `Join`, and lets a subquery refer to its outer query: unresolved models pass to the outer query, which checks them in turn.

## 38. Bulk writes

SQL has no `LIMIT` on `UPDATE` or `DELETE`, so a limited bulk write selects its rows by key in a subquery. So does a bulk write on a derived source, such as `From`, `FromCTE`, a set operation, or `Recursive`, because `UPDATE` and `DELETE` name only the table: without the subquery, the outer conditions would run against every row of it. The source must select the model's stored key, so a write refuses one narrowed by `Column` or `Omit`, `Values` of a model whose key is `writeonly`, and `FromRaw`, whose columns ant cannot see. A set operation matches columns by position, so its sides must hold the key in the same place. Without that check, SQLite, MySQL, and MariaDB resolve the missing key to the table being written, and change every row. The key subquery drops the ordering unless it pages, because order cannot change which rows are members, and SQL Server refuses it there.

The guard of decision 23 counts a derived source as filtered when its rows are. `From`, `FromCTE`, and `Recursive` take their source's, a union needs both sides filtered, an intersection either, and `EXCEPT` its first, since a second side that matches nothing leaves every row. Replacing the source replaces what it counts for, while the outer query's own conditions still count. SQL Server allows `WITH` only at the start of a statement, so a write after `FromCTE` or `Recursive` returns `ErrUnsupported` there. MySQL refuses a write whose subquery reads the table being written (error 1093), but it materializes a derived table there rather than merging it, so the key subquery needs no hint.

`InsertMany` matches generated keys to rows by sorting them, since one statement draws sequence values in row order but `RETURNING` promises no order. It does not copy keys with a conflict clause, where rows may be skipped.

## 39. Global filters are lazy

Filters can read the request context, such as a tenant, but subqueries are built before any context exists. A filter is a `qb.Lazy` node that the renderer resolves with the terminal call's context. So filters reach joins, `Include`, `Has`, and subqueries without anything having to remember them, and the same query object run under two tenants sees different rows. On a joined table, filters go in the `ON` clause, so they do not turn a left join into an inner one.

`Table.Find` skips the identity map for models with filters, because a tracked row may be one the filter now hides.

## 40. Soft deletes are a filter

A field of type `ant.DeletedAt` makes `Delete`, `DeleteByID`, and `Remove` mark rows, and adds a built-in global filter. `ForceDelete` removes rows for real.

## 41. Automatic timestamps

`time.Time` fields named `CreatedAt` and `UpdatedAt` are maintained on any model, as in GORM and Eloquent. They are stored in UTC, truncated to microseconds, so a value reads back exactly as written. A value the application set on insert is kept. `b.NoTimestamps()` opts out.

## 42. Converters are Scanner and Valuer

The roadmap planned `Convert(to, from)` functions. Go's `sql.Scanner` and `driver.Valuer` already do this, work in conditions without extra machinery, and need no ant API, so named types and those two interfaces are the mechanism. `ant.JSON[T]` and `postgres.Array[T]` are built that way.

## 43. Database-specific helpers

The root package stays neutral. PostgreSQL-only conditions and types live in `dialect/postgres`, built from the public `ant.SQL` and `ant.SQLCond`, so a dialect can add helpers without changing the core.

## 44. Many-to-many

A relation with `b.ManyToMany` goes through a join table with no model. `Include` reads the join table, then the targets. In a loaded relation, removing an element deletes the join row, never the target, unlike a one-to-many relation. For extra columns on the join table, model it, with two one-to-many relations.

## 45. Errors are classified, not wrapped

Roadmap v0.4 planned sentinels such as `ErrUniqueViolation`, which would need every error path to wrap driver errors, including those that only appear while reading rows. Instead `ant.IsUniqueViolation(err)` and its siblings read the SQLSTATE from the error chain, so nothing can be missed, and the driver's own error stays intact.

## 46. Interceptors

There is one point where every statement passes: the executor. `ant.Interceptor` wraps it with `Before` and `After`, which is enough for logging, metrics, tracing, and refusing or rewriting statements. `WithLogger` is built on it. OpenTelemetry and Prometheus packages are not included, because they would add dependencies. They are thin interceptors that can be added as sub-packages.

Values stay out of logs unless `ant.LogValues()` is used.

## 47. Hooks run inside the write

Hooks are methods on the model, found once per type, so models with none pay nothing. They run inside the unit's transaction, so an error from an After hook undoes the write, and a unit with After hooks always uses a transaction. Bulk `Update`, `Delete`, `InsertMany`, and `Raw` do not run them, because they do not load entities.

`Observer`s run after the commit, and never hear about a rollback.

## 48. Replicas

`WithReplicas` sends plain reads outside transactions to replicas in turn. A locking read, a transaction, `OnPrimary`, or a recent write with `WithStickyWrites` uses the primary. A failing replica is an error for that read, not a silent fallback.

## 49. PostgreSQL first

The owner chose to focus on PostgreSQL, so it has everything, and the feature suite runs on it alone. All SQL generation goes through the shared renderer and the `Dialect` interface, which is what let the other databases follow in [decision 74](#74-dialects-state-their-differences). Migrations for them are still open.

## 50. Three layers for migrations

`schema` describes a database as plain data and works out changes, with no SQL execution and no driver. `dialect/postgres` turns changes into SQL and reads live databases. `migrate` runs files and is the CLI.

`schema` is public, not internal, so a project can replace SQL generation with `Config.Render` and read a plan's changes. Migration support is a second interface, `schema.Dialect`, so the dialects without it still compile and return `schema.ErrUnsupported`.

## 51. Modeling added for migrations

Migrations need facts the structs did not hold, so parts of v0.6 arrived early: `b.Column`, a real `b.Index`, `b.IndexExpr`, and `b.Check`. `ant:"default=expr"` and `.Default(expr)` say what the default is. `ant:"default"` with no expression still works at runtime, and is an error when generating, since a column with no default would fail on the first insert.

`.Default` means what `ant:"default"` means: a zero field is left for the database. EF Core's `HasDefaultValue` does the same.

Whether a column allows NULL follows the Go type, including `sql.Null[T]`. A nil slice is written as NULL, so slices are nullable, and `ant.JSON` is not. A type with no obvious column is an error that says what to do. `schema.Typed` lets a type declare its own.

## 52. New NOT NULL columns fill existing rows

A plain `ADD COLUMN ... NOT NULL` fails on any table with rows, so a generated migration would work in development and break in production. Diff adds the column with its Go zero value as a default, then drops the default, so the column ends up as the model declares it. The value is in the file with a warning, and is easy to change.

`timestamptz` uses `now()`, and `uuid` uses `gen_random_uuid()`, because their zero values would be misleading. A type with no sensible value gets a warning instead.

## 53. Names follow PostgreSQL's defaults

Constraints and indexes are named `users_pkey`, `users_email_key`, `posts_user_id_fkey`, and `posts_user_id_idx`, as PostgreSQL itself names them, so a table written by hand usually needs no configuration to adopt. A name past the identifier limit is shortened with a hash, because the server would cut it silently and two names could become one.

## 54. Foreign keys

`OnDelete` defaults to the database's `NO ACTION`. EF Core cascades required relations by default, which silently deletes rows, and decision 8 prefers saying things out loud. The two sides of a relation are one key, either can configure it, and a contradiction is an error.

Foreign keys get an index unless one already leads with the column, as in EF Core. PostgreSQL does not make one, and a delete on the parent then scans the child. `.NoIndex()` opts out.

## 55. Renames

Decision 11 chose a drop and add with a warning. That stays the default, because a diff cannot know. Three things were added, since a warning alone loses data if missed: a terminal prompt, `--rename table.old=new`, and the exact `RENAME` statement in the warning, for editing the file by hand. Renames of objects follow by themselves: a constraint or index with the same definition under a new name is renamed, not rebuilt.

## 56. Failure

A migration runs in a transaction with its history row, so a crash cannot leave it half done, or done but unrecorded. A failure leaves no trace, and fixing the file is enough.

`-- ant:no-transaction` is for statements PostgreSQL refuses in a transaction, such as `CREATE INDEX CONCURRENTLY`. A failure there can leave work behind, so it is recorded, and `up` refuses to continue until `repair`, as Flyway does.

Connections keep settings past a commit, and a `pg_dump` file clears `search_path`, so each migration ends with `RESET ALL`.

## 57. Drift compares what the server stored

PostgreSQL rewrites what it is given: `int` becomes `integer`, and `x IN (1, 2)` becomes `x = ANY (ARRAY[1, 2])`. Matching that by hand would never be complete, and a tool with false alarms gets ignored.

So drift creates the expected tables as temporary tables in a transaction it rolls back, and reads them back, which needs no extra permission and leaves nothing. The roadmap rules out shadow databases for generating migrations. This is narrower: it only spells expressions, only for `drift`.

That is not quite enough, because the server prints one check in two ways, depending on whether it was typed or restored from a dump. When text differs, `SameExpression` compares with casts and parentheses removed. It can miss a change that only regroups an expression, which is the better mistake.

`drift` uses the snapshot when there is one, since that is what migrations produce. With none, it uses the models, which is what makes the adoption loop work.

## 58. Squashing needs no coordination

`-- ant:replaces 0006` at the top of a baseline says what it replaces. A database that has those migrations records the baseline without running it, and a new one runs it, so nobody has to run a command per environment, and rows for deleted files stay valid. A database partway through is refused, because neither path is right for it.

A squash writes from the models, which loses anything written by hand, so it warns, and `--keep` joins the old files instead.

## 59. Adopting an existing database

Generating Go structs from a live schema is not offered. The mapping is ambiguous, since `text` could be `string`, `*string`, a named type, or an enum, and relations cannot be read from foreign keys alone. A person or an assistant does this better, given a dump and a way to check the work.

So ant provides the check: `drift` works with no snapshot, and says exactly what still differs, so the loop ends when it reports nothing. `adopt` then makes a dump the first migration, removing psql commands that recent `pg_dump` versions write, and `baseline` records it on existing databases. The guide has a prompt to start from. The `migrate scaffold` item in v0.9 needs revisiting in light of this.

## 60. One program, two jobs

`go run ./cmd/migrate` writes files, and a deployed binary has no folder to read. `Config.Embedded` is used only when the folder is missing, so one `main.go` does both, with no flag to forget.

## 61. Sessions take options

`ant.Session(db)` copied a context's settings, including `WithTracking(false)`, so `SaveChanges` on such a session did nothing and returned nil. An application needs tracking off on its shared context, or it grows without end, so this is the normal setup. `Session` now takes options, and `ant.Session(db, ant.WithTracking(true))` is the unit of work. The blog example found this.

## 62. The statement AST is public

`internal/qb` moved to `qb`, because a dialect outside this module has to render it. It stays outside the stability promise until one exists, since only a real one will show whether the shape is right.

## 63. SaveChanges batches inserts

This revises [decision 22](#22-inserts-run-one-statement-per-row). New rows of one model go in together, up to 200 a statement, which turns a thousand round trips into a few. A model with insert hooks or a `.Touch()` relation is written row by row, as is a save with `WithoutAutoTx`, because each of those needs a result per row. Updates stay one statement each: a shared statement could only set one list of columns.

## 64. Through relations are read-only pivots

`HasManyThrough` reuses the many-to-many loader with a join on the intermediate table, marked read-only, because writing through it has no one obvious meaning.

## 65. Scoped relations use the same filter path

`.Where` on a relation, `.Morph`, `.Latest`, and `.Oldest` all add conditions wherever the relation is used, in `Include`, `Has`, and joins. A polymorphic relation has no foreign key, since one column cannot point at several tables.

## 66. Several models can share a table

`b.Discriminator` and table splitting are the same fact to migrations: models on one table merge into one definition, and a column not every model has allows NULL. Query results stay one type per model. There is no `[]any` of mixed subtypes.

## 67. Naming and conventions are global

`WithNaming` and `WithConvention` apply once per model, in the first `Open`, because a model is reflected once per process. Per-connection naming would need a second model registry, for a need nobody has.

## 68. Sensitive values are wrapped when bound

A `.Sensitive()` field's value is wrapped in `qb.Secret` as it enters the statement, so it prints as `[redacted]` wherever it travels. Looking the column up at logging time would miss conditions and raw paths.

## 69. Errors carry their SQL

`exec.Error` holds the statement, with placeholders and no values, so `ant.FailedSQL` can log it. It stays out of the message unless `WithDetailedErrors` is set, because messages are what reach users.

## 70. DryRun really runs

`ant.DryRun` runs your code in a transaction that rolls back, rather than faking results. A simulated run cannot know what a trigger or a constraint does. The cost is stated in its documentation: sequences advance, and anything outside the database still happens.

## 71. Dependencies stay out of the root package

The root package uses the standard library. Code that needs more goes elsewhere: `pgxext` for pgx, and separate modules for OpenTelemetry (`contrib/otel`), the analyzer (`tools/antvet`), and the benchmarks (`tools/compare`). `Batch` left the root package for this reason, as the roadmap's `ant.Batch` needed pgx. `dialect/mysql` imports its driver, as the error numbers are not reachable any other way, and only a program using MySQL compiles it.

## 72. Partitions are not migrations

Migrations create a partitioned parent, and `postgres.MonthlyPartitions` manages its months. Partitions come and go on a schedule, which a file history cannot describe, so `drift` ignores them.

## 73. Drift compares what models declare

`drift` reports enums and sequences only if a model declares them, so a type created by hand, or by an extension, is not reported for removal. Tables work the other way, because a table no model describes is what drift exists to find.

## 74. Dialects state their differences

A dialect's `Syntax` lists how its SQL differs from PostgreSQL's, and the zero value changes nothing, so PostgreSQL's output cannot move when another dialect is added. Differences are data, in one place, rather than a renderer per database.

A feature a database lacks is reported as `ErrUnsupported` before sending anything. Where the meaning can be kept exactly, ant emulates it instead: `FILTER` becomes a `CASE` inside the aggregate, and `ILIKE` a `LIKE` between lowered values. Where it cannot, ant refuses. It does not emulate `FULL JOIN` on MySQL, or upserts on SQL Server, because those would quietly change concurrency behaviour.

Error codes are translated to SQLSTATE values through `dialect.RegisterErrors`, so `IsUniqueViolation` takes no dialect.

## 75. Generated values without RETURNING

MySQL gets keys from `LastInsertId`, which reports one key per statement, so rows with generated keys are written singly there. `ON DUPLICATE KEY` returns the old row even when it changed nothing, so on MySQL and MariaDB, `Upsert` decides from the row count, and reads the row back by the conflict's columns.

## 76. One clock for times without a zone

SQLite compares times as text, and SQL Server's `DATETIME2` drops the zone, so a local time stored there sits hours from a UTC one and compares wrongly with no error. Those dialects bind every time in UTC.

## 77. Layout is enforced by tools

The owner wants code with room in it: a field per line, a blank line before a `return`, and an `if` kept apart from a statement it does not check. Reviewing that by hand does not last, so `make fmt` does it, and `make lint` fails on anything it would change.

`golines` breaks long lines at 100 columns, and `wsl_v5` and `nlreturn` handle blank lines. `tools/antfmt` covers what they leave: they only look at line length, so a short line of fields passes, and `wsl_v5` lets an `if` sit under a statement it has nothing to do with. antfmt only adds line breaks and trailing commas, so it cannot change what code means. It needs no dependencies, so it stays a small module beside `antvet`.

A literal with four or more keyed fields gets one per line, because three short ones read fine on a line, and test tables would grow a lot if they did not. Positional rows stay on one line for the same reason. Long strings and SQL stay whole.


## 78. A replaced column takes its dependents along, and they come back

PostgreSQL 17 can change a generated column's expression in place, with `SET EXPRESSION`, but ant supports 13 and later, and a migration is written without knowing which server will run it. So a new expression, or a column that becomes generated, is a `DROP COLUMN` and an `ADD COLUMN`. `AlterColumn.Replaces` says when, and the PostgreSQL dialect follows it exactly.

The drop takes indexes, unique and check constraints, the primary key, and the comment with it, and is refused while a foreign key in another table points at the column. A snapshot still lists all of these, so `check` passed while the database had lost them, and only `drift` would have found it. The plan now drops each of them before the column and makes them again after. An object counts as using the column if it names it in its columns, its included columns, an expression, or a predicate. Expressions are matched as text, by whole identifier, in double quotes, or spelled in Unicode escapes such as `U&"sl\0075g"`, and erring toward a match costs only a rebuild that was not needed. That is also why an escaped name is decoded with every character in it as the escape, rather than the one `UESCAPE` picks: comments may stand between the two, and a wrong guess can only find a match too many.

A foreign key depends on the unique key it points at, and PostgreSQL will not drop that key while the foreign key exists. Which key a foreign key chose is only known to the server, so whenever a plan drops a primary key, unique constraint, or unique index on the same columns, in any order, the foreign key is dropped first and made again last. That covers a unique index rebuilt because a replaced column is one of its included columns, and one changed for any other reason.

A plan that replaces a column builds and drops every index without `CONCURRENTLY`, not only those on the column. A single online statement takes the transaction away from the whole file, and half a migration could leave the column replaced with its indexes and constraints gone, which is the failure this decision is about. For indexes on the replaced column nothing is lost, since the replacement holds an exclusive lock on the table until the end anyway. An index elsewhere does block writes while it builds, so the file warns about each one that asked to be concurrent, and says to move it to a migration of its own. Splitting the file automatically was considered and rejected: moving online statements later breaks a foreign key that needs a new unique index, and a later `DROP INDEX` can find the index already gone with its column.

A column that stops being generated is not replaced: `DROP EXPRESSION` (PostgreSQL 13) keeps its values and everything on it. Before this, it was dropped and added empty, which lost the data, or failed on a `NOT NULL` column with rows.

## 79. A tracking context serves one operation at a time

A tracker hands every load the same instance of a row, and `Include` writes the related rows into it. Two requests sharing one tracking context therefore wrote into the same entities at once: each reset Alice's posts and appended its own, so both saw the posts twice, and two writes to the same entry's map of loaded relations crashed the process. Locks around ant's own writes cannot fix this, because the request that loaded Alice reads her posts after its load returns, while the next request writes them.

So a tracking context is one unit of work, as EF Core's `DbContext` is. An operation that loads into the tracker or saves from it claims the tracker, and another operation that starts meanwhile gets `ErrConcurrentUse` rather than waiting, because waiting would hide the mistake behind slow requests. The claimed operations are tracked reads, including `Find` from the identity map, and `SaveChanges`. `Insert`, `Upsert`, `DeleteByID`, `Load`, and `Reload` claim whenever the tracker holds anything: with tracking off, `Add` and `Attach` still fill it, and the entity they write may be in it. Untracked work on an empty tracker never claims, so a context opened with `WithTracking(false)` is safe to share, and `ant.Session(db, ant.WithTracking(true))` is the unit of work, as decision 29 says.

A hook runs inside the operation that called it, and its queries share the claim through the `context.Context` the hook receives. The context carries a token for that one operation, under a key for that tracker, so a hook that goes from one context to another and back keeps both claims, and a context kept after its operation ended claims nothing. Goroutines started inside an operation with its context share the claim, so they must not use the tracker at once. A query made with another context, such as `context.Background()`, looks like a second operation and is refused: Go has no goroutine identity to tell the two apart, and the error says what to do. `Chunk` claims each batch separately, so its callback may query. Helpers without a context, such as `HasChanges` and `Entry`, do not claim; they read the tracker under its lock.

A rollback reverses what `SaveChanges` recorded in the tracker, which writes to the same entries an operation would, so it needs the claim too. It cannot be refused, and it must not wait either: the operation holding the tracker may itself be waiting for the connection the rolling-back transaction holds. So the reversal runs at once if its caller holds the tracker or the tracker is free, and otherwise when the holding operation lets go, or when the next operation claims it, whichever comes first. Each recorded change names its tracker, because a session made inside a transaction has a tracker of its own. A late reversal can find its entry saved again by the operation it waited for, such as a second insert of the same entity. That newer write stands, and an entry it left unchanged becomes `Modified`, since neither write knew what the other left in the database: the next save writes every column, which is right either way. Each entry counts the writes saved to it to tell, and reversals run newest first, each putting the count back, so one save's several changes reverse together. Within its own transaction a rollback reverses everything, newest first, reads included: a `Reload` records its own reversal, which puts back the values, state, and snapshot it replaced, since the rows it read go with the rollback, and the relations' bookkeeping always comes back as the save found it, since a load inside the transaction read rows the rollback takes back, and one outside read the committed rows it restores. So a `Reload` or `Load` between a save and its rollback leaves an insert pending again, and a removal pending. A removal asked for after a save outlives its rollback: an entity that was new is forgotten, as its row never stayed, and a stored one stays `Deleted`, so the next save deletes the row the rollback brought back. Releasing a claim lets go of the tracker even if queued work panics, so a bug there cannot leave the tracker held for good.

`Include` also builds each parent's relation apart and sets it in one write, so a parent never shows half its rows, and drops repeated parents, which `Load(ctx, &U.Posts, alice, alice)` used to give every post twice.
