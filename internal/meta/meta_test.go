package meta

import (
	"database/sql"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/skudra-go/ant/internal/errs"
)

type base struct {
	ID        int64
	CreatedAt time.Time
}

// Base is embedded by the fixtures, as ant.Base is by users.
type Base = base

type user struct {
	Base
	Name   string
	Email  string `ant:"unique"`
	Secret string `ant:"-"`
	Nick   string `ant:"column=nickname"`
	Bio    *string
	Tags   []string
	Posts  []post
	//lint:ignore U1000 proves unexported fields are skipped
	private int
}

type post struct {
	Base
	UserID int64
	Title  string
	User   *user
}

func TestModelColumns(t *testing.T) {
	m := Of[user]()
	if m.Table.Name != "users" {
		t.Errorf("table = %q", m.Table.Name)
	}

	var got []string

	for _, f := range m.Fields {
		got = append(got, f.Column)
	}

	want := []string{"id", "created_at", "name", "email", "nickname", "bio", "tags"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("columns = %v, want %v", got, want)
	}

	if !m.Column("id").Primary {
		t.Error("id is not primary")
	}

	if !m.Column("email").Unique {
		t.Error("email is not unique")
	}
}

func TestModelRelations(t *testing.T) {
	u, p := Of[user](), Of[post]()
	if len(u.Relations) != 1 || u.Relations[0].Kind != Many ||
		u.Relations[0].Target != reflect.TypeFor[post]() {
		t.Errorf("user relations = %+v", u.Relations)
	}

	if len(p.Relations) != 1 || p.Relations[0].Kind != One ||
		p.Relations[0].Target != reflect.TypeFor[user]() {
		t.Errorf("post relations = %+v", p.Relations)
	}
}

func column(t *testing.T, ref Ref, ok bool) string {
	t.Helper()

	if !ok || ref.Field == nil {
		t.Fatalf("not a column: %+v, %v", ref, ok)
	}

	return ref.Field.Column
}

