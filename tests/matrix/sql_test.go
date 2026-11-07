package matrix

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/dialect"
	"github.com/skudra-go/ant/dialect/mysql"
	"github.com/skudra-go/ant/dialect/postgres"
	"github.com/skudra-go/ant/dialect/sqlite"
	"github.com/skudra-go/ant/dialect/sqlserver"
)

var update = flag.Bool("update", false, "rewrite the SQL snapshots in testdata")

// TestSQLSnapshots records the SQL of matrix combinations on every dialect, with
// no database. Rendering is ant's own, so any change to it shows here as a diff
// to review: go test ./tests/matrix -run SQLSnapshots -update rewrites them.
func TestSQLSnapshots(t *testing.T) {
	for _, d := range []dialect.Dialect{
		postgres.Dialect,
		mysql.Dialect,
		mysql.MariaDB,
		sqlite.Dialect,
		sqlserver.Dialect,
	} {
		t.Run(d.Name(), func(t *testing.T) {
			pool, err := sql.Open(d.DriverName(), "")
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { pool.Close() })

			db, err := ant.Wrap[MatrixDB](d, pool, ant.WithTracking(false))
			if err != nil {
				t.Fatal(err)
			}

			got := snapshot(t, db)
			path := filepath.Join("testdata", d.Name()+".sql")

			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}

				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v: run with -update to create it", err)
			}

			if got != string(want) {
				t.Errorf(
					"the SQL changed; review the diff after -update:\n%s",
					firstDiff(string(want), got),
				)
			}
		})
	}
}

// snapshot renders every condition on every source, and every ordering and
// paging on every source under one condition, each as a read, an update, and a
// delete.
func snapshot(t *testing.T, db *MatrixDB) string {
	t.Helper()

	var b strings.Builder

	ctx := t.Context()
	unordered, all := orders()[0], pagings()[0]
	conds := conditions()
	compound := conds[slices.IndexFunc(conds, func(c condition) bool {
		return c.name == "age>18 and score set"
	})]

	write := func(name string, q *ant.Query[Author]) {
		text, args, err := q.ToSQL()
		line(&b, name+" read", text, args, err)

		text, args, err = q.UpdateSQL(ctx, ant.Set(&A.Marked, true))
		line(&b, name+" update", text, args, err)

		text, args, err = q.DeleteSQL(ctx)
		line(&b, name+" delete", text, args, err)
	}

	for _, src := range sources() {
		for _, c := range conds {
			write(src.name+" / "+c.name, build(db, src, c, unordered, all))
		}

		for _, o := range orders() {
			for _, p := range pagings() {
				name := fmt.Sprintf("%s / %s / %s / %s", src.name, compound.name, o.name, p.name)
				write(name, build(db, src, compound, o, p))
			}
		}
	}

	return b.String()
}

func line(b *strings.Builder, name, text string, args []any, err error) {
	if err != nil {
		fmt.Fprintf(b, "%s\n  error: %v\n", name, err)

		return
	}

	fmt.Fprintf(b, "%s\n  %s\n  args: %s\n", name, text, formatArgs(args))
}

// formatArgs prints arguments by value, so that pointers do not print addresses.
func formatArgs(args []any) string {
	out := make([]string, len(args))

	for i, a := range args {
		v := reflect.ValueOf(a)
		for v.IsValid() && v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
		}

		if v.IsValid() {
			out[i] = fmt.Sprintf("%#v", v.Interface())
		} else {
			out[i] = "nil"
		}
	}

	return "[" + strings.Join(out, ", ") + "]"
}

// firstDiff shows where two snapshots part.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")

	for i := range max(len(w), len(g)) {
		var a, b string

		if i < len(w) {
			a = w[i]
		}

		if i < len(g) {
			b = g[i]
		}

		if a != b {
			return fmt.Sprintf("line %d\n- %s\n+ %s", i+1, a, b)
		}
	}

	return ""
}
