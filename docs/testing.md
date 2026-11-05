# Testing with ant

Test against a real database. ant has no in-memory fake on purpose: a fake accepts SQL that the real server refuses, and hides what constraints, triggers, and transactions do. PostgreSQL in a container starts in seconds.

## A database for tests

```yaml
# docker-compose.yml
services:
  db:
    image: postgres:17
    environment: { POSTGRES_USER: app, POSTGRES_PASSWORD: app, POSTGRES_DB: app }
    ports: ["127.0.0.1:55432:5432"]
```

Build the schema from your real migrations, so a broken migration fails a test before it reaches production. A schema per test package lets packages run in parallel:

```go
func TestMain(m *testing.M) {
    ctx := context.Background()
    db, err := ant.Open[models.AppDB](ctx, postgres.Dialect, os.Getenv("TEST_DATABASE_URL"))
    if err != nil {
        log.Fatal(err)
    }
    if err := migrate.Up(ctx, db, migrate.Config{FS: migrations.Files}); err != nil {
        log.Fatal(err)
    }
    shared = db
    os.Exit(m.Run())
}
```

For quick experiments, `db.EnsureCreated(ctx)` builds tables straight from the structs.

Skip when no database is set, so `go test ./...` works anywhere:

```go
if os.Getenv("TEST_DATABASE_URL") == "" {
    t.Skip("TEST_DATABASE_URL is not set")
}
```

## Isolating tests

`anttest.Isolate` gives a test a context bound to a transaction that rolls back when the test ends, so tests can share a database and need no cleanup.

```go
func TestSignup(t *testing.T) {
    db := anttest.Isolate(t, shared)
    // ...
}
```

A test of concurrency needs a schema of its own instead, because other connections cannot see an open transaction.

## Rows for a test

**Factories** make valid entities, so a test states only what it cares about.

```go
var Users = anttest.NewFactory(func(n int) *models.User {
    return &models.User{Name: fmt.Sprintf("user %d", n), Email: fmt.Sprintf("u%d@example.com", n)}
})

admin := Users.With(func(u *models.User) { u.Admin = true })  // a state

user := Users.Create(t, &db.Users)                            // in the database
draft := Users.Build()                                        // in memory
many := Users.CreateMany(t, &db.Users, 10)

Users.Create(t, &db.Users, func(u *models.User) { u.Age = 5 }) // one override
```

A sequence number keeps values unique. `AfterBuild` and `AfterCreate` add callbacks. `anttest.Recycle` shares one parent among many children.

**Fixtures** load rows from JSON files named after their tables, for data that does not change:

```go
//go:embed testdata
var data embed.FS

anttest.Load(t, db, data, "testdata/users.json", "testdata/posts.json")
```

Rows go in as written, with no hooks, so parents come first.

## SQLite

SQLite is tempting for speed, but it has no `FOR UPDATE`, no enum types, different date handling, and looser typing, so a test can pass there and fail in production. Test on the database you deploy. Where SQLite is what you deploy, ant's dialect runs queries and writes, and migrations are not there yet, so create the tables with your own SQL.

## Checking queries

`q.ToSQL()` returns a query's SQL with no database, which suits a test of a query builder. For anything else, run it: what matters is what the database returns.

`ant.DryRun` runs real code in a transaction, and returns every statement, which shows what a unit of work decides to write.

`antvet` catches mistakes before tests run:

```sh
go install github.com/skudra-go/ant/tools/antvet/cmd/antvet@latest
go vet -vettool=$(which antvet) ./...
```

It reports field pointers that are not probes, probes of the wrong model, and `Find` keys of the wrong type, including a key that `Configure` sets with `Key` or `GenerateKey`. A subquery's conditions may name its outer query's model, so a query is judged only where it runs, as in `.All(ctx)`, or inside the query it belongs to. Code that is wrong on purpose, such as a test of the error, says so at the end of its line, or on a line of its own above it:

```go
_, err := db.Users.Find(ctx, "1") //antvet:ignore the test checks ErrKeyType
```