func TestResolveColumns(t *testing.T) {
	u := Probe[user]()

	cases := []struct {
		name, want string
		ref        func() (Ref, bool)
	}{
		{"promoted first field", "id", func() (Ref, bool) { return Resolve(&u.ID) }},
		{
			"promoted second field",
			"created_at",
			func() (Ref, bool) { return Resolve(&u.CreatedAt) },
		},
		{"direct", "name", func() (Ref, bool) { return Resolve(&u.Name) }},
		{"tag override", "nickname", func() (Ref, bool) { return Resolve(&u.Nick) }},
		{"pointer column", "bio", func() (Ref, bool) { return Resolve(&u.Bio) }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, ok := c.ref()
			if got := column(t, ref, ok); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveRelations(t *testing.T) {
	u, p := Probe[user](), Probe[post]()
	if ref, ok := Resolve(&u.Posts); !ok || ref.Relation == nil || ref.Relation.Name != "Posts" {
		t.Errorf("Posts: %+v, %v", ref, ok)
	}

	if ref, ok := Resolve(&p.User); !ok || ref.Relation == nil || ref.Relation.Name != "User" {
		t.Errorf("User: %+v, %v", ref, ok)
	}
}

func TestResolveDistinguishesSharedAddress(t *testing.T) {
	u := Probe[user]()
	if _, ok := Resolve(&u.Base); ok {
		t.Error("embedded struct resolved as a field")
	}

	ref, ok := Resolve(&u.ID)
	if got := column(t, ref, ok); got != "id" {
		t.Errorf("got %q", got)
	}
}

func TestResolveSeparatesModels(t *testing.T) {
	u, p := Probe[user](), Probe[post]()
	a, _ := Resolve(&u.ID)

	b, _ := Resolve(&p.ID)
	if a.Model == b.Model {
		t.Error("probes of different models resolved to one model")
	}
}

func TestResolveRejectsNonProbe(t *testing.T) {
	x := 5
	if _, ok := Resolve(&x); ok {
		t.Error("local variable resolved")
	}

	var u user
	if _, ok := Resolve(&u.Name); ok {
		t.Error("field of a non-probe struct resolved")
	}
}

func TestProbeSurvivesGC(t *testing.T) {
	u := Probe[user]()
	before := reflect.ValueOf(&u.Name).Pointer()

	runtime.GC()
	runtime.GC()

	if after := reflect.ValueOf(&u.Name).Pointer(); after != before {
		t.Fatalf("probe moved: %x to %x", before, after)
	}

	ref, ok := Resolve(&u.Name)
	if got := column(t, ref, ok); got != "name" {
		t.Errorf("got %q", got)
	}
}

func TestLinks(t *testing.T) {
	type node struct {
		ID       int64
		ParentID *int64
		Parent   *node
		Children []node
	}

	type owner struct {
		ID int64
		A  []post2
	}

	u, p := Of[user](), Of[post]()

	many, err := u.Relations[0].Link()
	if err != nil || many.Local.Column != "id" || many.Foreign.Column != "user_id" ||
		many.BelongsTo {
		t.Errorf("user.Posts = %+v, %v", many, err)
	}

	one, err := p.Relations[0].Link()
	if err != nil || one.Local.Column != "user_id" || one.Foreign.Column != "id" || !one.BelongsTo {
		t.Errorf("post.User = %+v, %v", one, err)
	}

	n := Of[node]()

	parent, err := n.Relations[0].Link()
	if err != nil || parent.Local.Column != "parent_id" || !parent.BelongsTo {
		t.Errorf("node.Parent = %+v, %v", parent, err)
	}

	children, err := n.Relations[1].Link()
	if err != nil || children.Foreign.Column != "parent_id" || children.BelongsTo {
		t.Errorf("node.Children = %+v, %v", children, err)
	}

	if _, err := Of[owner]().Relations[0].Link(); !errors.Is(err, errs.ErrNoForeignKey) {
		t.Errorf("no key: %v", err)
	}
}

type post2 struct{ ID int64 }

func TestKeyNormalizesAcrossTypes(t *testing.T) {
	type row struct {
		A int32
		B int64
		C *int64
		D *int64
		E sql.Null[int64]
		F sql.Null[int64]
		G string
	}

	m := Of[row]()
	v := int64(7)
	r := row{
		A: 7,
		B: 7,
		C: &v,
		E: sql.Null[int64]{V: 7, Valid: true},
		G: "x",
	}

	for _, f := range m.Fields {
		got, ok := f.Key(unsafe.Pointer(&r))

		switch f.Short {
		case "D", "F":
			if ok {
				t.Errorf("%s: want NULL, got %v", f.Short, got)
			}
		case "G":
			if got != "x" {
				t.Errorf("G = %v", got)
			}
		default:
			if got != int64(7) {
				t.Errorf("%s = %#v", f.Short, got)
			}
		}
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"UserID":    "user_id",
		"ID":        "id",
		"CreatedAt": "created_at",
		"HTTPCode":  "http_code",
		"Name":      "name",
	} {
		if got := ColumnName(in); got != want {
			t.Errorf("ColumnName(%q) = %q, want %q", in, got, want)
		}
	}

	for in, want := range map[string]string{
		"User":      "users",
		"Post":      "posts",
		"Category":  "categories",
		"Box":       "boxes",
		"OrderItem": "order_items",
		"Key":       "keys",
	} {
		if got := TableName(in); got != want {
			t.Errorf("TableName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTagKeepsExpressionsWhole(t *testing.T) {
	type Row struct {
		Status string `ant:"default='a,b',immutable"`
		Score  int    `ant:"default=coalesce(1, 2),column=points"`
	}

	m := Of[Row]()

	status, score := m.Fields[0], m.Fields[1]
	if !status.Default || !status.Immutable || status.Storage.Default != "'a,b'" {
		t.Errorf("status = %+v", status)
	}

	if score.Storage.Default != "coalesce(1, 2)" || score.Column != "points" {
		t.Errorf("score = %+v", score)
	}
}

func TestEmbeddedFieldMustBeStructValue(t *testing.T) {
	type address struct{ City string }

	cases := []struct{ name string }{
		{"Pointer"}, {"Scalar"}, {"Nested.Address"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var model *Model

			switch tc.name {
			case "Pointer":
				model = Of[struct {
					Pointer *address `ant:"embedded,prefix=addr_"`
				}]()
			case "Scalar":
				model = Of[struct {
					Scalar string `ant:"embedded"`
				}]()
			default:
				model = Of[struct {
					Nested struct {
						Address *address `ant:"embedded"`
					} `ant:"embedded"`
				}]()
			}

			err := model.Err()
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("got %v, want field %s in error", err, tc.name)
			}

			called := false

			model.Configure(func() error {
				called = true

				return nil
			})

			if called || model.Err() != err {
				t.Fatal("configuration replaced invalid embedded-field error")
			}
		})
	}
}
