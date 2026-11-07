package ant_test

import (
	"errors"
	"testing"
	"time"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/internal/meta"
)

type user struct {
	ant.Base
	Name  string
	Email string `ant:"unique"`
	Age   int
	Bio   *string
	Posts []post
}

type post struct {
	ant.Base
	UserID    int64
	Title     string
	Published bool
	User      *user
}

func (user) Configure(b *ant.ModelBuilder[user]) {
	b.Table("users")
	b.Index(&U.Email).Unique()
	b.HasMany(&U.Posts).ForeignKey(&P.UserID).OnDelete(ant.Cascade)
}

var _ ant.Entity[user] = user{}

var (
	U = ant.Fields[user]()
	P = ant.Fields[post]()
)

func columnOf[V any](t *testing.T, field *V) string {
	t.Helper()

	ref, ok := meta.Resolve(field)
	if !ok || ref.Field == nil {
		t.Fatalf("not a column: %+v", ref)
	}

	return ref.Model.Table.Name + "." + ref.Field.Column
}

func TestProbeMapsUser(t *testing.T) {
	for want, got := range map[string]string{
		"users.id":         columnOf(t, &U.ID),
		"users.created_at": columnOf(t, &U.CreatedAt),
		"users.updated_at": columnOf(t, &U.UpdatedAt),
		"users.name":       columnOf(t, &U.Name),
		"users.email":      columnOf(t, &U.Email),
		"users.age":        columnOf(t, &U.Age),
		"users.bio":        columnOf(t, &U.Bio),
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestProbeMapsPost(t *testing.T) {
	for want, got := range map[string]string{
		"posts.id":        columnOf(t, &P.ID),
		"posts.user_id":   columnOf(t, &P.UserID),
		"posts.title":     columnOf(t, &P.Title),
		"posts.published": columnOf(t, &P.Published),
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestProbeMapsRelations(t *testing.T) {
	if ref, ok := meta.Resolve(&U.Posts); !ok || ref.Relation == nil {
		t.Errorf("Posts: %+v", ref)
	}

	if ref, ok := meta.Resolve(&P.User); !ok || ref.Relation == nil {
		t.Errorf("User: %+v", ref)
	}
}

// Builder errors must reach the terminal call, ahead of ErrNotImplemented.
func TestBuilderErrorsSurfaceAtTerminal(t *testing.T) {
	var (
		users ant.Table[user]
		posts ant.Table[post]
	)

	x := 5
	ctx := t.Context()

	cases := []struct {
		name string
		run  func() error
		want error
	}{
		{
			"non-probe condition",
			func() error {
				//antvet:ignore not a probe, on purpose: the test checks the error
				_, err := users.Where(ant.Gt(&x, 3)).All(ctx)

				return err
			},
			ant.ErrNotProbe,
		},
		{
			"wrong model condition",
			func() error {
				//antvet:ignore another model's probe, on purpose: the test checks the error
				_, err := posts.Where(ant.Gt(&U.Age, 18)).All(ctx)

				return err
			},
			ant.ErrWrongModel,
		},
		{
			"wrong model nested in Or",
			func() error {
				//antvet:ignore another model's probe in Or, on purpose: the test checks the error
				_, err := posts.Where(ant.Or(ant.Eq(&P.Published, true), ant.Gt(&U.Age, 18))).
					All(ctx)

				return err
			},
			ant.ErrWrongModel,
		},
		{
			"wrong model order",
			func() error {
				//antvet:ignore another model's field, on purpose: the test checks the error
				_, err := posts.OrderBy(&U.Name).All(ctx)

				return err
			},
			ant.ErrWrongModel,
		},
		{
			"wrong model assignment",
			func() error {
				//antvet:ignore another model's field, on purpose: the test checks the error
				_, err := posts.Where().Update(ctx, ant.Set(&U.Age, 1))

				return err
			},
			ant.ErrWrongModel,
		},
		{
			"non-probe relation",
			func() error {
				var rel []post

				//antvet:ignore not a probe, on purpose: the test checks the error
				_, err := users.Include(&rel).All(ctx)

				return err
			},
			ant.ErrNotProbe,
		},
		{
			"probe of an unjoined model in a join",
			func() error {
				type other struct{ ID int64 }

				o := ant.Fields[other]()
				//antvet:ignore another model's field, on purpose: the test checks the error
				_, err := posts.Join(&P.User).Where(ant.Eq(&o.ID, int64(1))).
					Select(func(p *post, u *user) string { return u.Name }).All(ctx)

				return err
			},
			ant.ErrWrongModel,
		},
		{
			"relation of an unjoined model",
			func() error {
				//antvet:ignore another model's field, on purpose: the test checks the error
				_, err := posts.Include(&U.Posts).All(ctx)

				return err
			},
			ant.ErrWrongModel,
		},
		{
			"error survives Join",
			func() error {
				//antvet:ignore not a probe, on purpose: the test checks the error
				_, err := posts.Where(ant.Gt(&x, 3)).Join(&P.User).
					Select(func(p *post, u *user) string { return u.Name }).All(ctx)

				return err
			},
			ant.ErrNotProbe,
		},
		{
			"error reaches Each",
			func() error {
				//antvet:ignore not a probe, on purpose: the test checks the error
				for _, err := range users.Where(ant.Gt(&x, 3)).Each(ctx) {
					return err
				}

				return nil
			},
			ant.ErrNotProbe,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestValidChainHasNoBuilderErrors(t *testing.T) {
	var posts ant.Table[post]

	_, err := posts.
		Join(&P.User).
		Where(ant.Eq(&P.Published, true), ant.Gt(&U.Age, 18)).
		OrderBy(&U.CreatedAt).
		Select(func(p *post, u *user) string { return p.Title }).
		All(t.Context())
	if !errors.Is(err, ant.ErrNotOpen) || errors.Is(err, ant.ErrWrongModel) {
		t.Errorf("err = %v", err)
	}
}

func TestUnopenedTable(t *testing.T) {
	var users ant.Table[user]
	if _, err := users.All(t.Context()); !errors.Is(err, ant.ErrNotOpen) {
		t.Errorf("All: %v", err)
	}

	if err := users.Insert(t.Context(), &user{}); !errors.Is(err, ant.ErrNotOpen) {
		t.Errorf("Insert: %v", err)
	}

	if _, err := users.Find(t.Context(), 1); !errors.Is(err, ant.ErrNotOpen) {
		t.Errorf("Find: %v", err)
	}
}

func TestOrderedAcceptsTime(t *testing.T) {
	_ = ant.Gt(&U.CreatedAt, time.Now())
}

func TestOpenRejectsTypeWithoutContext(t *testing.T) {
	type notAContext struct{}

	_, err := ant.Open[notAContext](t.Context(), nil, "")
	if !errors.Is(err, ant.ErrNoContext) {
		t.Errorf("err = %v", err)
	}
}
