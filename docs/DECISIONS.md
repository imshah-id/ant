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

The guard of decision 23 counts a derived source as filtered when its rows are. `From`, `FromCTE`, and `Recursive` take their source's, a union needs both sides filtered, an intersection either, and `EXCEPT` its first, since a second side that matches nothing leaves every row. Replacing the source replaces what it counts for, while the outer query's own conditions still count. SQL Server allows `WITH` only at the start of a statement, so a write after `FromCTE` or `Recursive` returns `ErrUnsupported` there. MySQL refuses a write whose subquery reads the table being written (error 1093), but it materializes a derived table there rather than merging it, so a derived source needs nothing more. Other reads of the table are decision 85's.

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

## 80. Every write moves the version on

An upsert that updated copied every column from the row it tried to insert, the version included, and `Insert` starts a version at 1. So the stored version went back to 1, and a copy loaded at version 1, two saves earlier, matched again and saved over both. `DoUpdate` and `Set` left the version where it was, which hid the upsert from every copy loaded before it. `Merge` did the same, and `InsertMany` with a conflict shares the upsert's clause.

Now each of them moves the stored version on by one when it updates a row, as `SaveChanges` and bulk `Update` do, and a row `Merge` inserts starts at 1 unless the source gives a version. The version is ant's to keep, so naming it in `DoUpdate`, `Set`, or a `Merge` assignment is an error before anything runs.

An entity passed to `Upsert` with a version of its own is a loaded copy, so its update applies only while the stored row still has that version, and otherwise `Upsert` returns a `ConcurrencyError`, as `SaveChanges` would. A zero version is a new entity, with nothing to check. `ON DUPLICATE KEY UPDATE` takes no condition, so on MySQL and MariaDB the version moves on but the copy's update goes ahead. `InsertMany` cannot tell a row's own version from the 1 it stamps on a new one, so it moves versions on without checking them.

## 81. A set operation's members keep their own paging

`Union` built its statement from the left query and appended the right one, so the left query's `ORDER BY`, `LIMIT`, and `OFFSET` were written after the right member, where SQL applies them to the whole set. The youngest two users united with the oldest came back as the youngest of all three, a `Delete` on such a set removed rows outside it, and an ordering by a table's column failed on PostgreSQL, which orders a set only by its output columns. SQLite writes members without parentheses, so there the right member's paging escaped too, and SQL Server and SQLite refuse an ordered member outright.

Each member is now built on its own. A paged member becomes a derived table, `SELECT * FROM (... LIMIT 2) AS users`, which every database reads as one member. A member that orders without paging loses its order, since order cannot change which rows a set holds. `Recursive` takes its starting rows the same way. `TestConformanceSetMembers` checks every pair of six member shapes under each of the four operations, on all five databases.

## 82. A rename must land on a name the models have

`--rename` is there to keep data, but a table rename never checked its new name. `--rename users=peple` renamed the table to the typo, created `people` empty from the models, and dropped `peple` as a table the models no longer had, with every row in it. Column renames checked the models, but not the snapshot, so a column renamed onto one that already existed planned a statement the database refuses.

Now a rename's new name must be a table or column the models have, in the table's own schema, and one the snapshot does not have yet, which also catches two renames onto one name and a rename to the same name. Otherwise `diff` stops before writing anything, and the error lists the tables or columns that are new in the models, since a typo is usually one of them. A rename into another schema is refused for the same reason: the plan would move nothing, and drop the table.

## 83. antvet judges only what it can see

`antvet` flagged valid code in ant's own tests, so nobody could run it on a project and trust the result. Three things caused it. Models were named with `types.TypeString`, which since Go 1.23 keeps an alias's own name, so a query of `type User = fixture.User` was "a query of User" that a probe of `fixture.User` did not match. `Find` keys were checked against a field named `ID`, though `Configure` can make another field the key. And a condition on another model was reported in any query, though a subquery may name its outer query's model, as decision 37 allows.

