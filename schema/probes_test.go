package schema_test

// Exploratory probes of migration planning (schema.Diff, schema.Script) against
// the PostgreSQL DDL dialect. Every scenario:
//
//  1. plans old -> new, and checks the plan without a database (Script splits
//     back into the planned statements, Diff does not touch its inputs, Diff(new,
//     new) is empty, snapshots round trip);
//  2. with ANT_TEST_DATABASES=postgres, creates old in a scratch PostgreSQL schema,
//     applies the Script the way the migrator would (Split, one transaction unless
//     the plan is online), then compares the result with a fresh create of new and
//     with a drift check (Inspect against Canonical, as `migrate drift` does).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/skudra-go/ant/internal/testdb"
	"github.com/skudra-go/ant/schema"
)

// ---------------------------------------------------------------- builders

func prReq(name, typ string) *schema.Column { return &schema.Column{Name: name, Type: typ} }

func prOpt(name, typ string) *schema.Column {
	return &schema.Column{Name: name, Type: typ, Nullable: true}
}

func prDef(name, typ, def string) *schema.Column {
	return &schema.Column{Name: name, Type: typ, Default: def}
}

func prIdent(name string) *schema.Column {
	return &schema.Column{Name: name, Type: "bigint", Identity: true}
}

func prGen(name, typ, expr string) *schema.Column {
	return &schema.Column{Name: name, Type: typ, Generated: expr}
}

func prTab(name string, cols ...*schema.Column) *schema.Table {
	return &schema.Table{Name: name, Columns: cols}
}

func prIn(schemaName string, t *schema.Table) *schema.Table {
	t.Schema = schemaName

	return t
}

func prPK(t *schema.Table, cols ...string) *schema.Table {
	t.PrimaryKey = &schema.Key{Name: t.Name + "_pkey", Columns: cols}

	return t
}

func prUniq(t *schema.Table, name string, cols ...string) *schema.Table {
	t.Uniques = append(t.Uniques, &schema.Key{Name: name, Columns: cols})

	return t
}

func prCheck(t *schema.Table, name, expr string) *schema.Table {
	t.Checks = append(t.Checks, &schema.Check{Name: name, Expr: expr})

	return t
}

func prIdx(t *schema.Table, name string, cols ...string) *schema.Table {
	i := &schema.Index{Name: name}

	for _, c := range cols {
		i.Parts = append(i.Parts, schema.IndexPart{Column: c})
	}

	t.Indexes = append(t.Indexes, i)

	return t
}

func prIndex(t *schema.Table, i *schema.Index) *schema.Table {
	t.Indexes = append(t.Indexes, i)

	return t
}

func prFK(
	t *schema.Table,
	name string,
	cols []string,
	refSchema, refTable string,
	refCols []string,
	onDelete string,
) *schema.Table {
	t.ForeignKeys = append(t.ForeignKeys, &schema.ForeignKey{
		Name:       name,
		Columns:    cols,
		RefSchema:  refSchema,
		RefTable:   refTable,
		RefColumns: refCols,
		OnDelete:   onDelete,
	})

	return t
}

func prComment(t *schema.Table, text string) *schema.Table {
	t.Comment = text

	return t
}

func prCols(names ...string) []string { return names }

// prWorld builds a schema and lists the named schemas its tables use.
func prWorld(tables ...*schema.Table) *schema.Schema {
	s := &schema.Schema{Tables: tables}

	for _, t := range tables {
		if t.Schema != "" && !slices.Contains(s.Schemas, t.Schema) {
			s.Schemas = append(s.Schemas, t.Schema)
		}
	}

	return s
}

// ---------------------------------------------------------------- sandbox

type prSandbox struct {
	d     schema.Dialect
	admin *sql.DB
	db    *sql.DB
	main  string
}

func prNewSandbox(t *testing.T, p string) *prSandbox {
	t.Helper()

	dsn := ""

	for _, target := range testdb.Targets() {
		if target.Name == "postgres" {
			dsn = target.DSN
		}
	}

	if dsn == "" {
		return nil // DB-less checks only
	}

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() { admin.Close() })

	main := p + "main"
	if _, err := admin.Exec(`CREATE SCHEMA "` + main + `"`); err != nil {
		t.Fatalf("create scratch schema: %v", err)
	}

	// Each cleanup is set as soon as there is something to clean, so a failure
	// below leaves nothing behind. They run newest first: the scratch pool
	// closes before its schemas go, and admin closes last.
	t.Cleanup(func() { prDropSchemas(admin, main, p) })

	// The DSN may be a URL, with a query or not, or keyword=value pairs, so
	// it is parsed rather than added to.
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the PostgreSQL DSN: %v", err)
	}

	config.RuntimeParams["search_path"] = main
	config.DefaultQueryExecMode = pgx.QueryExecModeExec

	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	return &prSandbox{
		d:     ddl(t),
		admin: admin,
		db:    db,
		main:  main,
	}
}

// prDropSchemas drops the scratch schema and every schema a probe with prefix p
// made.
func prDropSchemas(admin *sql.DB, main, p string) {
	admin.Exec(`DROP SCHEMA IF EXISTS "` + main + `" CASCADE`)

	rows, err := admin.Query(`SELECT nspname FROM pg_namespace WHERE nspname LIKE $1`, p+"%")
	if err != nil {
		return
	}

	var names []string

	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}

	rows.Close()

	for _, n := range names {
		admin.Exec(`DROP SCHEMA IF EXISTS "` + n + `" CASCADE`)
	}
}

func prUnique(t *testing.T) string {
	t.Helper()

	return "pr" + strings.ToLower(rand.Text()[:10]) + "_"
}

// wipe removes the scratch schema's contents and the named schemas. It takes
// the running test rather than the one that made the sandbox, because each
// direction of a probe is a subtest and may only stop itself.
func (sb *prSandbox) wipe(t *testing.T, named []string) {
	t.Helper()

	for _, n := range named {
		if _, err := sb.admin.Exec(`DROP SCHEMA IF EXISTS "` + n + `" CASCADE`); err != nil {
			t.Fatalf("wipe %s: %v", n, err)
		}
	}

	for _, q := range []string{
		`DROP SCHEMA IF EXISTS "` + sb.main + `" CASCADE`,
		`CREATE SCHEMA "` + sb.main + `"`,
	} {
		if _, err := sb.admin.Exec(q); err != nil {
			t.Fatalf("wipe: %v", err)
		}
	}
}

// exec runs a script the way the migrator does: split into statements, in one
// transaction unless the plan has statements that cannot run in one.
func (sb *prSandbox) exec(plan *schema.Plan, script string) error {
	ctx := context.Background()
	statements := sb.d.Split(script)

	if plan != nil && plan.Online() {
		for _, s := range statements {
			if _, err := sb.db.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("%w\n  while running: %s", err, s)
			}
		}

		return nil
	}

	tx, err := sb.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, s := range statements {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%w\n  while running: %s", err, s)
		}
	}

	return tx.Commit()
}

func (sb *prSandbox) create(s *schema.Schema) error {
	plan, err := schema.Diff(nil, s)
	if err != nil {
		return err
	}

	schema.Backfill(sb.d, plan)

	script, err := schema.Script(sb.d, plan)
	if err != nil {
		return err
	}

	return sb.exec(plan, script)
}

func (sb *prSandbox) inspect(names []string) (*schema.Schema, error) {
	return sb.d.Inspect(context.Background(), sb.db, names)
}

// drift mirrors `migrate drift`: the live database against the models as the
// server itself would store them.
func (sb *prSandbox) drift(to *schema.Schema) (*schema.Plan, error) {
	ctx := context.Background()

	conn, err := sb.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	canonical, err := sb.d.Canonical(ctx, conn, to)
	if err != nil {
		return nil, err
	}

	live, err := sb.d.Inspect(ctx, conn, to.Schemas)
	if err != nil {
		return nil, err
	}

	prReconcile(sb.d, live, canonical)

	return schema.Diff(live, canonical)
}

// prReconcile is migrate's reconcile, copied.
func prReconcile(d schema.Dialect, live, expected *schema.Schema) {
	live.Enums = slices.DeleteFunc(
		live.Enums,
		func(e *schema.Enum) bool { return expected.Enum(e.Name) == nil },
	)

	live.Sequences = slices.DeleteFunc(
		live.Sequences,
		func(q *schema.Sequence) bool { return expected.Sequence(q.Name) == nil },
	)

	same := func(have *string, want string) {
		if *have != want && d.SameExpression(*have, want) {
			*have = want
		}
	}

	for _, t := range expected.Tables {
		other := live.Table(t.Ref())
		if other == nil {
			continue
		}

		for _, c := range t.Columns {
			if column := other.Column(c.Name); column != nil {
				same(&column.Default, c.Default)
				same(&column.Generated, c.Generated)
			}
		}

		for _, check := range t.Checks {
			for _, have := range other.Checks {
				if have.Name == check.Name {
					same(&have.Expr, check.Expr)
				}
			}
		}

		for _, index := range t.Indexes {
			for _, have := range other.Indexes {
				if have.Name != index.Name || len(have.Parts) != len(index.Parts) {
					continue
				}

				same(&have.Where, index.Where)

				for i := range index.Parts {
					same(&have.Parts[i].Expr, index.Parts[i].Expr)
				}
			}
		}
	}
}

// prDescribe flattens an inspected schema to comparable pieces. Column order
// does not matter, because ADD COLUMN always appends.
func prDescribe(s *schema.Schema) map[string]string {
	c := s.Clone()
	for _, t := range c.Tables {
		slices.SortFunc(t.Columns, func(a, b *schema.Column) int {
			return strings.Compare(a.Name, b.Name)
		})
	}

	c.Sort()

	out := map[string]string{}

	for _, t := range c.Tables {
		b, _ := json.Marshal(t)
		out["table "+t.Ref().String()] = string(b)
	}

	for _, e := range c.Enums {
		out["enum "+e.Name] = strings.Join(e.Values, ",")
	}

	for _, q := range c.Sequences {
		b, _ := json.Marshal(q)
		out["sequence "+q.Name] = string(b)
	}

	out["schemas"] = strings.Join(c.Schemas, ",")

	return out
}

func prCompare(migrated, fresh *schema.Schema) string {
	a, b := prDescribe(migrated), prDescribe(fresh)

	keys := map[string]bool{}

	for k := range a {
		keys[k] = true
	}

	for k := range b {
		keys[k] = true
	}

	var sorted []string

	for k := range keys {
		sorted = append(sorted, k)
	}

	slices.Sort(sorted)

	var out strings.Builder

	for _, k := range sorted {
		if a[k] != b[k] {
			fmt.Fprintf(&out, "  %s\n    migrated: %s\n    fresh:    %s\n", k, a[k], b[k])
		}
	}

	return out.String()
}