Now models are compared as types, with `types.Identical`, which sees through aliases at any depth, so `Row[[4]Key]` with `type Key = int64` is `Row[[4]int64]`, and which tells two models of one name apart, such as a type declared again in an inner block. Reports and facts name a model with its aliases resolved, and reports leave packages out, as in `Row[Code]`. Calls through a dot import are recognized like any other. A model's `Configure` method is read for `Key`, `GenerateKey`, `CompositeKey`, and `NoKey`, if it is one ant calls, which takes one `*ModelBuilder` and nothing else, while a `Configure` the model gets from a struct it embeds, which ant finds by reflection too, leaves its key unknown, and unchecked, and the key it sets, which an alias of the model has too, and which a `Find` key is judged against by `Find`'s type argument, given or inferred, as ant judges it, so `Find[any]` is reported, travels to other packages as an analysis fact, the way probes already did, with its whole path, so `Key(&C.Identity.Code)` is not taken for a top-level `Code`. Only a key set by one statement that always runs counts, chained calls such as `.NotGenerated()` included, and written as a method expression too, `(*ant.ModelBuilder[C]).Key(b, &C.Code)`, whose receiver comes first: one set in a branch or a closure, after a `return` or past a `goto` that may skip it, set twice, or possibly set by whatever the builder is handed to, even under another name or as a method value such as `setKey := b.Key[string]`, is unknown, and `Find` is not checked against it. A query is judged only where it plainly is not a subquery: when its chain ends in a call that runs it, leaving ant's builder types, so `GroupBy` and `Join` carry it on, or in a function that runs it, such as `ant.SumOf` or `ant.Pluck`, or when it sits in the arguments of outer queries whose models it may also name, the outermost of which runs. A function such as `ant.Project`, `ant.Merge`, or `ant.JoinSub` makes the query part of what it builds, a grouped query given to `ant.Project` included, so the chain goes on from there, and a method given its type arguments, as `OrderBy[string]` is, is followed past them. A join reads all its models, up to the four of `Join4`, and is an outer query to its subqueries. A join function's first query may name the model it joins, as a condition before a `Join` method may, and a lateral join's subquery may name the query it joins, while `JoinSub`'s subquery stands alone. A subquery in a join's conditions may name either side, and is left alone. Only a function given the query itself runs it or builds on it: one given a condition made of it, as `ant.Exists` makes, uses it as a subquery. Parentheses around a receiver change nothing, nor does a type of one's own that embeds an ant query or table, whose methods are the builder's and are judged as its, and neither does a method expression, which takes the receiver as its first argument and goes on along its chain, as `(*ant.Query[Post]).All(q, ctx)` runs `q`, though a query in one of its other arguments is not taken as a subquery of it. The outer queries end at a statement, except that a query a function literal returns is the query of the call the literal is given to: `When` and `Scope` hand back their own query, with its scope, while an `Include` or `IncludeOne` shape is a query of the related model that ant runs apart, so nothing outside the shape is in its scope, as ant itself refuses. A model whose type argument holds a type parameter, inside an interface too, is left alone until an instantiation fills it in. So a subquery built apart and passed in, inside an `Include` shape too, one inside a grouped query or a relation's scope, such as `Has`, whose model the analyzer does not follow, and one whose outermost query does not run where the analyzer looks, are left alone, since only data flow could say which query they end up in.

A variable holds a probe if everything ever assigned to it is one, however that is done: declared with `var` in a function, assigned with `=`, as a package variable set in `init` is, which other packages learn of as a fact, or copied from another such variable, wherever in the package that comes, and even when copies go round, as `q := p` and then `p = q` do, as long as a probe from `ant.Fields` or another package reaches them. A declaration with no value does not count. A variable that is ever given anything else, an ordinary pointer or one of the values of a call that returns several, may hold that at any use, since the analyzer does not follow the order code runs in, so it counts as an ordinary variable throughout. So does one whose value the analyzer cannot see: a parameter or a receiver, which the caller gives, a range variable, and a variable whose address is taken, through which anything may be stored. A field behind a pointer held in a struct field, as in `&holder.U.Age`, or behind what a call returns may be a probe's, so it is not judged at all. Conditions that `And`, `Or`, and `Not` combine are judged with the query they are given to, and so are `Past`, `Future`, `Today`, `InSub`, and `NotInSub`, which take one of its columns. `IncludeJoined` is not a join here: it reads its row under an alias of its own, which conditions cannot name, so ant refuses them. A model that holds a type parameter is known only where its function is instantiated, so its queries and `Find` keys are left alone. A `Find` key passes as ant's own check passes it: the key's type, any integer for an integer, or a type of the same kind that converts to it, such as `[16]byte` for a named UUID type, and a type argument written out, as in `Find[string]`, is checked too.

Conditions are not the only place a field of another model fails. The fields that ordering and grouping take are judged as conditions are, since a subquery may order by its outer query's columns, and those that `Column`, `Omit`, `With`, the include and join methods, an update's assignments, and the aggregates such as `ant.SumOf` take, by the query's own models alone. `JoinUsing` reads only the column's name, which both sides have, so it may take either's. `AnyOf`, `AllOf`, and `NoneOf` have each of their fields judged. Every field pointer that ant's functions and its builders' methods take must be a probe's, `ant.Into`, `ant.EqCol`, the relation functions, and `ant.Original` included, while a function that takes an entity, such as `ant.Add`, takes any pointer.

Code that is wrong on purpose, as in tests of ant's errors, carries `//antvet:ignore` and a reason, at the end of its line or on a line of its own above it. One at the end of a line covers only that line, so it cannot hide a report on the next. `make lint` builds the analyzer and runs it on ant, so a new false report fails the build where it starts.

## 84. Every time is bound in UTC where columns keep no zone

SQL Server's `DATETIME2` and SQLite's text keep a time's wall clock and drop its zone, so ant binds times in UTC there, as decision 76 says. Only a plain `time.Time` was converted, though. A `*time.Time`, `sql.Null[time.Time]`, `sql.NullTime`, `ant.DeletedAt`, a sensitive field's wrapped value, and every argument of a raw query went in with the application's zone, so a nullable time stored three hours off in a zone three hours from UTC, and a condition on it found nothing.

The renderer and the raw-query binder now share one conversion. It sees through pointers, as many as database/sql would follow, and interfaces anywhere along them, `qb.Secret` and a pointer to one, which stay secrets, as a secret inside a `Valuer` that gives a time does once the `Valuer` is asked, and `sql.NullTime` and an `sql.Null` of a time, of a pointer to one, of a `Valuer` that gives one, or of an interface that holds one when it is bound, and structs built on them, such as `ant.DeletedAt`, and it leaves NULL as NULL. Raw arguments are converted after an `IN` list expands, so each element is. Other `driver.Valuer` types are left alone rather than asked for their value: a JSON or encrypted value would be serialized twice, once to look for a time and once by database/sql. A type built on a time is asked once, and its value, or its error, goes on in its place, so one that overrides `Value` is not asked twice either. A `Valuer` of one's own that gives a time is the application's to give in UTC. A pointer chain that comes back to itself binds an error rather than being followed for ever. `TestConformanceTimeShapes` stores and finds each shape in a zone three hours from UTC on all five databases, and reads the stored clock back through database/sql, past ant.

## 85. MySQL reads the table it writes through a derived table

MySQL refuses an `UPDATE` or `DELETE` whose subquery reads the table being written, with error 1093, at any depth: `IN` a subquery of the same table, `EXISTS` through another table's subquery that reads it, a value in `SET`, a join or nested condition inside the key subquery of decision 38. It allows the read inside a derived table, which it materializes before the write. MariaDB allows all of them.