func prNames(ss ...*schema.Schema) []string {
	var out []string

	for _, s := range ss {
		for _, n := range s.Schemas {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}

	return out
}

func prEncode(s *schema.Schema) string {
	b, err := s.Encode()
	if err != nil {
		return "ERR " + err.Error()
	}

	return string(b)
}

var prLeadingComments = regexp.MustCompile(`^(\s*--[^\n]*\n)+`)

func prCode(statement string) string {
	return strings.TrimSpace(prLeadingComments.ReplaceAllString(statement+"\n", ""))
}

type prCheckQuery struct{ query, want string }

type probe struct {
	name        string
	build       func(p string) (old, new *schema.Schema)
	renames     []string
	seed        []string // SQL run on the old schema; {p} is the unique prefix
	verify      []prCheckQuery
	forbid      []string // regexps that must not match any planned statement
	back        bool     // also migrate new -> old
	backRenames []string
	// noDB limits the probe to planning checks.
	noDB bool
	// refuse is a regexp for the error Diff must refuse the forward plan with,
	// for a plan ant should not make. The probe then checks nothing else.
	refuse string
}

func prFill(p string, in []string) []string {
	out := make([]string, len(in))

	for i, s := range in {
		out[i] = strings.ReplaceAll(s, "{p}", p)
	}

	return out
}

// planChecks is everything that needs no database.
func planChecks(
	t *testing.T,
	d schema.Dialect,
	from, to *schema.Schema,
	renames, forbid []string,
) (*schema.Plan, string, bool) {
	t.Helper()

	beforeFrom, beforeTo := prEncode(from), prEncode(to)

	plan, err := schema.Diff(from, to, renames...)
	if err != nil {
		t.Errorf("Diff returned an error: %v", err)

		return nil, "", false
	}

	schema.Backfill(d, plan)

	script, err := schema.Script(d, plan)
	if err != nil {
		t.Errorf("Script: %v", err)

		return nil, "", false
	}

	statements, err := schema.Statements(d, plan)
	if err != nil {
		t.Errorf("Statements: %v", err)

		return nil, "", false
	}

	if prEncode(from) != beforeFrom {
		t.Errorf("Diff changed its from schema")
	}

	if prEncode(to) != beforeTo {
		t.Errorf("Diff changed its to schema")
	}

	var split []string

	for _, s := range d.Split(script) {
		split = append(split, prCode(s))
	}

	var planned []string

	for _, s := range statements {
		planned = append(planned, strings.TrimSpace(s))
	}

	if !slices.Equal(split, planned) {
		t.Errorf(
			"Split(Script) does not give back the planned statements:\n  planned: %q\n  split:   %q",
			planned,
			split,
		)
	}

	for _, pattern := range forbid {
		re := regexp.MustCompile(pattern)

		for _, s := range statements {
			if re.MatchString(s) {
				t.Errorf("wasteful/forbidden statement (%s): %s", pattern, s)
			}
		}
	}

	if again, err := schema.Diff(to, to); err != nil || !again.Empty() {
		t.Errorf("Diff(new, new) is not empty: %v %v", err, again)
	}

	// A stored snapshot of the models plans the same as the models.
	for _, s := range []*schema.Schema{from, to} {
		round, err := schema.Decode([]byte(prEncode(s)))
		if err != nil {
			t.Errorf("snapshot does not decode: %v", err)

			continue
		}

		if p, err := schema.Diff(round, s); err != nil || !p.Empty() {
			t.Errorf("snapshot round trip is a change: %v %v", err, p)
		}
	}

	// Every hint must be one a person can take up.
	for _, h := range plan.Hints {
		if _, err := schema.Diff(from, to, append(slices.Clone(renames), h.Flag())...); err != nil {
			t.Errorf("hint %q cannot be taken up: %v", h.Flag(), err)
		}
	}

	return plan, script, true
}

func prMigrate(
	t *testing.T,
	sb *prSandbox,
	p string,
	from, to *schema.Schema,
	renames, seed, forbid []string,
	verify []prCheckQuery,
) {
	t.Helper()

	d := ddl(t)

	plan, script, ok := planChecks(t, d, from, to, renames, forbid)
	if !ok || sb == nil {
		return
	}

	named := prNames(from, to)

	sb.wipe(t, named)

	if err := sb.create(from); err != nil {
		t.Errorf("[old schema does not create] %v", err)

		return
	}

	for _, s := range seed {
		if _, err := sb.db.Exec(s); err != nil {
			t.Fatalf("[seed] %v: %s", err, s)
		}
	}

	if err := sb.exec(plan, script); err != nil {
		statements, _ := schema.Statements(d, plan)
		t.Errorf(
			"migration failed: %v\n  plan:\n    %s",
			err,
			strings.Join(statements, ";\n    "),
		)

		return
	}

	for _, v := range verify {
		var got string
		if err := sb.db.QueryRow(strings.ReplaceAll(v.query, "{p}", p)).
			Scan(&got); err != nil ||
			got != v.want {
			t.Errorf("verify %q: got %q (%v), want %q", v.query, got, err, v.want)
		}
	}

	migrated, err := sb.inspect(named)
	if err != nil {
		t.Errorf("inspect after migration: %v", err)

		return
	}

	if drift, err := sb.drift(to); err != nil {
		t.Errorf("drift check: %v", err)
	} else if !drift.Empty() {
		var what []string

		for _, c := range drift.Changes {
			what = append(what, c.String())
		}

		sql, _ := schema.Statements(d, drift)
		t.Errorf(
			"drift after migration (live vs models): %s\n  fix would be: %s",
			strings.Join(what, "; "),
			strings.Join(sql, "; "),
		)
	}

	sb.wipe(t, named)

	if err := sb.create(to); err != nil {
		t.Errorf("[new schema does not create from scratch] %v", err)

		return
	}

	fresh, err := sb.inspect(named)
	if err != nil {
		t.Errorf("inspect fresh: %v", err)

		return
	}

	if diff := prCompare(migrated, fresh); diff != "" {
		statements, _ := schema.Statements(d, plan)
		t.Errorf(
			"migrated database differs from a fresh create of the new schema:\n%s  plan was:\n    %s",
			diff,
			strings.Join(statements, ";\n    "),
		)
	}
}

// knownScenarios maps the start of a scenario's name, and /back for its way
// back, to the issue it fails for.
var knownScenarios = map[string]string{
	"01_":      "#29",
	"04_":      "#29",
	"06_":      "#29",
	"07_":      "#29",
	"29_":      "#29",
	"29_/back": "#29",
	"63_":      "#29",
	"02_/back": "#29",
	"13_":      "#30",
	"14_":      "#30",
	"16_":      "#30",
	"50_":      "#30",
	"61_":      "#30",
	"26_":      "#31",
	"27_":      "#31",
	"28_":      "#31",
	"09_":      "#32",
	"12b_":     "#32",
	"20_":      "#32",
	"21_":      "#32",
	"43_":      "#32",
	"56_":      "#32",
	"48_":      "#33",
	"48_/back": "#33",
	"54_":      "#34",
	"25_":      "#35",
	"33_":      "#36",
	"45_":      "#36",
	"40_":      "#36",
}

// knownScenario skips a scenario that fails for a tracked issue.
func knownScenario(t *testing.T, name string, back bool) {
	t.Helper()

	for prefix, issue := range knownScenarios {
		key, onBack := strings.CutSuffix(prefix, "/back")
		if onBack == back && strings.HasPrefix(name, key) {
			knownProbe(t, issue)
		}
	}
}

// knownProbe skips a probe that fails for a tracked issue, unless
// ANT_RUN_KNOWN is set, as it is when working on that issue.
func knownProbe(t *testing.T, issue string) {
	t.Helper()

	if os.Getenv("ANT_RUN_KNOWN") == "" {
		t.Skipf("known failure, %s; ANT_RUN_KNOWN=1 runs it", issue)
	}
}

// prRefused checks that Diff refuses to plan with the renames, with an error
// that matches want.
func prRefused(t *testing.T, from, to *schema.Schema, renames []string, want string) {
	t.Helper()

	_, err := schema.Diff(from, to, renames...)
	if err == nil {
		t.Errorf("Diff planned with %q, want a refusal matching %q", renames, want)

		return
	}

	if !regexp.MustCompile(want).MatchString(err.Error()) {
		t.Errorf("Diff refused with %q, want one matching %q", err, want)
	}
}

func runProbe(t *testing.T, c probe) {
	p := prUnique(t)
	old, next := c.build(p)

	var sb *prSandbox

	if !c.noDB && c.refuse == "" {
		sb = prNewSandbox(t, p)
	}

	// Each direction is its own subtest, so a known failure skips only its own
	// direction: prMigrate builds the old schema afresh every time.
	t.Run("forward", func(t *testing.T) {
		knownScenario(t, c.name, false)

		if c.refuse != "" {
			prRefused(t, old, next, prFill(p, c.renames), c.refuse)

			return
		}

		prMigrate(t, sb, p, old, next, prFill(p, c.renames), prFill(p, c.seed), c.forbid, c.verify)
	})

	if c.back {
		t.Run("back", func(t *testing.T) {
			knownScenario(t, c.name, true)
			prMigrate(t, sb, p, next, old, prFill(p, c.backRenames), nil, nil, nil)
		})
	}
}

// ---------------------------------------------------------------- scenarios

func TestProbeScenarios(t *testing.T) {
	for _, c := range probes {
		t.Run(c.name, func(t *testing.T) { runProbe(t, c) })
	}
}

var probes = []probe{
	{
		// text default, to integer: PostgreSQL cannot cast the old default, so it
		// must go before the type changes.
		name: "01_type_notnull_default_text_to_integer",
		seed: []string{"INSERT INTO t (id, qty) VALUES (1, '12'), (2, '7')"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(
				prTab("t",
					prReq("id", "bigint"),
					&schema.Column{
						Name:     "qty",
						Type:     "text",
						Nullable: true,
						Default:  "'0'",
					},
				),
				"id",
			))

			next := prWorld(prPK(
				prTab("t",
					prReq("id", "bigint"),
					prDef("qty", "integer", "5"),
				),
				"id",
			))

			return old, next
		},
		back: true,
	},
	{
		// control: the old default casts to the new type on its own.
		name: "02_type_default_integer_to_text",
		seed: []string{"INSERT INTO t (id, qty) VALUES (1, 12), (2, 7)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(prTab("t", prReq("id", "bigint"), prDef("qty", "integer", "0")), "id"),
			)

			next := prWorld(
				prPK(prTab("t", prReq("id", "bigint"), prDef("qty", "text", "''")), "id"),
			)

			return old, next
		},
		back: true,
	},
	{
		name: "03_nullable_bigint_to_numeric_notnull_default",
		seed: []string{"INSERT INTO t (id, amount) VALUES (1, 100), (2, 7)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prOpt("amount", "bigint")), "id"))

			next := prWorld(prPK(
				prTab("t",
					prReq("id", "bigint"),
					prDef("amount", "numeric(12,2)", "0"),
				),
				"id",
			))

			return old, next
		},
		back: true,
	},
	{
		// text column with a default becomes an enum with a default.
		name: "04_text_default_to_enum_default",
		seed: []string{"INSERT INTO t (id, status) VALUES (1, 'ok'), (2, 'sad')"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(prTab("t", prReq("id", "bigint"), prDef("status", "text", "'ok'")), "id"),
			)

			next := prWorld(
				prPK(prTab("t", prReq("id", "bigint"), prDef("status", "mood", "'ok'")), "id"),
			)
			next.Enums = []*schema.Enum{{Name: "mood", Values: []string{"sad", "ok", "happy"}}}

			return old, next
		},
	},
	{
		name: "05_text_no_default_to_enum",
		seed: []string{"INSERT INTO t (id, status) VALUES (1, 'ok'), (2, 'sad')"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("status", "text")), "id"))
			next := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("status", "mood")), "id"))
			next.Enums = []*schema.Enum{{Name: "mood", Values: []string{"sad", "ok", "happy"}}}

			return old, next
		},
		back: true,
	},
	{
		// identity to a plain column with a default (and back)
		name: "06_identity_to_plain_with_default",
		seed: []string{"INSERT INTO t (id) VALUES (1), (2)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prIdent("n")), "id"))

			next := prWorld(
				prPK(prTab("t", prReq("id", "bigint"), prDef("n", "bigint", "0")), "id"),
			)

			return old, next
		},
		back: true,
	},
	{
		// a plain nullable column becomes identity, which needs NOT NULL first
		name: "07_plain_nullable_to_identity",
		seed: []string{"INSERT INTO t (id, n) VALUES (1, 1), (2, 2)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prOpt("n", "bigint")), "id"))
			next := prWorld(prPK(prTab("t", prReq("id", "bigint"), prIdent("n")), "id"))

			return old, next
		},
		back: true,
	},
	{
		// identity primary key to plain, and a column type change at once
		name: "08_identity_pk_to_plain_integer",
		seed: []string{"INSERT INTO t (name) VALUES ('a'), ('b')"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prIdent("id"), prReq("name", "text")), "id"))
			next := prWorld(prPK(prTab("t", prReq("id", "integer"), prReq("name", "text")), "id"))

			return old, next
		},
		back: true,
	},
	{
		// rename columns used by an expression index, a partial index, a check,
		// a unique constraint and an INCLUDE list
		name: "09_rename_column_used_by_index_expr_partial_check",
		seed: []string{
			"INSERT INTO users (id, email, age, name) VALUES (1, 'A@x', 20, 'n1'), (2, 'b@x', 3, 'n2')",
		},
		renames: []string{
			"users.email=mail",
			"users.age=years",
		},
		forbid: []string{`(?i)DROP (INDEX|CONSTRAINT)`},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(email, age string) *schema.Schema {
				t := prPK(
					prTab("users",
						prReq("id", "bigint"),
						prReq(email, "text"),
						prReq(age, "integer"),
						prReq("name", "text"),
					),
					"id",
				)
				prUniq(t, "users_email_key", email)
				prCheck(t, "users_age_check", age+" >= 0")

				prIndex(t, &schema.Index{
					Name:  "users_email_lower_idx",
					Parts: []schema.IndexPart{{Expr: "lower(" + email + ")"}},
				})

				prIndex(t, &schema.Index{
					Name:  "users_adult_idx",
					Parts: []schema.IndexPart{{Column: "name"}},
					Where: age + " >= 18",
				})

				prIndex(t, &schema.Index{
					Name:  "users_cover_idx",
					Parts: []schema.IndexPart{{Column: age, Desc: true}, {Column: email}},
				})

				return prWorld(t)
			}

			return mk("email", "age"), mk("mail", "years")
		},
		back: true,
		backRenames: []string{
			"users.mail=email",
			"users.years=age",
		},
	},
	{
		// rename a referenced column and a referencing column of a composite FK
		name: "10_rename_columns_of_composite_foreign_key",
		seed: []string{
			"INSERT INTO accounts (tenant_id, id) VALUES (1, 1), (1, 2)",
			"INSERT INTO orders (id, tenant_id, account_id) VALUES (1, 1, 2)",
		},
		renames: []string{
			"accounts.id=acc_id",
			"accounts.tenant_id=org_id",
			"orders.account_id=acc_ref",
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(org, acc, ref string) *schema.Schema {
				accounts := prPK(
					prTab("accounts", prReq(org, "bigint"), prReq(acc, "bigint")),
					org,
					acc,
				)

				orders := prPK(
					prTab(
						"orders",
						prReq("id", "bigint"),
						prReq("tenant_id", "bigint"),
						prReq(ref, "bigint"),
					),
					"id",
				)
				prFK(
					orders,
					"orders_account_fkey",
					prCols("tenant_id", ref),
					"",
					"accounts",
					prCols(org, acc),
					"CASCADE",
				)

				prIdx(orders, "orders_account_idx", "tenant_id", ref)

				return prWorld(accounts, orders)
			}

			return mk("tenant_id", "id", "account_id"), mk("org_id", "acc_id", "acc_ref")
		},
		back: true,
		backRenames: []string{
			"accounts.acc_id=id",
			"accounts.org_id=tenant_id",
			"orders.acc_ref=account_id",
		},
	},
	{
		// rename a table that composite FKs from two tables and itself refer to,
		// with every constraint name following the table
		name: "11_rename_table_with_composite_fks_and_names_following",
		seed: []string{
			"INSERT INTO accounts (tenant_id, id, parent_id) VALUES (1, 1, NULL), (1, 2, 1)",
			"INSERT INTO orders (id, tenant_id, account_id) VALUES (1, 1, 2)",
			"INSERT INTO invoices (id, tenant_id, account_id) VALUES (1, 1, 1)",
		},
		renames: []string{"accounts=people"},
		verify:  []prCheckQuery{{"SELECT count(*)::text FROM people", "2"}},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(name string) *schema.Schema {
				accounts := prPK(
					prTab(
						name,
						prReq("tenant_id", "bigint"),
						prReq("id", "bigint"),
						prOpt("parent_id", "bigint"),
					),
					"tenant_id",
					"id",
				)
				prUniq(accounts, name+"_id_key", "tenant_id", "id", "parent_id")

				prFK(
					accounts,
					name+"_parent_fkey",
					prCols("tenant_id", "parent_id"),
					"",
					name,
					prCols("tenant_id", "id"),
					"SET NULL",
				)

				prIdx(accounts, name+"_parent_idx", "parent_id")

				orders := prPK(
					prTab(
						"orders",
						prReq("id", "bigint"),
						prReq("tenant_id", "bigint"),
						prReq("account_id", "bigint"),
					),
					"id",
				)
				prFK(
					orders,
					"orders_account_fkey",
					prCols("tenant_id", "account_id"),
					"",
					name,
					prCols("tenant_id", "id"),
					"CASCADE",
				)

				invoices := prPK(
					prTab(
						"invoices",
						prReq("id", "bigint"),
						prReq("tenant_id", "bigint"),
						prReq("account_id", "bigint"),
					),
					"id",
				)
				prFK(
					invoices,
					"invoices_account_fkey",
					prCols("tenant_id", "account_id"),
					"",
					name,
					prCols("tenant_id", "id"),
					"",
				)

				return prWorld(accounts, orders, invoices)
			}

			return mk("accounts"), mk("people")
		},
		back:        true,
		backRenames: []string{"people=accounts"},
	},
	{
		// table rename first, then a column of the renamed table
		name:    "12a_rename_table_then_its_column",
		renames: []string{"accounts=people", "people.name=title"},
		seed:    []string{"INSERT INTO accounts (id, name) VALUES (1, 'x')"},
		verify:  []prCheckQuery{{"SELECT title FROM people", "x"}},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(prTab("accounts", prReq("id", "bigint"), prReq("name", "text")), "id"),
			)

			next := prWorld(
				prPK(prTab("people", prReq("id", "bigint"), prReq("title", "text")), "id"),
			)

			return old, next
		},
	},
	{
		// the same two renames, column first, as written against the old schema
		name:    "12b_rename_column_then_its_table",
		renames: []string{"accounts.name=title", "accounts=people"},
		seed:    []string{"INSERT INTO accounts (id, name) VALUES (1, 'x')"},
		verify:  []prCheckQuery{{"SELECT title FROM people", "x"}},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(prTab("accounts", prReq("id", "bigint"), prReq("name", "text")), "id"),
			)

			next := prWorld(
				prPK(prTab("people", prReq("id", "bigint"), prReq("title", "text")), "id"),
			)

			return old, next
		},
	},
	{
		// drop a column and the generated column that is computed from it
		name: "13_drop_base_column_and_its_generated_column",
		seed: []string{"INSERT INTO calc (id, price, qty) VALUES (1, 2, 3)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(
				prTab(
					"calc",
					prReq("id", "bigint"),
					prReq("price", "integer"),
					prReq("qty", "integer"),
					prGen("total", "integer", "price * qty"),
				),
				"id",
			))

			next := prWorld(
				prPK(prTab("calc", prReq("id", "bigint"), prReq("qty", "integer")), "id"),
			)

			return old, next
		},
	},
	{
		// drop a column, and re-define the generated column that used it
		name: "14_drop_base_column_and_redefine_generated",
		seed: []string{"INSERT INTO calc (id, price, qty) VALUES (1, 2, 3)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(
				prTab(
					"calc",
					prReq("id", "bigint"),
					prReq("price", "integer"),
					prReq("qty", "integer"),
					prGen("total", "integer", "price * qty"),
				),
				"id",
			))

			next := prWorld(prPK(
				prTab(
					"calc",
					prReq("id", "bigint"),
					prReq("qty", "integer"),
					prGen("total", "integer", "qty * 2"),
				),
				"id",
			))

			return old, next
		},
	},
	{
		// change a generated expression: the column is dropped and added, which also
		// drops its index, check and comment
		name: "15_generated_expression_change_keeps_index_check_comment",
		seed: []string{"INSERT INTO calc (id, a, b) VALUES (1, 2, 3)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(expr string) *schema.Schema {
				total := prGen("total", "integer", expr)
				total.Comment = "the total"

				t := prPK(
					prTab(
						"calc",
						prReq("id", "bigint"),
						prReq("a", "integer"),
						prReq("b", "integer"),
						total,
					),
					"id",
				)
				prIdx(t, "calc_total_idx", "total")
				prCheck(t, "calc_total_check", "total >= 0")
				prUniq(t, "calc_total_key", "total")

				return prWorld(t)
			}

			return mk("a + b"), mk("a * b")
		},
	},
	{
		// rename the base column of a generated column that has an index and a comment
		name:    "16_rename_base_of_generated_column",
		renames: []string{"calc.a=x"},
		seed:    []string{"INSERT INTO calc (id, a, b) VALUES (1, 2, 3)"},
		forbid:  []string{`(?i)DROP COLUMN`},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(a string) *schema.Schema {
				total := prGen("total", "integer", a+" + b")
				total.Comment = "the total"

				t := prPK(
					prTab(
						"calc",
						prReq("id", "bigint"),
						prReq(a, "integer"),
						prReq("b", "integer"),
						total,
					),
					"id",
				)
				prIdx(t, "calc_total_idx", "total")

				return prWorld(t)
			}

			return mk("a"), mk("x")
		},
		back:        true,
		backRenames: []string{"calc.x=a"},
	},
	{
		// the primary key changes shape while another table's FK, which stays the
		// same, depends on it
		name: "17_primary_key_change_with_unchanged_referencing_fk",
		seed: []string{
			"INSERT INTO users (id, region) VALUES (1, 'eu')",
			"INSERT INTO posts (id, user_id) VALUES (1, 1)",
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			posts := prPK(prTab("posts", prReq("id", "bigint"), prReq("user_id", "bigint")), "id")
			prFK(
				posts,
				"posts_user_id_fkey",
				prCols("user_id"),
				"",
				"users",
				prCols("id"),
				"CASCADE",
			)

			old := prWorld(
				prPK(prTab("users", prReq("id", "bigint"), prReq("region", "text")), "id"),
				posts,
			)

			posts2 := prPK(prTab("posts", prReq("id", "bigint"), prReq("user_id", "bigint")), "id")
			prFK(
				posts2,
				"posts_user_id_fkey",
				prCols("user_id"),
				"",
				"users",
				prCols("id"),
				"CASCADE",
			)

			users2 := prPK(
				prTab("users", prReq("id", "bigint"), prReq("region", "text")),
				"region",
				"id",
			)
			prUniq(users2, "users_id_key", "id")

			return old, prWorld(users2, posts2)
		},
		back: true,
	},
	{
		// control: primary keys that are added, removed and reshaped, with nothing
		// referring to them
		name: "18_primary_key_lifecycle_without_references",
		seed: []string{
			"INSERT INTO a (id, r) VALUES (1, 1)",
			"INSERT INTO b (id, r) VALUES (1, 1)",
			"INSERT INTO c (id, r) VALUES (1, 1)",
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(prTab("a", prReq("id", "bigint"), prReq("r", "bigint")), "id"),
				prPK(prTab("b", prReq("id", "bigint"), prReq("r", "bigint")), "id"),
				prTab("c", prReq("id", "bigint"), prReq("r", "bigint")),
			)

			next := prWorld(
				prPK(prTab("a", prReq("id", "bigint"), prReq("r", "bigint")), "id", "r"),
				prTab("b", prReq("id", "bigint"), prReq("r", "bigint")),
				prPK(prTab("c", prReq("id", "bigint"), prReq("r", "bigint")), "id"),
			)

			return old, next
		},
		back: true,
	},
	{
		// composite primary key reordered while a composite FK refers to it
		name: "19_composite_primary_key_reorder_with_composite_fk",
		seed: []string{
			"INSERT INTO parent (a, b) VALUES (1, 2)",
			"INSERT INTO child (id, a, b) VALUES (1, 1, 2)",
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(order ...string) *schema.Schema {
				child := prPK(
					prTab(
						"child",
						prReq("id", "bigint"),
						prReq("a", "bigint"),
						prReq("b", "bigint"),
					),
					"id",
				)
				prFK(
					child,
					"child_parent_fkey",
					prCols("a", "b"),
					"",
					"parent",
					prCols("a", "b"),
					"",
				)

				return prWorld(
					prPK(prTab("parent", prReq("a", "bigint"), prReq("b", "bigint")), order...),
					child,
				)
			}

			return mk("a", "b"), mk("b", "a")
		},
		back: true,
	},
	{
		// swap two columns' names with renames: needs a temporary name
		name:    "20_swap_two_column_names_by_rename",
		renames: []string{"t.a=b", "t.b=a"},
		seed:    []string{"INSERT INTO t (id, a, b) VALUES (1, 'x', 5)"},
		verify:  []prCheckQuery{{"SELECT a::text FROM t", "5"}},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(
					prTab("t", prReq("id", "bigint"), prReq("a", "text"), prReq("b", "integer")),
					"id",
				),
			)

			next := prWorld(
				prPK(
					prTab("t", prReq("id", "bigint"), prReq("b", "text"), prReq("a", "integer")),
					"id",
				),
			)

			return old, next
		},
	},
	{
		// swap two tables' names with renames
		name:    "21_swap_two_table_names_by_rename",
		renames: []string{"a=b", "b=a"},
		seed:    []string{"INSERT INTO a (id, x) VALUES (1, 'x')"},
		verify:  []prCheckQuery{{"SELECT x FROM b", "x"}},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(prTab("a", prReq("id", "bigint"), prReq("x", "text")), "id"),
				prPK(prTab("b", prReq("id", "bigint"), prReq("y", "integer")), "id"),
			)

			next := prWorld(
				prPK(prTab("b", prReq("id", "bigint"), prReq("x", "text")), "id"),
				prPK(prTab("a", prReq("id", "bigint"), prReq("y", "integer")), "id"),
			)

			return old, next
		},
	},
	{
		// unique constraint to unique index of the same name, and back
		name: "22_unique_constraint_to_unique_index",
		seed: []string{"INSERT INTO users (id, email) VALUES (1, 'a'), (2, 'b')"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prUniq(
					prPK(prTab("users", prReq("id", "bigint"), prReq("email", "text")), "id"),
					"users_email_key",
					"email",
				),
			)

			t := prPK(prTab("users", prReq("id", "bigint"), prReq("email", "text")), "id")
			t.Indexes = []*schema.Index{
				{
					Name:   "users_email_key",
					Parts:  []schema.IndexPart{{Column: "email"}},
					Unique: true,
				},
			}

			return old, prWorld(t)
		},
		back: true,
	},
	{
		// the same, with a foreign key that refers to the unique constraint
		name: "23_unique_constraint_with_fk_to_unique_index",
		seed: []string{
			"INSERT INTO users (id, email) VALUES (1, 'a')",
			"INSERT INTO posts (id, author) VALUES (1, 'a')",
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			posts := func() *schema.Table {
				t := prPK(prTab("posts", prReq("id", "bigint"), prReq("author", "text")), "id")

				return prFK(
					t,
					"posts_author_fkey",
					prCols("author"),
					"",
					"users",
					prCols("email"),
					"",
				)
			}
			old := prWorld(
				prUniq(
					prPK(prTab("users", prReq("id", "bigint"), prReq("email", "text")), "id"),
					"users_email_key",
					"email",
				),
				posts(),
			)

			u := prPK(prTab("users", prReq("id", "bigint"), prReq("email", "text")), "id")
			u.Indexes = []*schema.Index{
				{
					Name:   "users_email_key",
					Parts:  []schema.IndexPart{{Column: "email"}},
					Unique: true,
				},
			}

			return old, prWorld(u, posts())
		},
		back: true,
	},
	{
		// INCLUDE + DESC + expression + partial + unique + method all at once
		name: "24_compound_index_include_desc_expr_partial",
		seed: []string{"INSERT INTO t (id, a, b, c) VALUES (1, 1, 'X', 5), (2, 2, 'Y', 6)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(i *schema.Index) *schema.Schema {
				t := prPK(
					prTab(
						"t",
						prReq("id", "bigint"),
						prReq("a", "integer"),
						prReq("b", "text"),
						prReq("c", "integer"),
					),
					"id",
				)

				return prWorld(prIndex(t, i))
			}
			old := mk(&schema.Index{Name: "t_idx", Parts: []schema.IndexPart{{Column: "a"}}})

			next := mk(&schema.Index{
				Name: "t_idx",
				Parts: []schema.IndexPart{
					{Column: "a", Desc: true},
					{Expr: "lower(b)", Desc: true},
				},
				Include: []string{"c"},
				Where:   "c > 0",
				Unique:  true,
			})

			return old, next
		},
		back: true,
	},
	{
		// NULLS FIRST has no field: the only way is an expression part
		name: "25_index_nulls_first_expression_part",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prOpt("a", "integer")), "id"))

			next := prWorld(prIndex(
				prPK(prTab("t", prReq("id", "bigint"), prOpt("a", "integer")), "id"),
				&schema.Index{
					Name:    "t_a_idx",
					Parts:   []schema.IndexPart{{Expr: "a NULLS FIRST"}},
					Include: []string{"id"},
				},
			))

			return old, next
		},
	},
	{
		// an enum value is added in the middle: PostgreSQL appends it
		name: "26_enum_value_added_in_the_middle",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("m", "mood")), "id"))
			old.Enums = []*schema.Enum{{Name: "mood", Values: []string{"sad", "happy"}}}
			next := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("m", "mood")), "id"))
			next.Enums = []*schema.Enum{{Name: "mood", Values: []string{"sad", "ok", "happy"}}}

			return old, next
		},
	},
	{
		// a new enum value used by the same migration: default, check, partial index
		name: "27_enum_value_added_and_used_in_same_migration",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("m", "mood")), "id"))
			old.Enums = []*schema.Enum{{Name: "mood", Values: []string{"sad", "happy"}}}

			t := prPK(
				prTab("t", prReq("id", "bigint"), prReq("m", "mood"), prDef("n", "mood", "'ok'")),
				"id",
			)
			prIndex(t, &schema.Index{
				Name:  "t_ok_idx",
				Parts: []schema.IndexPart{{Column: "id"}},
				Where: "m = 'ok'",
			})

			next := prWorld(t)
			next.Enums = []*schema.Enum{{Name: "mood", Values: []string{"sad", "happy", "ok"}}}

			return old, next
		},
	},
	{
		// a column moves from one enum to another
		name: "28_column_from_enum_a_to_enum_b",
		seed: []string{"INSERT INTO t (id, m) VALUES (1, 'a1')"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("m", "ea")), "id"))
			old.Enums = []*schema.Enum{{Name: "ea", Values: []string{"a1", "a2"}}}
			next := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("m", "eb")), "id"))
			next.Enums = []*schema.Enum{{Name: "eb", Values: []string{"a1", "b1"}}}

			return old, next
		},
	},
	{
		// control: a column leaves an enum for text, and the enum is dropped
		name: "29_enum_dropped_after_column_goes_to_text",
		seed: []string{"INSERT INTO t (id, m) VALUES (1, 'a1')"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prDef("m", "ea", "'a1'")), "id"))
			old.Enums = []*schema.Enum{{Name: "ea", Values: []string{"a1", "a2"}}}

			next := prWorld(
				prPK(prTab("t", prReq("id", "bigint"), prDef("m", "text", "'a1'")), "id"),
			)

			return old, next
		},
		back: true,
	},
	{
		// same table names in several schemas, with FKs across them
		name: "30_schema_qualified_tables_same_names_across_schemas",
		seed: []string{
			`INSERT INTO "{p}s1".users (id, name) VALUES (1, 'a')`,
			`INSERT INTO "{p}s2".users (id, email) VALUES (1, 'e')`,
			`INSERT INTO "{p}s1".posts (id, author_id) VALUES (1, 1)`,
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			s1, s2, s3 := p+"s1", p+"s2", p+"s3"

			mk := func(next bool) *schema.Schema {
				u1 := prPK(
					prIn(s1, prTab("users", prReq("id", "bigint"), prReq("name", "text"))),
					"id",
				)

				u2 := prPK(
					prIn(s2, prTab("users", prReq("id", "bigint"), prReq("email", "text"))),
					"id",
				)

				def := prPK(prTab("users", prReq("id", "bigint")), "id")

				p1 := prPK(
					prIn(s1, prTab("posts", prReq("id", "bigint"), prReq("author_id", "bigint"))),
					"id",
				)
				prFK(
					p1,
					"posts_author_fkey",
					prCols("author_id"),
					s2,
					"users",
					prCols("id"),
					"CASCADE",
				)

				prIdx(p1, "posts_author_idx", "author_id")

				p2 := prPK(
					prIn(s2, prTab("posts", prReq("id", "bigint"), prOpt("author_id", "bigint"))),
					"id",
				)
				prFK(
					p2,
					"posts_author_fkey",
					prCols("author_id"),
					s1,
					"users",
					prCols("id"),
					"SET NULL",
				)

				prIdx(p2, "posts_author_idx", "author_id")

				if !next {
					return prWorld(u1, u2, def, p1, p2)
				}

				u1.Columns = append(u1.Columns, prOpt("bio", "text"))
				u2.Columns = []*schema.Column{prReq("id", "bigint"), prReq("handle", "text")}

				prIdx(u1, "users_idx", "name")
				prIdx(u2, "users_idx", "handle")

				p1.ForeignKeys[0].OnDelete = "RESTRICT"

				c := prPK(
					prIn(s2, prTab("comments", prReq("id", "bigint"), prReq("post_id", "bigint"))),
					"id",
				)
				prFK(
					c,
					"comments_post_fkey",
					prCols("post_id"),
					s1,
					"posts",
					prCols("id"),
					"CASCADE",
				)

				u3 := prPK(
					prIn(s3, prTab("users", prReq("id", "bigint"), prReq("default_id", "bigint"))),
					"id",
				)
				prFK(u3, "users_default_fkey", prCols("default_id"), "", "users", prCols("id"), "")

				return prWorld(u1, u2, def, p1, c, u3)
			}

			return mk(false), mk(true)
		},
	},
	{
		// rename a table that shares its name with tables in other schemas
		name:    "31_rename_schema_qualified_table_with_namesakes",
		renames: []string{"{p}s1.users=members"},
		seed: []string{
			`INSERT INTO "{p}s1".users (id, name) VALUES (1, 'a')`,
			`INSERT INTO users (id, name) VALUES (1, 'a')`,
		},
		verify: []prCheckQuery{{`SELECT count(*)::text FROM "{p}s1".members`, "1"}},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			s1 := p + "s1"

			mk := func(name string) *schema.Schema {
				u := prPK(prIn(s1, prTab(name, prReq("id", "bigint"), prReq("name", "text"))), "id")
				prIdx(u, name+"_name_idx", "name")

				def := prPK(prTab("users", prReq("id", "bigint"), prReq("name", "text")), "id")
				prIdx(def, "users_name_idx", "name")

				posts := prPK(
					prIn(s1, prTab("posts", prReq("id", "bigint"), prReq("author_id", "bigint"))),
					"id",
				)
				prFK(posts, "posts_author_fkey", prCols("author_id"), s1, name, prCols("id"), "")

				dposts := prPK(
					prTab("posts", prReq("id", "bigint"), prReq("author_id", "bigint")),
					"id",
				)
				prFK(
					dposts,
					"posts_author_fkey",
					prCols("author_id"),
					"",
					"users",
					prCols("id"),
					"",
				)

				return prWorld(u, def, posts, dposts)
			}

			return mk("users"), mk("members")
		},
		back:        true,
		backRenames: []string{"{p}s1.members=users"},
	},
	{
		// identifiers that need quoting: upper case, spaces, double quotes, quotes
		name:    "32_identifiers_needing_quotes",
		renames: []string{`User Data.Full Name=Display Name`, `Other=Other "2"`},
		seed: []string{
			`INSERT INTO "User Data" ("Id", "Full Name", "we""ird", "select", m) VALUES (1, 'n', 1, 's', 'it''s')`,
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(full, other string, next bool) *schema.Schema {
				ud := prPK(
					prTab(
						"User Data",
						prReq("Id", "bigint"),
						prReq(full, "text"),
						prReq(`we"ird`, "integer"),
						prReq("select", "text"),
						prReq("m", `"Mood"`),
					),
					"Id",
				)
				ud.PrimaryKey.Name = `User "Data"_pkey`
				prUniq(ud, `UQ "x"`, full)

				prIndex(
					ud,
					&schema.Index{Name: `Idx "a"`, Parts: []schema.IndexPart{{Column: `we"ird`}}},
				)

				prIndex(
					ud,
					&schema.Index{
						Name:  `Idx lower`,
						Parts: []schema.IndexPart{{Expr: `lower("` + full + `")`}},
					},
				)

				prCheck(ud, `Chk`, `"we""ird" > 0`)
				ud.Comment = `it's a "table"`
				ud.Columns[1].Comment = `col with 'quotes' and "double"`

				o := prPK(prTab(other, prReq("Id", "bigint"), prReq("ref", "bigint")), "Id")
				o.PrimaryKey.Name = `Other pkey`
				prFK(o, `Other FK`, prCols("ref"), "", "User Data", prCols("Id"), "CASCADE")

				if next {
					ud.Columns = append(ud.Columns, prOpt(`New "col"`, "text"))

					ud.Columns = slices.DeleteFunc(ud.Columns, func(c *schema.Column) bool {
						return c.Name == "select"
					})
				}

				s := prWorld(ud, o)
				s.Enums = []*schema.Enum{{Name: "Mood", Values: []string{"it's", "ok"}}}

				return s
			}

			return mk("Full Name", "Other", false), mk("Display Name", `Other "2"`, true)
		},
	},
	{
		// a name with a dot cannot be written in a rename
		name:    "33_rename_names_containing_dots",
		renames: []string{"a.b=c", "c.x.y=z"},
		noDB:    true,
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("a.b", prReq("id", "bigint"), prReq("x.y", "text")), "id"))
			next := prWorld(prPK(prTab("c", prReq("id", "bigint"), prReq("z", "text")), "id"))

			return old, next
		},
	},
	{
		// control: one table dropped and another created with the same constraint,
		// index and primary key names
		name: "34_drop_table_create_other_with_same_object_names",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			a := prPK(prTab("a", prReq("id", "bigint"), prReq("x", "integer")), "id")
			prIdx(a, "shared_idx", "x")
			prUniq(a, "shared_key", "x")
			prFK(a, "shared_fkey", prCols("x"), "", "c", prCols("id"), "")

			c := prPK(prTab("c", prReq("id", "bigint")), "id")

			b := prPK(prTab("b", prReq("id", "bigint"), prReq("x", "integer")), "id")
			b.PrimaryKey.Name = "a_pkey"
			prIdx(b, "shared_idx", "x")
			prUniq(b, "shared_key", "x")
			prFK(b, "shared_fkey", prCols("x"), "", "c", prCols("id"), "")

			return prWorld(a, c), prWorld(b, c)
		},
	},
	{
		// a table is renamed away and a new table takes its old name
		name:    "35_rename_table_away_and_create_new_with_old_name",
		renames: []string{"a=b"},
		seed:    []string{"INSERT INTO a (id, x) VALUES (1, 1)"},
		verify: []prCheckQuery{
			{"SELECT count(*)::text FROM b", "1"},
			{"SELECT count(*)::text FROM a", "0"},
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			a := prPK(prTab("a", prIdent("id"), prReq("x", "integer")), "id")
			prIdx(a, "a_x_idx", "x")

			b := prPK(prTab("b", prIdent("id"), prReq("x", "integer")), "id")
			prIdx(b, "b_x_idx", "x")

			a2 := prPK(prTab("a", prIdent("id"), prReq("z", "text")), "id")
			prIdx(a2, "a_x_idx", "z")

			return prWorld(a), prWorld(b, a2)
		},
	},
	{
		// foreign key actions change, are added and removed
		name: "36_foreign_key_actions_change",
		seed: []string{"INSERT INTO u (id) VALUES (1)", "INSERT INTO c (id, uid) VALUES (1, 1)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(del, upd string) *schema.Schema {
				c := prPK(prTab("c", prReq("id", "bigint"), prOpt("uid", "bigint")), "id")
				prFK(c, "c_uid_fkey", prCols("uid"), "", "u", prCols("id"), del)
				c.ForeignKeys[0].OnUpdate = upd

				return prWorld(prPK(prTab("u", prReq("id", "bigint")), "id"), c)
			}

			return mk("CASCADE", ""), mk("SET NULL", "CASCADE")
		},
		back: true,
	},
	{
		// defaults: expression vs literal, awkward literals, new NOT NULL columns
		// with expression defaults on a table that has rows
		name: "38_defaults_expressions_and_awkward_literals",
		seed: []string{"INSERT INTO t (id, f) VALUES (1, 'z')"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(
				prTab("t",
					prReq("id", "bigint"),
					prDef("a", "text", "'x'"),
					prDef("b", "timestamptz", "now()"),
					prDef("c", "integer", "0"),
					prDef("d", "text", "'it''s'"),
					prDef("e", "uuid", "gen_random_uuid()"),
					prReq("f", "text"),
				),
				"id",
			))

			next := prWorld(prPK(
				prTab("t",
					prReq("id", "bigint"),
					prDef("a", "text", "'y'"),
					prDef("b", "timestamptz", "CURRENT_TIMESTAMP"),
					prDef("c", "integer", "1 + 1"),
					prDef("d", "text", "$q$a;b -- c$q$"),
					prReq("e", "uuid"),
					prDef("f", "text", "'a' || 'b'"),
					prDef("g", "text", "'semi;colon'"),
					prDef("h", "text", "';--''/*'"),
					prDef("i", "timestamptz", "now()"),
					prDef("j", "integer", "(2 * 3)"),
					prDef("k", "text[]", "ARRAY['a;b']::text[]"),
					prDef("l", "uuid", "gen_random_uuid()"),
				),
				"id",
			))

			return old, next
		},
		back: true,
	},
	{
		// a column name over 63 bytes
		name: "39_long_column_name_over_63_bytes",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			long := strings.Repeat("c", 70)
			old := prWorld(prPK(prTab("t", prReq("id", "bigint")), "id"))
			t := prPK(prTab("t", prReq("id", "bigint"), prOpt(long, "text")), "id")
			prIdx(t, "t_long_idx", long)

			return old, prWorld(t)
		},
	},
	{
		// two index names that PostgreSQL would cut to the same 63 bytes
		name: "40_two_index_names_with_same_63_byte_prefix",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			prefix := strings.Repeat("i", 63)
			old := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("a", "integer")), "id"))
			t := prPK(prTab("t", prReq("id", "bigint"), prReq("a", "integer")), "id")
			prIdx(t, prefix+"_one", "a")
			prIdx(t, prefix+"_two", "id", "a")

			return old, prWorld(t)
		},
	},
	{
		// comments: added, changed, removed, kept across a type change
		name: "42_comments_added_changed_removed_across_type_change",
		seed: []string{"INSERT INTO t (id, a, b, c) VALUES (1, 1, 'x', 2)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(next bool) *schema.Schema {
				a, b, c := prReq("a", "integer"), prReq("b", "text"), prReq("c", "integer")
				t := prPK(prTab("t", prReq("id", "bigint"), a, b, c), "id")

				if !next {
					prComment(t, "old table")

					a.Comment, b.Comment = "a comment", "b comment"

					return prWorld(t)
				}

				a.Type = "bigint"
				a.Comment = "a comment"
				b.Comment = ""
				c.Comment = "new c"
				t.Comment = ""

				return prWorld(t)
			}

			return mk(false), mk(true)
		},
		back: true,
	},
	{
		// an index name moves from one table to another
		name: "43_index_name_moves_between_tables",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(next bool) *schema.Schema {
				a := prPK(prTab("a", prReq("id", "bigint"), prReq("x", "integer")), "id")
				b := prPK(prTab("b", prReq("id", "bigint"), prReq("x", "integer")), "id")

				if !next {
					prIdx(a, "shared_idx", "x")
					prIdx(b, "b_old_idx", "x")
				} else {
					prIdx(b, "shared_idx", "x")
				}

				return prWorld(a, b)
			}

			return mk(false), mk(true)
		},
	},
	{
		// rename onto the name of a table that is dropped in the same plan: the
		// snapshot has that name, so the rename is refused (decision 82)
		name:    "44_rename_table_onto_existing_table_name",
		renames: []string{"users=people"},
		refuse:  `rename "users=people": the snapshot already has a table people`,
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(prTab("people", prReq("id", "bigint"), prReq("legacy", "text")), "id"),
				prPK(prTab("users", prReq("id", "bigint"), prReq("name", "text")), "id"),
			)

			next := prWorld(
				prPK(prTab("people", prReq("id", "bigint"), prReq("name", "text")), "id"),
			)

			return old, next
		},
	},
	{
		// a table moves to another schema
		name: "45_table_moves_between_schemas",
		noDB: true,
		build: func(p string) (*schema.Schema, *schema.Schema) {
			s1, s2 := p+"s1", p+"s2"
			old := prWorld(
				prPK(prIn(s1, prTab("t", prReq("id", "bigint"), prReq("x", "text"))), "id"),
			)

			next := prWorld(
				prPK(prIn(s2, prTab("t", prReq("id", "bigint"), prReq("x", "text"))), "id"),
			)

			return old, next
		},
	},
	{
		// everything new at once: schema, enum, sequence, tables, cross-schema FK,
		// partial index on an enum value of a type created in the same plan
		name: "46_everything_new_at_once",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			s1 := p + "s1"
			users := prPK(prTab("users", prReq("id", "bigint")), "id")
			old := prWorld(users)

			orders := prPK(
				prIn(s1, prTab("orders",
					prDef("id", "bigint", "nextval('order_seq')"),
					prReq("user_id", "bigint"),
					prDef("state", "state", "'new'"),
				)),
				"id",
			)
			orders.Columns[0].Identity = false

			prFK(
				orders,
				"orders_user_fkey",
				prCols("user_id"),
				"",
				"users",
				prCols("id"),
				"CASCADE",
			)

			prIndex(orders, &schema.Index{
				Name:  "orders_open_idx",
				Parts: []schema.IndexPart{{Column: "user_id"}},
				Where: "state = 'new'",
			})

			next := prWorld(prPK(prTab("users", prReq("id", "bigint")), "id"), orders)
			next.Enums = []*schema.Enum{{Name: "state", Values: []string{"new", "done"}}}
			next.Sequences = []*schema.Sequence{{Name: "order_seq", Start: 100, Increment: 5}}

			return old, next
		},
	},
	{
		// tables that refer to each other, dropped together, then created together
		name: "47_cyclic_foreign_keys_dropped_and_created",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func() *schema.Schema {
				a := prPK(prTab("a", prReq("id", "bigint"), prOpt("b_id", "bigint")), "id")
				b := prPK(prTab("b", prReq("id", "bigint"), prOpt("a_id", "bigint")), "id")

				prFK(a, "a_b_fkey", prCols("b_id"), "", "b", prCols("id"), "")
				prFK(b, "b_a_fkey", prCols("a_id"), "", "a", prCols("id"), "")

				return prWorld(a, b)
			}

			return mk(), prWorld()
		},
		back: true,
	},
	{
		// both sides of a foreign key change type in one plan
		name: "48_type_change_on_both_sides_of_foreign_key",
		seed: []string{
			"INSERT INTO users (id) VALUES (1)",
			"INSERT INTO posts (id, user_id) VALUES (1, 1)",
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(typ string) *schema.Schema {
				posts := prPK(prTab("posts", prReq("id", "bigint"), prReq("user_id", typ)), "id")
				prFK(
					posts,
					"posts_user_id_fkey",
					prCols("user_id"),
					"",
					"users",
					prCols("id"),
					"CASCADE",
				)

				return prWorld(prPK(prTab("users", prReq("id", typ)), "id"), posts)
			}

			return mk("bigint"), mk("text")
		},
		back: true,
	},
	{
		// control: integer to bigint on both sides stays compatible throughout
		name: "49_integer_to_bigint_on_both_sides_of_foreign_key",
		seed: []string{
			"INSERT INTO users (id) VALUES (1)",
			"INSERT INTO posts (id, user_id) VALUES (1, 1)",
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(typ string) *schema.Schema {
				posts := prPK(prTab("posts", prReq("id", "bigint"), prReq("user_id", typ)), "id")
				prFK(
					posts,
					"posts_user_id_fkey",
					prCols("user_id"),
					"",
					"users",
					prCols("id"),
					"CASCADE",
				)

				return prWorld(prPK(prTab("users", prReq("id", typ)), "id"), posts)
			}

			return mk("integer"), mk("bigint")
		},
		back: true,
	},
	{
		// the type of a column that a generated column uses
		name: "50_type_change_of_column_used_by_generated_column",
		seed: []string{"INSERT INTO t (id, a) VALUES (1, 2)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(typ string) *schema.Schema {
				return prWorld(prPK(
					prTab(
						"t",
						prReq("id", "bigint"),
						prReq("a", typ),
						prGen("b", "bigint", "a * 2"),
					),
					"id",
				))
			}

			return mk("integer"), mk("bigint")
		},
	},
	{
		// collation added and removed, on a column with a unique index
		name: "51_collation_added_and_removed",
		seed: []string{"INSERT INTO t (id, name) VALUES (1, 'a'), (2, 'B')"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(collation string) *schema.Schema {
				name := prReq("name", "text")
				name.Collation = collation
				t := prPK(prTab("t", prReq("id", "bigint"), name), "id")
				t.Indexes = []*schema.Index{
					{Name: "t_name_idx", Parts: []schema.IndexPart{{Column: "name"}}, Unique: true},
				}

				return prWorld(t)
			}

			return mk(""), mk("C")
		},
		back: true,
	},
	{
		// concurrent index on a populated table, together with a new column and a
		// replaced concurrent index
		name: "52_concurrent_index_with_other_changes",
		seed: []string{"INSERT INTO t (id, a, b) VALUES (1, 1, 2)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prIndex(
				prPK(
					prTab("t", prReq("id", "bigint"), prReq("a", "integer"), prReq("b", "integer")),
					"id",
				),
				&schema.Index{
					Name:       "t_a_idx",
					Parts:      []schema.IndexPart{{Column: "a"}},
					Concurrent: true,
				},
			))

			t := prPK(
				prTab(
					"t",
					prReq("id", "bigint"),
					prReq("a", "integer"),
					prReq("b", "integer"),
					prReq("c", "integer"),
				),
				"id",
			)
			prIndex(t, &schema.Index{
				Name:       "t_a_idx",
				Parts:      []schema.IndexPart{{Column: "a"}, {Column: "b"}},
				Concurrent: true,
				Unique:     true,
			})

			prIndex(
				t,
				&schema.Index{
					Name:       "t_c_idx",
					Parts:      []schema.IndexPart{{Column: "c"}},
					Concurrent: true,
				},
			)

			return old, prWorld(t)
		},
		back: true,
	},
	{
		// a populated table gets a nullable column with FK and index at once
		name: "53_new_column_with_foreign_key_and_index",
		seed: []string{"INSERT INTO t (id) VALUES (1)", "INSERT INTO u (id) VALUES (1)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			u := func() *schema.Table { return prPK(prTab("u", prReq("id", "bigint")), "id") }
			old := prWorld(prPK(prTab("t", prReq("id", "bigint")), "id"), u())
			t := prPK(prTab("t", prReq("id", "bigint"), prOpt("uid", "bigint")), "id")
			prFK(t, "t_uid_fkey", prCols("uid"), "", "u", prCols("id"), "SET NULL")
			prIdx(t, "t_uid_idx", "uid")

			return old, prWorld(t, u())
		},
		back: true,
	},
	{
		// an unmodelled newline in an identifier reaches a SQL comment in the Script
		name: "54_newline_in_identifier_reaches_script_comment",
		seed: nil,
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(prTab("t", prReq("id", "bigint"), prOpt("a\nSELECT 1/0", "text")), "id"),
			)

			next := prWorld(prPK(prTab("t", prReq("id", "bigint")), "id"))

			return old, next
		},
	},
	{
		// a unique constraint on a column that is renamed and has its type changed
		name:    "55_rename_and_retype_column_with_unique_and_fk",
		renames: []string{"t.ref=ref_id"},
		seed:    []string{"INSERT INTO u (id) VALUES (1)", "INSERT INTO t (id, ref) VALUES (1, 1)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(ref, typ string) *schema.Schema {
				t := prPK(prTab("t", prReq("id", "bigint"), prReq(ref, typ)), "id")
				prUniq(t, "t_ref_key", ref)
				prFK(t, "t_ref_fkey", prCols(ref), "", "u", prCols("id"), "")

				return prWorld(prPK(prTab("u", prReq("id", typ)), "id"), t)
			}

			return mk("ref", "bigint"), mk("ref_id", "integer")
		},
		back:        true,
		backRenames: []string{"t.ref_id=ref"},
	},
	{
		// two tables swap the names of their (identical) indexes
		name: "56_index_names_swapped_between_tables",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(a, b string) *schema.Schema {
				ta := prIdx(
					prPK(prTab("a", prReq("id", "bigint"), prReq("x", "integer")), "id"),
					a,
					"x",
				)

				tb := prIdx(
					prPK(prTab("b", prReq("id", "bigint"), prReq("x", "integer")), "id"),
					b,
					"x",
				)

				return prWorld(ta, tb)
			}

			return mk("ix1", "ix2"), mk("ix2", "ix1")
		},
	},
	{
		// primary key dropped with its column made nullable, and the reverse
		name: "57_drop_primary_key_and_allow_null",
		seed: []string{"INSERT INTO t (id) VALUES (1)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint")), "id"))
			next := prWorld(prTab("t", prOpt("id", "bigint")))

			return old, next
		},
		back: true,
	},
	{
		// a rename onto a column name that the same plan frees by dropping its
		// owner: the table has that column, so the rename is refused (decision 82)
		name:    "58_rename_column_onto_dropped_column_name",
		renames: []string{"t.a=b"},
		refuse:  `rename "t\.a=b": table t already has a column b`,
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(
				prPK(
					prTab("t", prReq("id", "bigint"), prReq("a", "text"), prReq("b", "text")),
					"id",
				),
			)

			next := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("b", "text")), "id"))

			return old, next
		},
	},
	{
		// awkward characters in comments, enum values, defaults, checks, predicates
		name: "59_awkward_characters_in_literals",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint")), "id"))
			v := prDef("v", "text", `'--;'`)
			v.Comment = "multi\nline; -- $$ \\ 'q' \"d\" /* c */"
			t := prPK(prTab("t", prReq("id", "bigint"), v, prDef("e", "weird", `'a;b'`)), "id")
			t.Comment = `x'; DROP TABLE t; -- $$ \`
			prCheck(t, "t_v_check", "v <> ';' AND v <> $q$;--$q$")

			prIndex(
				t,
				&schema.Index{
					Name:  "t_v_idx",
					Parts: []schema.IndexPart{{Column: "v"}},
					Where: "v <> '--'",
				},
			)

			next := prWorld(t)
			next.Enums = []*schema.Enum{
				{Name: "weird", Values: []string{"a;b", "it's", `back\slash`, "$$", "--x", "/*"}},
			}

			return old, next
		},
	},
	{
		// a column's default stops using a sequence, which is dropped
		name: "60_sequence_dropped_after_default_changes",
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prDef("id", "bigint", "nextval('s1')")), "id"))
			old.Sequences = []*schema.Sequence{{Name: "s1"}}
			next := prWorld(prPK(prTab("t", prDef("id", "bigint", "0")), "id"))

			return old, next
		},
	},
	{
		// a generated column is declared before the columns it needs, and all three
		// are added to a table that has rows
		name: "61_add_generated_column_declared_before_its_inputs",
		seed: []string{"INSERT INTO t (id) VALUES (1)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(prTab("t", prReq("id", "bigint")), "id"))

			next := prWorld(prPK(
				prTab("t",
					prReq("id", "bigint"),
					prGen("total", "integer", "price * qty"),
					prReq("price", "integer"),
					prReq("qty", "integer"),
				),
				"id",
			))

			return old, next
		},
	},
	{
		// index, unique, check and FK names that follow a renamed column
		name:    "62_object_names_follow_renamed_column",
		renames: []string{"orders.account_id=acc_ref"},
		seed: []string{
			"INSERT INTO accounts (id) VALUES (1)",
			"INSERT INTO orders (id, account_id) VALUES (1, 1)",
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(c string) *schema.Schema {
				o := prPK(prTab("orders", prReq("id", "bigint"), prReq(c, "bigint")), "id")
				prUniq(o, "orders_"+c+"_key", c)
				prIdx(o, "orders_"+c+"_idx", c)
				prCheck(o, "orders_"+c+"_check", c+" > 0")
				prFK(o, "orders_"+c+"_fkey", prCols(c), "", "accounts", prCols("id"), "CASCADE")

				return prWorld(prPK(prTab("accounts", prReq("id", "bigint")), "id"), o)
			}

			return mk("account_id"), mk("acc_ref")
		},
		back:        true,
		backRenames: []string{"orders.acc_ref=account_id"},
	},
	{
		// type changes while the default text stays: the stored default is rewritten
		// by the server and must still be what the model says
		name: "63_type_change_with_unchanged_default",
		seed: []string{"INSERT INTO t (id) VALUES (1)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prPK(
				prTab("t",
					prReq("id", "bigint"),
					prDef("a", "varchar(10)", "'x'"),
					prDef("b", "integer", "5"),
				),
				"id",
			))

			next := prWorld(prPK(
				prTab("t",
					prReq("id", "bigint"),
					prDef("a", "text", "'x'"),
					prDef("b", "bigint", "5"),
				),
				"id",
			))

			return old, next
		},
	},
	{
		// kitchen sink: many kinds of change to one populated table at once
		name:    "64_kitchen_sink_on_users",
		renames: []string{"users.name=full_name"},
		seed: []string{
			"INSERT INTO users (name, email, bio, age) VALUES ('a', 'a@x', NULL, 3), ('b', 'b@x', 'hi', 4)",
		},
		verify: []prCheckQuery{{"SELECT count(*)::text FROM users WHERE full_name = 'a'", "1"}},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			old := prWorld(prUniq(
				prCheck(
					prIdx(
						prPK(
							prTab(
								"users",
								prIdent("id"),
								prReq("name", "text"),
								prReq("email", "text"),
								prOpt("bio", "text"),
								prReq("age", "integer"),
							),
							"id",
						),
						"users_name_idx",
						"name",
					),
					"users_age_check",
					"age >= 0",
				),
				"users_email_key",
				"email",
			))

			t := prUniq(
				prPK(
					prTab(
						"users",
						prIdent("id"),
						prReq("full_name", "text"),
						prReq("email", "text"),
						prReq("age", "bigint"),
						prReq("nick", "text"),
						prDef("role", "role", "'member'"),
					),
					"id",
				),
				"users_email_name_key",
				"email",
				"full_name",
			)
			prIdx(t, "users_full_name_idx", "full_name")

			prIndex(
				t,
				&schema.Index{
					Name:  "users_lower_idx",
					Parts: []schema.IndexPart{{Expr: "lower(full_name)"}},
				},
			)

			prCheck(t, "users_age_check", "age >= 0")

			posts := prPK(prTab("posts", prIdent("id"), prReq("user_id", "bigint")), "id")
			prFK(
				posts,
				"posts_user_id_fkey",
				prCols("user_id"),
				"",
				"users",
				prCols("id"),
				"CASCADE",
			)

			next := prWorld(t, posts)
			next.Enums = []*schema.Enum{{Name: "role", Values: []string{"member", "admin"}}}

			return old, next
		},
	},
	{
		// a three-part column rename in a named schema, next to a namesake in the default one
		name:    "65_rename_column_in_named_schema",
		renames: []string{"{p}s1.users.name=title"},
		seed: []string{
			`INSERT INTO "{p}s1".users (id, name) VALUES (1, 'x')`,
			`INSERT INTO users (id, name) VALUES (1, 'y')`,
		},
		verify: []prCheckQuery{
			{`SELECT title FROM "{p}s1".users`, "x"},
			{`SELECT name FROM users`, "y"},
		},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			s1 := p + "s1"

			mk := func(col string) *schema.Schema {
				u := prIdx(
					prPK(prIn(s1, prTab("users", prReq("id", "bigint"), prReq(col, "text"))), "id"),
					"users_c_idx",
					col,
				)

				d := prIdx(
					prPK(prTab("users", prReq("id", "bigint"), prReq("name", "text")), "id"),
					"users_c_idx",
					"name",
				)

				return prWorld(u, d)
			}

			return mk("name"), mk("title")
		},
		back:        true,
		backRenames: []string{"{p}s1.users.title=name"},
	},
	{
		// index method, uniqueness and predicate change together
		name: "66_index_method_unique_and_predicate_change",
		seed: []string{`INSERT INTO t (id, a, j) VALUES (1, 1, '{"k": 1}')`},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(next bool) *schema.Schema {
				t := prPK(
					prTab("t", prReq("id", "bigint"), prReq("a", "integer"), prReq("j", "jsonb")),
					"id",
				)

				if !next {
					prIndex(
						t,
						&schema.Index{
							Name:   "t_a_idx",
							Parts:  []schema.IndexPart{{Column: "a"}},
							Unique: true,
						},
					)

					prIndex(
						t,
						&schema.Index{Name: "t_j_idx", Parts: []schema.IndexPart{{Column: "j"}}},
					)
				} else {
					prIndex(t, &schema.Index{
						Name:   "t_a_idx",
						Parts:  []schema.IndexPart{{Column: "a"}},
						Method: "hash",
						Where:  "a > 0",
					})

					prIndex(
						t,
						&schema.Index{
							Name:   "t_j_idx",
							Parts:  []schema.IndexPart{{Column: "j"}},
							Method: "gin",
						},
					)
				}

				return prWorld(t)
			}

			return mk(false), mk(true)
		},
		back: true,
	},
	{
		// the same compound index without INCLUDE, to separate Inspect's INCLUDE failure
		name: "24b_compound_index_desc_expr_partial_without_include",
		seed: []string{"INSERT INTO t (id, a, b, c) VALUES (1, 1, 'X', 5), (2, 2, 'Y', 6)"},
		build: func(p string) (*schema.Schema, *schema.Schema) {
			mk := func(i *schema.Index) *schema.Schema {
				t := prPK(
					prTab(
						"t",
						prReq("id", "bigint"),
						prReq("a", "integer"),
						prReq("b", "text"),
						prReq("c", "integer"),
					),
					"id",
				)

				return prWorld(prIndex(t, i))
			}
			old := mk(&schema.Index{Name: "t_idx", Parts: []schema.IndexPart{{Column: "a"}}})

			next := mk(&schema.Index{
				Name: "t_idx",
				Parts: []schema.IndexPart{
					{Column: "a", Desc: true},
					{Expr: "lower(b)", Desc: true},
				},
				Where:  "c > 0",
				Unique: true,
			})

			return old, next
		},
		back: true,
	},
}

// ---------------------------------------------------------------- extras

// TestProbeAddNotNullColumnOfEachType adds a NOT NULL column to a populated table
// and expects the zero-value backfill to work for every type.
// noZero lists the types that get no backfill value yet, #36.
var noZero = map[string]bool{
	"time":                true,
	"time with time zone": true,
	"interval":            true,
	"inet":                true,
	"money":               true,
	"mood":                true,
	"bit(3)":              true,
}

func TestProbeAddNotNullColumnOfEachType(t *testing.T) {
	types := []string{
		"smallint",
		"integer",
		"bigint",
		"numeric(10,2)",
		"real",
		"double precision",
		"text",
		"varchar(20)",
		"char(3)",
		"boolean",
		"date",
		"time",
		"time with time zone",
		"timestamp",
		"timestamp(3) without time zone",
		"timestamptz",
		"interval",
		"uuid",
		"bytea",
		"json",
		"jsonb",
		"text[]",
		"integer[]",
		"inet",
		"money",
		"mood",
		"bit(3)",
	}

	for _, typ := range types {
		t.Run(strings.ReplaceAll(typ, " ", "_"), func(t *testing.T) {
			if noZero[typ] {
				knownProbe(t, "#36")
			}

			runProbe(t, probe{
				name: typ,
				seed: []string{"INSERT INTO t (id) VALUES (1), (2)"},
				build: func(p string) (*schema.Schema, *schema.Schema) {
					old := prWorld(prPK(prTab("t", prReq("id", "bigint")), "id"))
					next := prWorld(prPK(prTab("t", prReq("id", "bigint"), prReq("v", typ)), "id"))
					next.Enums = []*schema.Enum{{Name: "mood", Values: []string{"a", "b"}}}
					old.Enums = next.Enums

					return old, next
				},
			})
		})
	}
}

// TestProbeEnumValueRemoved: PostgreSQL cannot remove an enum value, so the plan
// only warns. The warning must reach the Script, and no SQL may pretend to do it.
func TestProbeEnumValueRemoved(t *testing.T) {
	d := ddl(t)

	old := prWorld(prTab("t", prReq("id", "bigint"), prReq("m", "mood")))
	old.Enums = []*schema.Enum{{Name: "mood", Values: []string{"a", "b", "c"}}}
	next := old.Clone()
	next.Enums[0].Values = []string{"a", "c"}

	plan, err := schema.Diff(old, next)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Warnings()) == 0 {
		t.Errorf("no warning for a removed enum value")
	}

	script, _ := schema.Script(d, plan)
	if !strings.Contains(script, "WARNING") {
		t.Errorf("script has no WARNING:\n%s", script)
	}

	// The plan is never "done": the live database keeps the value, so the same plan
	// comes back on every diff against it.
	live := old.Clone()
	again, _ := schema.Diff(live, next)

	if again.Empty() {
		t.Errorf("unexpected: plan became empty")
	}
}

// TestProbeNullsOrderingInvisible: an index with DESC NULLS LAST and a plain DESC
// index read back the same, so the difference cannot be seen or planned.
func TestProbeNullsOrderingInvisible(t *testing.T) {
	knownProbe(t, "#35")

	p := prUnique(t)

	sb := prNewSandbox(t, p)
	if sb == nil {
		t.Skip("no database")
	}

	for _, q := range []string{
		"CREATE TABLE t (id bigint, a integer)",
		"CREATE INDEX i_desc ON t (a DESC)",
		"CREATE INDEX i_desc_last ON t (a DESC NULLS LAST)",
		"CREATE INDEX i_asc_first ON t (a ASC NULLS FIRST)",
		"CREATE INDEX i_asc ON t (a)",
	} {
		if _, err := sb.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	live, err := sb.inspect(nil)
	if err != nil {
		t.Fatal(err)
	}

	tab := live.Table(schema.Ref{Name: "t"})
	find := func(n string) *schema.Index {
		for _, i := range tab.Indexes {
			if i.Name == n {
				return i
			}
		}

		return nil
	}

	if fmt.Sprint(find("i_desc").Parts) == fmt.Sprint(find("i_desc_last").Parts) &&
		fmt.Sprint(find("i_desc").Method) == fmt.Sprint(find("i_desc_last").Method) {
		t.Errorf("Inspect cannot tell DESC from DESC NULLS LAST: both are %+v", find("i_desc"))
	}

	if fmt.Sprint(find("i_asc").Parts) == fmt.Sprint(find("i_asc_first").Parts) {
		t.Errorf("Inspect cannot tell ASC from ASC NULLS FIRST: both are %+v", find("i_asc"))
	}
}

// TestProbeDeterministic plans every scenario five times from the same inputs and
// expects identical Scripts (Diff must not carry state between calls).
func TestProbeDeterministic(t *testing.T) {
	d := ddl(t)

	for _, c := range probes {
		old, next := c.build("pdet_")

		var first string

		for i := 0; i < 5; i++ {
			// A Diff that refuses is an outcome too, and must refuse the same
			// way on every call, so a later call that fails is caught.
			var got string

			plan, err := schema.Diff(old, next, prFill("pdet_", c.renames)...)
			if err != nil {
				got = "Diff refused: " + err.Error()
			} else {
				schema.Backfill(d, plan)

				script, err := schema.Script(d, plan)
				if err != nil {
					t.Errorf("%s: %v", c.name, err)

					break
				}

				got = script
			}

			if i == 0 {
				first = got
			} else if got != first {
				t.Errorf("%s: plan is not deterministic:\n%s\n---\n%s", c.name, first, got)

				break
			}
		}
	}
}

// TestProbeHintForTableMovedBetweenSchemas: the hint for a table that moved
// to another schema must lead to a plan that keeps the data.
func TestProbeHintForTableMovedBetweenSchemas(t *testing.T) {
	knownProbe(t, "#36")

	d := ddl(t)

	old := prWorld(prPK(prIn("s1", prTab("t", prReq("id", "bigint"), prReq("x", "text"))), "id"))
	next := prWorld(prPK(prIn("s2", prTab("t", prReq("id", "bigint"), prReq("x", "text"))), "id"))

	plan, err := schema.Diff(old, next)
	if err != nil {
		t.Fatal(err)
	}

	script, _ := schema.Script(d, plan)

	if len(plan.Hints) == 0 {
		t.Skip("no hint for a table that moved schema")
	}

	for _, h := range plan.Hints {
		after, err := schema.Diff(old, next, h.Flag())
		if err != nil {
			t.Errorf("hint %s: %v", h.Flag(), err)

			continue
		}

		drops := 0

		for _, c := range after.Changes {
			if _, ok := c.(schema.DropTable); ok {
				drops++
			}
		}

		if drops > 0 {
			t.Errorf(
				"hint %q does not remove the DROP TABLE; the hinted rename cannot change schema.\nscript:\n%s",
				h.Flag(),
				script,
			)
		}
	}
}

// TestProbeInspectIndexWithInclude: an index with INCLUDE columns must be readable.
func TestProbeInspectIndexWithInclude(t *testing.T) {
	sb := prNewSandbox(t, prUnique(t))
	if sb == nil {
		t.Skip("no database")
	}

	for _, q := range []string{
		"CREATE TABLE t (id bigint, a integer, b integer)",
		"CREATE INDEX t_idx ON t (a) INCLUDE (b)",
	} {
		if _, err := sb.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	live, err := sb.inspect(nil)
	if err != nil {
		t.Fatalf("Inspect of an index with INCLUDE: %v", err)
	}

	if i := live.Table(schema.Ref{Name: "t"}).Indexes; len(i) != 1 || len(i[0].Include) != 1 {
		t.Errorf("INCLUDE not read back: %+v", i)
	}
}

// TestProbeInspectQuotedIndexColumn: a column name with a double quote, as an
// indexed column, must read back as it was written.
func TestProbeInspectQuotedIndexColumn(t *testing.T) {
	knownProbe(t, "#35")

	sb := prNewSandbox(t, prUnique(t))
	if sb == nil {
		t.Skip("no database")
	}

	for _, q := range []string{
		`CREATE TABLE t (id bigint, "we""ird" integer, "Mixed Case" integer)`,
		`CREATE INDEX t_idx ON t ("we""ird", "Mixed Case")`,
	} {
		if _, err := sb.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	live, err := sb.inspect(nil)
	if err != nil {
		t.Fatal(err)
	}

	parts := live.Table(schema.Ref{Name: "t"}).Indexes[0].Parts
	if parts[0].Column != `we"ird` || parts[1].Column != "Mixed Case" {
		t.Errorf("index columns read back as %q and %q", parts[0].Column, parts[1].Column)
	}
}

// TestProbeSequenceChangeIgnored: a changed sequence start or increment is a
// change in the models, and Diff should not drop it silently.
func TestProbeSequenceChangeIgnored(t *testing.T) {
	knownProbe(t, "#36")

	old := &schema.Schema{Sequences: []*schema.Sequence{{Name: "s", Start: 1, Increment: 1}}}
	next := &schema.Schema{Sequences: []*schema.Sequence{{Name: "s", Start: 100, Increment: 5}}}

	plan, err := schema.Diff(old, next)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Empty() {
		t.Errorf("Diff ignores a sequence whose start and increment changed")
	}
}