So on MySQL, while an `UPDATE` or `DELETE` renders its values and conditions, the renderer watches each subquery for the written table, by name in any case, in the same schema, and by name in raw SQL, which it cannot read: as a whole word or a quoted name, while a string literal or a comment that holds it does not count. Raw SQL is split into literals and comments as MySQL splits it, with `#` and `-- ` comments and `/*! */` read as SQL, past a version of five or six digits that may run straight into it, and in every SQL mode, since the renderer cannot know the server's: a backslash escapes a quote unless `NO_BACKSLASH_ESCAPES` is set, and `ANSI_QUOTES` makes double quotes a name. A name that any mode reads as the table counts, because a needless wrapper costs a derived table and a missed one fails the write. A subquery that reads it, through a table or a source named like it, is wrapped once, as a whole: `SELECT /*+ NO_MERGE(materialized) */ * FROM (...) AS materialized`. A derived table needs unique column names. MySQL makes one up for a column with none, from its SQL, so a single column is always unique, and so are several whose names the renderer can tell apart. Otherwise, where two columns share a name, or one of several is an expression or raw SQL, the derived table names its columns with a list, `AS materialized(ant_c1, ant_c2)`, which `IN`, `EXISTS`, and a value do not mind, as they read by position. The subquery itself is left as it was either way, so what it refers to by name, as `ORDER BY` and `HAVING` can, keeps its meaning: an earlier version renamed columns inside the subquery instead, and every name it gave could be one the query read. A raw expression in the select list is one column, and one that holds several fails with MySQL's error 1353, as the list does not match. A star in `EXISTS` becomes `1` when nothing reads its columns: no grouping, ordering, offset, or set, while `DISTINCT` cannot change whether a row exists. A star over one table stays, with the table's own names and no list, since its columns cannot be counted. A star that has to stay beside other columns, or over a join or a set, may name a column twice, which no derived table takes, so a write whose subquery plainly reads the table returns `ErrUnsupported`. Only a read the renderer is sure of refuses a write, though: a table of exactly the written table's name and schema, or with no schema on either side, outside any derived table. A name in raw SQL, a source name, which may be a CTE's, a name that differs only in case, which is another table unless `lower_case_table_names` folds them, a table where only one side gives a schema, which is the same table only if the other is in that schema, and a read inside a derived table of the subquery, which MySQL materializes before the write, may need the wrapper or not, so such a subquery is wrapped where it can be, and otherwise left as it was, for MySQL to judge. A table in another schema than the written table's is another table. In raw SQL any spelling of the table counts, whatever schema qualifies it, since raw SQL is not parsed, and a wrapper the subquery does not need costs only a temporary table. The subquery is written twice, once to find the read and once inside the wrapper, so the `Lazy` conditions it holds, such as global filters, are recorded the first time and replayed, and each is built once. The column list needs MySQL 8.0.19, and a correlated subquery, which the derived table refers to the outer row from, needs 8.0.14. ant is tested on 8.4, and `TestConformanceWritesReadingTheirTable`, which runs each shape, the column lists included, and checks the rows it changed, passes on 8.0.46 too. MySQL 8.0 reached its end of life in April 2026. Before those versions such a write failed with error 1093 anyway, so no write that worked stops working. A read already inside a derived table of the subquery is wrapped too: MySQL's manual says the optimizer may merge such a derived table into the outer query, which brings error 1093 back unless its materialization is forced, while 8.4 does not merge it, so the wrapper costs a temporary table at most, and needs no column list when the subquery's names are its own. The hint keeps the optimizer from merging the wrapper back, whatever `derived_merge` says. A subquery that reads only other tables stays as it was, since wrapping it would cost a temporary table for nothing, and a `LIMIT`ed subquery is a derived table already. Raw SQL directly in the `WHERE`, outside any subquery, is the application's own.

`ON DUPLICATE KEY UPDATE` refuses a read of its own table even through a derived table, so an upsert whose `Set` reads the table still fails on MySQL; only `INSERT ... SELECT` would accept it.

## 86. Combinations are tested against an oracle

Most bugs found lately came from one feature meeting another: a bulk write on a derived source, a paged member of a union, a nullable time on a database without zones, a relation load inside a transaction that rolls back. Hand-written tests check features one at a time, so the combinations went unchecked.

`tests/matrix` builds queries from a grid of dimensions and checks every point against an oracle: the same combination worked out in Go, on the same seeded rows, with SQL's three-valued logic. It runs on every database, about 40,000 checks each, in a minute on PostgreSQL. Checking rows, not SQL, is what finds wrong results; SQL snapshots per dialect add a review of every change to generated SQL, with no database needed. A known failure is listed with its issue and skips, and an entry whose combinations all pass fails the run, so the list tracks what is still broken. The grid grows by adding a point to a dimension, with its meaning in Go beside it.

## 87. An embedded field must be a struct value

`ant:"embedded"` on a pointer made opening a context panic, deep in reflection. Supporting one would need its columns nullable, and a rule for when columns that are all NULL mean a nil pointer rather than a zero struct, so ant refuses it instead, with an error that names the field, as it does a scalar, or a struct that holds such a field. The model keeps that error, and every use of it returns it: opening a context, a query, and a raw query or a projection that scans into the struct, which would otherwise read the fields ant did walk and leave those after the pointer empty.
