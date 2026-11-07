package change_tracking

// Exploratory probes of change tracking and SaveChanges on relation graphs.
// Every test is named TestProbe... and asserts what a correct ORM should do,
// reading the database back past the tracker.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// ---------------------------------------------------------------- models

type Dept struct {
	ID     int64
	Name   string
	Emps   []Emp
	Badges []Badge
}

func (Dept) Configure(b *ant.ModelBuilder[Dept]) {
	b.ManyToMany(&Dp.Badges).Through("dept_badges")
}

type Emp struct {
	ID      int64
	DeptID  int64
	Name    string
	Version int64 `ant:"version"`
	Dept    *Dept
}

type Badge struct {
	ID    int64
	Name  string
	Depts []Dept
}

func (Badge) Configure(b *ant.ModelBuilder[Badge]) {
	b.ManyToMany(&Bd.Depts).Through("dept_badges")
}

type Grp struct {
	ID    int64
	Name  string
	Items []Item
}

type Item struct {
	ant.Base
	GrpID *int64
	Label string
	Note  *string
	Qty   *int
	Ver   int64 `ant:"version"`
	Grp   *Grp
}

type Crate struct {
	ant.Base
	Name      string
	DeletedAt ant.DeletedAt
	Gems      []Gem
}

type Gem struct {
	ant.Base
	CrateID   int64
	Name      string
	DeletedAt ant.DeletedAt
	Crate     *Crate
}

type Cat struct {
	ID       int64
	ParentID *int64
	Name     string
	Parent   *Cat
	Children []Cat
}

type ProbeDB struct {
	ant.Context
	Depts  ant.Table[Dept]
	Emps   ant.Table[Emp]
	Badges ant.Table[Badge]
	Grps   ant.Table[Grp]
	Items  ant.Table[Item]
	Crates ant.Table[Crate]
	Gems   ant.Table[Gem]
	Cats   ant.Table[Cat]
}

var (
	Dp = ant.Fields[Dept]()
	Em = ant.Fields[Emp]()
	Bd = ant.Fields[Badge]()
	Gp = ant.Fields[Grp]()
	It = ant.Fields[Item]()
	Cr = ant.Fields[Crate]()
	Gm = ant.Fields[Gem]()
	Ct = ant.Fields[Cat]()
)

const probeDDL = `
	CREATE TABLE depts (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL);
	CREATE TABLE emps (id BIGSERIAL PRIMARY KEY, dept_id BIGINT NOT NULL REFERENCES depts, name TEXT NOT NULL, version BIGINT NOT NULL, UNIQUE (dept_id, name));
	CREATE TABLE badges (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL);
	CREATE TABLE dept_badges (
		dept_id BIGINT NOT NULL REFERENCES depts ON DELETE CASCADE,
		badge_id BIGINT NOT NULL REFERENCES badges ON DELETE CASCADE,
		PRIMARY KEY (dept_id, badge_id));
	CREATE TABLE grps (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL);
	CREATE TABLE items (id BIGSERIAL PRIMARY KEY, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
		grp_id BIGINT REFERENCES grps, label TEXT NOT NULL, note TEXT, qty INT, ver BIGINT NOT NULL);
	CREATE TABLE crates (id BIGSERIAL PRIMARY KEY, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
		name TEXT NOT NULL, deleted_at TIMESTAMPTZ);
	CREATE TABLE gems (id BIGSERIAL PRIMARY KEY, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
		crate_id BIGINT NOT NULL REFERENCES crates, name TEXT NOT NULL, deleted_at TIMESTAMPTZ);
	CREATE TABLE cats (id BIGSERIAL PRIMARY KEY, parent_id BIGINT REFERENCES cats, name TEXT NOT NULL UNIQUE)`

var probeCascadeDDL = strings.Replace(
	probeDDL,
	"REFERENCES depts, name",
	"REFERENCES depts ON DELETE CASCADE, name",
	1,
)

// ---------------------------------------------------------------- harness

// wlog records the write statements a context runs.
type wlog struct {
	mu    sync.Mutex
	stmts []string
}

func (w *wlog) Before(ctx context.Context, s *ant.Statement) (context.Context, error) {
	sql := strings.ToUpper(strings.TrimSpace(s.SQL))
	if strings.HasPrefix(sql, "INSERT") || strings.HasPrefix(sql, "UPDATE") ||
		strings.HasPrefix(sql, "DELETE") {
		w.mu.Lock()
		w.stmts = append(w.stmts, s.SQL)
		w.mu.Unlock()
	}

	return ctx, nil
}

func (w *wlog) After(context.Context, *ant.Statement, ant.Outcome) {}

func (w *wlog) reset() {
	w.mu.Lock()
	w.stmts = nil
	w.mu.Unlock()
}

func (w *wlog) writes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.Clone(w.stmts)
}

type saver interface {
	HasChanges() bool
	SaveChanges(context.Context) error
}

// idle asserts that nothing is pending and a further SaveChanges writes nothing.
func idle(t *testing.T, ses saver, w *wlog) {
	t.Helper()
	assert.False(t, ses.HasChanges(), "HasChanges is still true after SaveChanges")
	w.reset()
	assert.NoError(t, ses.SaveChanges(t.Context()))
	assert.Empty(t, w.writes(), "a second SaveChanges wrote again")
}

func probe(t *testing.T, fn func(t *testing.T, db *ProbeDB, w *wlog)) {
	t.Helper()
	probeWith(t, probeDDL, fn)
}

func probeWith(t *testing.T, ddl string, fn func(t *testing.T, db *ProbeDB, w *wlog)) {
	t.Helper()

	fixture.Each(t, func(t *testing.T, target fixture.Target) {
		_, err := target.DB.ExecContext(t.Context(), ddl)
		require.NoError(t, err)

		w := &wlog{}
		fn(t, fixture.Open[ProbeDB](t, target, ant.WithTracking(true), ant.WithInterceptor(w)), w)
	})
}

var errProbeStop = errors.New("probe stop")

// seedAB inserts depts A (emps a1, a2, a3) and B (emp b1) past the tracker.
func seedAB(t *testing.T, db *ProbeDB) (a, b *Dept) {
	t.Helper()
	ctx := t.Context()
	a, b = &Dept{Name: "A"}, &Dept{Name: "B"}
	require.NoError(t, db.Depts.InsertOnly(ctx, a, b))

	require.NoError(t, db.Emps.InsertOnly(ctx,
		&Emp{DeptID: a.ID, Name: "a1"},
		&Emp{DeptID: a.ID, Name: "a2"},
		&Emp{DeptID: a.ID, Name: "a3"},
		&Emp{DeptID: b.ID, Name: "b1"}))

	return a, b
}

func byName(q *ant.Query[Emp]) *ant.Query[Emp] { return q.OrderBy(&Em.Name) }

// loadAB returns A and B with their emps, from the session.
func loadAB(t *testing.T, ses *ProbeDB) (a, b *Dept) {
	t.Helper()
	ds, err := ses.Depts.Include(&Dp.Emps, byName).OrderBy(&Dp.Name).All(t.Context())
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(ds), 2)

	return ds[0], ds[1]
}

func empsOf(t *testing.T, db *ProbeDB, dept int64) []string {
	t.Helper()

	rows, err := db.Emps.NoTracking().Where(ant.Eq(&Em.DeptID, dept)).OrderBy(&Em.Name).
		All(t.Context())
	require.NoError(t, err)

	out := make([]string, len(rows))

	for i, r := range rows {
		out[i] = r.Name
	}

	return out
}

func storedEmp(t *testing.T, db *ProbeDB, id int64) *Emp {
	t.Helper()
	e, err := db.Emps.NoTracking().Find(t.Context(), id)
	require.NoError(t, err)

	return e
}

func empCount(t *testing.T, db *ProbeDB) int64 {
	t.Helper()
	n, err := db.Emps.NoTracking().Count(t.Context())
	require.NoError(t, err)

	return n
}

func storedItem(t *testing.T, db *ProbeDB, id int64) *Item {
	t.Helper()
	i, err := db.Items.NoTracking().Find(t.Context(), id)
	require.NoError(t, err)

	return i
}

func badgesOf(t *testing.T, db *ProbeDB, dept int64) []string {
	t.Helper()

	d, err := db.Depts.NoTracking().
		Include(&Dp.Badges, func(q *ant.Query[Badge]) *ant.Query[Badge] {
			return q.OrderBy(&Bd.Name)
		}).
		Find(t.Context(), dept)
	require.NoError(t, err)

	out := make([]string, len(d.Badges))

	for i, b := range d.Badges {
		out[i] = b.Name
	}

	return out
}

func pivotCount(t *testing.T, db *ProbeDB) int {
	t.Helper()
	n, err := ant.Raw[int](db, `SELECT count(*) FROM dept_badges`).First(t.Context())
	require.NoError(t, err)

	return *n
}

// ================================================================ slices

// A child moved from one loaded parent's slice to another's, with its foreign
// key left alone, ends up with the new parent.
func TestProbeMoveChildNoFKEdit(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)
		a, b := loadAB(t, ses)

		moved := a.Emps[0]
		a.Emps = a.Emps[1:]
		b.Emps = append(b.Emps, moved)

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"a1", "b1"}, empsOf(t, db, bSeed.ID))
		assert.Equal(t, bSeed.ID, b.Emps[len(b.Emps)-1].DeptID, "in-memory FK follows the slice")
		idle(t, ses, w)
	})
}

// Removing a child and putting it back before saving changes nothing.
func TestProbeRemoveThenReaddSameSave(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		removed := a.Emps[1]
		a.Emps = slices.Delete(a.Emps, 1, 2)
		a.Emps = append(a.Emps, removed)

		w.reset()
		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, w.writes())
		assert.Equal(t, []string{"a1", "a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.EqualValues(t, 1, storedEmp(t, db, removed.ID).Version)
		idle(t, ses, w)
	})
}

// Re-adding a child whose row was deleted by an earlier save either fails or
// brings the row back. Silently doing nothing loses it.
func TestProbeReaddAfterDeleteSaved(t *testing.T) {
	knownProbe(t, "#44")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		removed := a.Emps[0]
		a.Emps = a.Emps[1:]

		require.NoError(t, ses.SaveChanges(ctx))
		require.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))

		a.Emps = append(a.Emps, removed)

		err := ses.SaveChanges(ctx)
		if err == nil {
			assert.Contains(
				t,
				empsOf(t, db, aSeed.ID),
				"a1",
				"SaveChanges returned nil but the re-added child is not in the database",
			)
		}
	})
}

// A slice replaced by a new one holding some old elements and some new ones.
func TestProbeReplaceSliceMixed(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		keep := a.Emps[1]
		keep.Name = "a2-edited"
		a.Emps = []Emp{keep, {Name: "fresh"}}

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2-edited", "fresh"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"b1"}, empsOf(t, db, bSeed.ID))
		assert.EqualValues(t, 2, storedEmp(t, db, keep.ID).Version)
		assert.EqualValues(t, 2, a.Emps[0].Version, "in-memory version bumped")
		assert.NotZero(t, a.Emps[1].ID)
		assert.Equal(t, aSeed.ID, a.Emps[1].DeptID)
		idle(t, ses, w)
	})
}

// Many appends that reallocate, with edits to old elements in between.
func TestProbeManyAppendsAndEdits(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		for i := range 20 {
			a.Emps = append(a.Emps, Emp{Name: fmt.Sprintf("n%02d", i)})
			a.Emps[i%3].Name = fmt.Sprintf("edit%d", i)
		}

		require.NoError(t, ses.SaveChanges(ctx))

		got := empsOf(t, db, aSeed.ID)
		assert.Len(t, got, 23)
		assert.Contains(t, got, "edit17") // i%3==2 last at i=17 -> a.Emps[2]
		assert.Contains(t, got, "edit18")
		assert.Contains(t, got, "edit19")
		assert.NotContains(t, got, "a1")
		idle(t, ses, w)

		// Now the new elements are tracked: edit one, append one, save.
		a.Emps[10].Name = "late-edit"
		a.Emps = append(a.Emps, Emp{Name: "tail"})

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Contains(t, empsOf(t, db, aSeed.ID), "late-edit")
		assert.Contains(t, empsOf(t, db, aSeed.ID), "tail")
		assert.EqualValues(t, 24, countIn(t, db, aSeed.ID), "no duplicated inserts")
		idle(t, ses, w)
	})
}

// Swapping children between two parents in one save.
func TestProbeSwapChildren(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)
		a, b := loadAB(t, ses)

		x, y := a.Emps[0], b.Emps[0]
		a.Emps = append(a.Emps[1:], y)
		b.Emps = []Emp{x}

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2", "a3", "b1"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"a1"}, empsOf(t, db, bSeed.ID))
		idle(t, ses, w)
	})
}

// Move away and back before saving: nothing is written.
func TestProbeMoveAwayAndBack(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)
		a, b := loadAB(t, ses)

		e := a.Emps[0]
		a.Emps = a.Emps[1:]
		b.Emps = append(b.Emps, e)
		b.Emps = b.Emps[:len(b.Emps)-1]
		a.Emps = append([]Emp{e}, a.Emps...)

		w.reset()
		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, w.writes(), "net no change")
		assert.Equal(t, []string{"a1", "a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"b1"}, empsOf(t, db, bSeed.ID))
	})
}

// Moving a child to a parent whose Emps were not loaded (the natural thing to
// do after Find) must move it, not delete it.
func TestProbeMoveToParentWithoutLoadedRelation(t *testing.T) {
	knownProbe(t, "#37")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		b, err := ses.Depts.Find(ctx, bSeed.ID) // Emps not loaded; returns the tracked instance
		require.NoError(t, err)

		other := &Dept{Name: "C"}
		require.NoError(t, ses.Depts.Insert(ctx, other))

		moved := a.Emps[0]
		a.Emps = a.Emps[1:]
		other.Emps = append(other.Emps, moved)

		_ = b

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))

		assert.Equal(
			t,
			[]string{"a1"},
			empsOf(t, db, other.ID),
			"the moved child must survive under C",
		)

		assert.EqualValues(t, 4, empCount(t, db), "no emp may be lost")
	})
}

// Moving a child to a parent that is new in the same save.
func TestProbeMoveToNewParent(t *testing.T) {
	knownProbe(t, "#37")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		fresh := &Dept{Name: "Split"}
		require.NoError(t, ant.Add(ses, fresh))

		moved := a.Emps[0]
		a.Emps = a.Emps[1:]
		fresh.Emps = append(fresh.Emps, moved)

		require.NoError(t, ses.SaveChanges(ctx))
		assert.NotZero(t, fresh.ID)
		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))

		assert.Equal(
			t,
			[]string{"a1"},
			empsOf(t, db, fresh.ID),
			"moved child survives under the new dept",
		)

		assert.EqualValues(t, 4, empCount(t, db), "no emp may be lost")
	})
}

// Removing a child from the slice of a parent that was inserted by this
// session (its slice was never loaded, but it was in memory when inserted).
func TestProbeRemoveFromInsertedParentSlice(t *testing.T) {
	knownProbe(t, "#45")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)
		d := &Dept{Name: "N", Emps: []Emp{{Name: "e1"}, {Name: "e2"}, {Name: "e3"}}}
		require.NoError(t, ses.Depts.Insert(ctx, d))

		d.Emps = d.Emps[:2]

		require.NoError(t, ses.SaveChanges(ctx))

		assert.Equal(
			t,
			[]string{"e1", "e2"},
			empsOf(t, db, d.ID),
			"a child removed from the slice of an inserted parent is not deleted",
		)
	})
}

// HasChanges documents "whether SaveChanges has anything to write".
func TestProbeHasChangesCountsRelationEdits(t *testing.T) {
	knownProbe(t, "#44")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps = a.Emps[1:]

		assert.True(t, ses.HasChanges(), "a removed child is pending")

		a.Emps = append(a.Emps, Emp{Name: "new"})

		assert.True(t, ses.HasChanges(), "an appended child is pending")

		w.reset()
		require.NoError(t, ses.SaveChanges(t.Context()))
		assert.NotEmpty(t, w.writes(), "SaveChanges wrote, so HasChanges should have said so")
	})
}

// A duplicate of an element appended to the same slice: edits must not vanish.
func TestProbeDuplicateElementInSlice(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps[0].Name = "orig-edit"
		a.Emps = append(a.Emps, a.Emps[0]) // same key twice, same values

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, "orig-edit", storedEmp(t, db, a.Emps[0].ID).Name)
		assert.EqualValues(t, 3, countIn(t, db, a.ID), "no extra row from the duplicate")
	})
}

func countIn(t *testing.T, db *ProbeDB, dept int64) int64 {
	t.Helper()
	n, err := db.Emps.NoTracking().Where(ant.Eq(&Em.DeptID, dept)).Count(t.Context())
	require.NoError(t, err)

	return n
}

// ================================================================ version columns

// Edit a slice child, save, edit again, save: versions track, no false conflict.
func TestProbeVersionedChildTwoSaves(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)
		id := a.Emps[0].ID

		a.Emps[0].Name = "v2"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.EqualValues(t, 2, a.Emps[0].Version)

		a.Emps = append(a.Emps, Emp{Name: "pushed"}) // reallocates
		a.Emps[0].Name = "v3"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.EqualValues(t, 3, storedEmp(t, db, id).Version)
		assert.Equal(t, "v3", storedEmp(t, db, id).Name)
		assert.EqualValues(t, 3, a.Emps[0].Version)
		idle(t, ses, w)
	})
}

// A stale slice child is rejected by the version column.
func TestProbeVersionedChildConflict(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		seedAB(t, db)
		one, two := ant.Session(db), ant.Session(db)
		a1, _ := loadAB(t, one)
		a2, _ := loadAB(t, two)

		a2.Emps[0].Name = "from-two"

		require.NoError(t, two.SaveChanges(ctx))

		a1.Emps[0].Name = "from-one"
		err := one.SaveChanges(ctx)
		assert.ErrorIs(t, err, ant.ErrConcurrency)
		assert.Equal(t, "from-two", storedEmp(t, db, a1.Emps[0].ID).Name)
	})
}

// A move bumps the version; a later edit still succeeds.
func TestProbeVersionedMoveThenEdit(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		seedAB(t, db)
		ses := ant.Session(db)
		a, b := loadAB(t, ses)

		moved := a.Emps[0]
		a.Emps = a.Emps[1:]
		b.Emps = append(b.Emps, moved)

		require.NoError(t, ses.SaveChanges(ctx))
		assert.EqualValues(t, 2, storedEmp(t, db, moved.ID).Version, "the move is a write")
		assert.EqualValues(t, 2, b.Emps[len(b.Emps)-1].Version)

		b.Emps[len(b.Emps)-1].Name = "after-move"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, "after-move", storedEmp(t, db, moved.ID).Name)
		assert.EqualValues(t, 3, storedEmp(t, db, moved.ID).Version)
		idle(t, ses, w)
	})
}

// An orphan delete after an earlier save of the same row uses the new version.
func TestProbeVersionedOrphanAfterEditSave(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps[2].Name = "a3-edit"

		require.NoError(t, ses.SaveChanges(ctx))

		a.Emps = a.Emps[:2]

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a1", "a2"}, empsOf(t, db, aSeed.ID))
	})
}

// The same row loaded twice (Find and through a slice): one instance edited,
// the other removed from the slice. The row is deleted; the edit must not turn
// into a conflict with the same unit's delete.
func TestProbeVersionedDuplicateInstanceEditAndOrphan(t *testing.T) {
	knownProbe(t, "#42")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		// Load the pointer instance first, so the slice element is the second one.
		direct, err := ses.Emps.Where(ant.Eq(&Em.Name, "a1")).Single(ctx)
		require.NoError(t, err)

		a, _ := loadAB(t, ses)
		require.NotSame(t, direct, &a.Emps[0], "second instance")
		require.Equal(t, direct.ID, a.Emps[0].ID)

		direct.Name = "edited-elsewhere"
		a.Emps = a.Emps[1:]

		err = ses.SaveChanges(ctx)
		assert.NoError(t, err, "edit + delete of one row in one unit")
		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))
	})
}

// The same item loaded twice, edits to different columns through each instance,
// with a version column: both must be saved without a false conflict.
func TestProbeVersionedDuplicateInstancesBothEdited(t *testing.T) {
	knownProbe(t, "#42")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		g := &Grp{Name: "g"}
		require.NoError(t, db.Grps.InsertOnly(ctx, g))
		item := &Item{GrpID: &g.ID, Label: "l"}
		require.NoError(t, db.Items.InsertOnly(ctx, item))

		ses := ant.Session(db)
		direct, err := ses.Items.Find(ctx, item.ID)
		require.NoError(t, err)

		loadedG, err := ses.Grps.Include(&Gp.Items).Find(ctx, g.ID)
		require.NoError(t, err)
		require.NotSame(t, direct, &loadedG.Items[0])

		direct.Label = "label-from-direct"
		loadedG.Items[0].Qty = new(7)

		err = ses.SaveChanges(ctx)
		require.NoError(t, err, "two instances of one versioned row, different columns")

		got := storedItem(t, db, item.ID)
		assert.Equal(t, "label-from-direct", got.Label)
		require.NotNil(t, got.Qty)
		assert.Equal(t, 7, *got.Qty)
	})
}

// A failed save must not bump versions twice on retry.
func TestProbeRetryAfterFailureVersions(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps[0].Name = "a1-new"
		a.Emps[2].Name = "a2" // collides with the second emp (unique dept_id, name)
		fresh := Emp{Name: "fresh"}
		a.Emps = append(a.Emps, fresh)

		err := ses.SaveChanges(ctx)
		require.Error(t, err)
		assert.EqualValues(t, 1, a.Emps[0].Version, "in-memory version restored after failure")
		assert.Zero(t, a.Emps[3].ID, "generated key cleared")
		assert.Equal(t, []string{"a1", "a2", "a3"}, empsOf(t, db, aSeed.ID), "nothing was saved")

		a.Emps[2].Name = "a3-fixed"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a1-new", "a2", "a3-fixed", "fresh"}, empsOf(t, db, aSeed.ID))
		assert.EqualValues(t, 2, storedEmp(t, db, a.Emps[0].ID).Version, "bumped once")
		assert.EqualValues(t, 2, a.Emps[0].Version)
		idle(t, ses, w)
	})
}

// A failed save that included a move: retry completes the move.
func TestProbeRetryAfterFailureWithMove(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)
		a, b := loadAB(t, ses)

		moved := a.Emps[0]
		a.Emps = a.Emps[1:]
		b.Emps = append(b.Emps, moved)
		b.Emps = append(b.Emps, Emp{Name: "b1"}) // duplicate of existing b1 in B

		require.Error(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a1", "a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"b1"}, empsOf(t, db, bSeed.ID))

		b.Emps[len(b.Emps)-1].Name = "b2"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"a1", "b1", "b2"}, empsOf(t, db, bSeed.ID))
		idle(t, ses, w)
	})
}

// ================================================================ transactions

// SaveChanges inside a Tx that rolls back, then SaveChanges again outside.
func TestProbeTxRollbackThenSaveAgain(t *testing.T) {
	knownProbe(t, "#38")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)
		a, b := loadAB(t, ses)

		moved := a.Emps[0]
		a.Emps = a.Emps[1:] // a1 leaves A...
		b.Emps = append(b.Emps, moved)
		b.Emps = append(b.Emps, Emp{Name: "extra"})
		a.Emps[0].Name = "a2-renamed"

		err := ses.Tx(ctx, func(tx *ProbeDB) error {
			require.NoError(t, tx.SaveChanges(ctx))

			return errProbeStop
		})
		require.ErrorIs(t, err, errProbeStop)
		assert.Equal(t, []string{"a1", "a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"b1"}, empsOf(t, db, bSeed.ID))
		assert.True(t, ses.HasChanges(), "the rolled back edit is pending again")

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2-renamed", "a3"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"a1", "b1", "extra"}, empsOf(t, db, bSeed.ID))
		assert.EqualValues(t, 5, empCount(t, db), "no duplicated rows")
		idle(t, ses, w)
	})
}

// A graph inserted in a Tx that rolls back can be inserted again.
func TestProbeTxRollbackInsertGraphThenInsertAgain(t *testing.T) {
	knownProbe(t, "#38")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)
		d := &Dept{Name: "T", Emps: []Emp{{Name: "t1"}, {Name: "t2"}}}

		err := ses.Tx(ctx, func(tx *ProbeDB) error {
			require.NoError(t, tx.Depts.Insert(ctx, d))

			return errProbeStop
		})
		require.ErrorIs(t, err, errProbeStop)

		cnt, err := db.Depts.NoTracking().Count(ctx)
		require.NoError(t, err)
		require.Zero(t, cnt, "rolled back")

		assert.Zero(t, d.ID, "rolled back insert should not leave a key behind")
		assert.Zero(t, d.Emps[0].ID, "child keys too")

		require.NoError(t, ses.Depts.Insert(ctx, d))

		assert.Equal(
			t,
			[]string{"t1", "t2"},
			empsOf(t, db, d.ID),
			"children are inserted by the second attempt",
		)
	})
}

// Entities added in a Tx that rolls back, saved via SaveChanges: same.
func TestProbeTxRollbackAddThenSaveAgain(t *testing.T) {
	knownProbe(t, "#38")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)
		d := &Dept{Name: "T2", Emps: []Emp{{Name: "t1"}}}
		require.NoError(t, ant.Add(ses, d))

		err := ses.Tx(ctx, func(tx *ProbeDB) error {
			require.NoError(t, tx.SaveChanges(ctx))

			return errProbeStop
		})
		require.ErrorIs(t, err, errProbeStop)
		assert.Equal(t, ant.Added, ant.Entry(ses, d).State(), "back to pending insert")

		require.NoError(t, ses.SaveChanges(ctx))
		assert.NotZero(t, d.ID)
		assert.Equal(t, []string{"t1"}, empsOf(t, db, d.ID))
		idle(t, ses, w)
	})
}

// Tx commits: the tracker is clean afterwards.
func TestProbeTxCommitCleansTracker(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps = append(a.Emps[:1], a.Emps[2:]...)
		a.Emps[0].Name = "a1-tx"

		require.NoError(t, ses.Tx(ctx, func(tx *ProbeDB) error { return tx.SaveChanges(ctx) }))
		assert.Equal(t, []string{"a1-tx", "a3"}, empsOf(t, db, aSeed.ID))
		idle(t, ses, w)
	})
}

// A rollback after a slice reallocation inside the Tx.
func TestProbeTxRollbackAfterRealloc(t *testing.T) {
	knownProbe(t, "#38")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps = slices.Clip(a.Emps)
		a.Emps = append(a.Emps, Emp{Name: "x1"})
		a.Emps[0].Name = "edited"

		err := ses.Tx(ctx, func(tx *ProbeDB) error {
			require.NoError(t, tx.SaveChanges(ctx))
			// more edits after the first save, in the same tx
			a.Emps = append(a.Emps, Emp{Name: "x2"})
			a.Emps[1].Name = "edited-too"

			require.NoError(t, tx.SaveChanges(ctx))

			return errProbeStop
		})
		require.ErrorIs(t, err, errProbeStop)
		assert.Equal(t, []string{"a1", "a2", "a3"}, empsOf(t, db, aSeed.ID))

		require.NoError(t, ses.SaveChanges(ctx))
		got := empsOf(t, db, aSeed.ID)
		slices.Sort(got)
		assert.Equal(t, []string{"a3", "edited", "edited-too", "x1", "x2"}, got)
		assert.EqualValues(t, 5, countIn(t, db, aSeed.ID))
		idle(t, ses, w)
	})
}

// ================================================================ detach / attach

// Detach a slice child and edit it: the edit is forgotten. Attach assumes the
// current values are the stored ones.
func TestProbeDetachChildThenEditThenAttach(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)
		child := &a.Emps[0]
		id := child.ID

		ant.Detach(ses, child)
		assert.Equal(t, ant.Detached, ant.Entry(ses, child).State())

		child.Name = "ghost"

		w.reset()
		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, w.writes())
		assert.Equal(t, "a1", storedEmp(t, db, id).Name)
		assert.Equal(t, ant.Detached, ant.Entry(ses, child).State(), "a save does not re-attach it")

		require.NoError(t, ant.Attach(ses, child))
		child.Name = "real"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, "real", storedEmp(t, db, id).Name)
		idle(t, ses, w)
	})
}

// A detached child dropped from its parent's slice is nobody's business any
// more: it should not be deleted.
func TestProbeDetachedChildRemovedFromSlice(t *testing.T) {
	knownProbe(t, "#44")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		ant.Detach(ses, &a.Emps[0])
		a.Emps = a.Emps[1:]

		require.NoError(t, ses.SaveChanges(ctx))

		assert.Equal(
			t,
			[]string{"a1", "a2", "a3"},
			empsOf(t, db, aSeed.ID),
			"a detached entity must not be deleted by a relation change",
		)
	})
}

// A detached parent: its children stay tracked and their edits are saved; a
// child dropped from the detached parent's slice is not deleted.
func TestProbeDetachedParentChildrenStillSave(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		ant.Detach(ses, a)
		a.Emps[0].Name = "a1-edit"
		a.Emps = a.Emps[:2]

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a1-edit", "a2", "a3"}, empsOf(t, db, aSeed.ID))
	})
}

// Detach and re-attach the parent: appended children are inserted.
func TestProbeReattachParentThenAppend(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		ant.Detach(ses, a)
		require.NoError(t, ant.Attach(ses, a))

		a.Emps = append(a.Emps, Emp{Name: "a4"})
		a.Emps[0].Name = "a1-edit"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a1-edit", "a2", "a3", "a4"}, empsOf(t, db, aSeed.ID))
		idle(t, ses, w)
	})
}

// Remove then Attach cancels the deletion.
func TestProbeRemoveThenAttachCancels(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		require.NoError(t, ant.Remove(ses, &a.Emps[0]))
		require.NoError(t, ant.Attach(ses, &a.Emps[0]))

		w.reset()
		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, w.writes())
		assert.Equal(t, []string{"a1", "a2", "a3"}, empsOf(t, db, aSeed.ID))
	})
}

// Add then Remove of an entity that was never saved writes nothing and is not
// an error.
func TestProbeAddThenRemoveNewEntity(t *testing.T) {
	knownProbe(t, "#44")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		seedAB(t, db)
		ses := ant.Session(db)

		fresh := &Dept{Name: "ephemeral"}
		require.NoError(t, ant.Add(ses, fresh))
		require.NoError(t, ant.Remove(ses, fresh))

		w.reset()
		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, w.writes())

		n, err := db.Depts.NoTracking().Count(ctx)
		require.NoError(t, err)
		assert.EqualValues(t, 2, n)
	})
}

// Attach the parent only, then put an existing child of another parent in its
// slice: the child's foreign key is moved.
func TestProbeAttachParentAdoptsExistingChild(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		a1, err := db.Emps.NoTracking().Where(ant.Eq(&Em.Name, "a1")).Single(ctx)
		require.NoError(t, err)

		ses := ant.Session(db)
		b := &Dept{ID: bSeed.ID, Name: "B"}
		require.NoError(t, ant.Attach(ses, b))

		b.Emps = []Emp{*a1}

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))

		assert.Equal(
			t,
			[]string{"a1", "b1"},
			empsOf(t, db, bSeed.ID),
			"b1 is untouched: nothing was loaded",
		)
	})
}

// TrackGraph with a body that moves a child under another parent.
func TestProbeTrackGraphMovesChild(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		rows, err := db.Emps.NoTracking().OrderBy(&Em.Name).All(ctx)
		require.NoError(t, err)

		ses := ant.Session(db)

		incoming := &Dept{
			ID:   bSeed.ID,
			Name: "B2",
			Emps: []Emp{
				{ID: rows[0].ID, Name: "a1", Version: 1}, // was in A
				{ID: rows[3].ID, Name: "b1", Version: 1},
				{Name: "brand-new"},
			},
		}

		require.NoError(t, ant.TrackGraph(ses, incoming, func(n ant.GraphNode) ant.State {
			if n.HasKey {
				return ant.Modified
			}

			return ant.Added
		}))

		require.NoError(t, ses.SaveChanges(ctx))

		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"a1", "b1", "brand-new"}, empsOf(t, db, bSeed.ID))

		d, err := db.Depts.NoTracking().Find(ctx, bSeed.ID)
		require.NoError(t, err)
		assert.Equal(t, "B2", d.Name)
		idle(t, ses, w)
	})
}

// ================================================================ many-to-many

func seedBadges(t *testing.T, db *ProbeDB) (x, y, z *Badge) {
	t.Helper()

	x, y, z = &Badge{Name: "x"}, &Badge{Name: "y"}, &Badge{Name: "z"}
	require.NoError(t, db.Badges.InsertOnly(t.Context(), x, y, z))

	return x, y, z
}

func loadDeptBadges(t *testing.T, ses *ProbeDB, id int64) *Dept {
	t.Helper()

	d, err := ses.Depts.Include(&Dp.Badges, func(q *ant.Query[Badge]) *ant.Query[Badge] {
		return q.OrderBy(&Bd.Name)
	}).Find(t.Context(), id)
	require.NoError(t, err)

	return d
}

func TestProbeM2MRemoveAndAppendExisting(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		a, _ := seedAB(t, db)
		x, y, z := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, a, &Dp.Badges, x, y))

		ses := ant.Session(db)
		d := loadDeptBadges(t, ses, a.ID)
		require.Len(t, d.Badges, 2)

		d.Badges = slices.Delete(d.Badges, 0, 1)
		d.Badges = append(d.Badges, *z)

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"y", "z"}, badgesOf(t, db, a.ID))

		n, err := db.Badges.NoTracking().Count(ctx)
		require.NoError(t, err)
		assert.EqualValues(t, 3, n, "unlinked badge row stays")
		idle(t, ses, w)
	})
}

func TestProbeM2MAppendNewBadge(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		a, _ := seedAB(t, db)
		x, _, _ := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, a, &Dp.Badges, x))

		ses := ant.Session(db)
		d := loadDeptBadges(t, ses, a.ID)
		d.Badges = append(d.Badges, Badge{Name: "w"})

		require.NoError(t, ses.SaveChanges(ctx))
		assert.NotZero(t, d.Badges[1].ID)
		assert.Equal(t, []string{"w", "x"}, badgesOf(t, db, a.ID))
		idle(t, ses, w)
	})
}

// ant.Link / ant.Unlink write at once and leave the loaded slice alone; a later
// SaveChanges must not undo them, and must still apply slice edits.
func TestProbeM2MLinkUnlinkThenSave(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		a, _ := seedAB(t, db)
		x, y, z := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, a, &Dp.Badges, x, y))

		ses := ant.Session(db)
		d := loadDeptBadges(t, ses, a.ID)

		require.NoError(t, ant.Link(ctx, ses, d, &Dp.Badges, z))
		require.NoError(t, ant.Unlink(ctx, ses, d, &Dp.Badges, x))

		d.Badges = append(d.Badges, Badge{Name: "w"})

		require.NoError(t, ses.SaveChanges(ctx))

		assert.Equal(
			t,
			[]string{"w", "y", "z"},
			badgesOf(t, db, a.ID),
			"x stays unlinked, z stays linked, w is new",
		)

		idle(t, ses, w)
	})
}

// Unlink a badge and also remove it from the loaded slice.
func TestProbeM2MUnlinkAndRemoveFromSlice(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		a, _ := seedAB(t, db)
		x, y, _ := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, a, &Dp.Badges, x, y))

		ses := ant.Session(db)
		d := loadDeptBadges(t, ses, a.ID)

		require.NoError(t, ant.Unlink(ctx, ses, d, &Dp.Badges, x))
		d.Badges = d.Badges[1:]

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"y"}, badgesOf(t, db, a.ID))
	})
}

// The other side of the relation.
func TestProbeM2MOtherSide(t *testing.T) {
	knownProbe(t, "#39")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		a, b := seedAB(t, db)
		x, _, _ := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, a, &Dp.Badges, x))

		ses := ant.Session(db)
		bx, err := ses.Badges.Include(&Bd.Depts).Find(ctx, x.ID)
		require.NoError(t, err)
		require.Len(t, bx.Depts, 1)

		bx.Depts = append(bx.Depts[:0], Dept{ID: b.ID, Name: "B"})

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, badgesOf(t, db, a.ID))
		assert.Equal(t, []string{"x"}, badgesOf(t, db, b.ID))
		idle(t, ses, w)
	})
}

// A new graph that reaches back through a many-to-many relation. The dept
// inside the badge is a second new dept, so two pivot rows result.
func TestProbeM2MNewGraphBothDirections(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)

		d := &Dept{Name: "cyc"}
		bg := Badge{Name: "bcyc", Depts: []Dept{{Name: "other"}}}
		d.Badges = []Badge{bg}

		require.NoError(t, ses.Depts.Insert(ctx, d))
		assert.Equal(t, []string{"bcyc"}, badgesOf(t, db, d.ID))
		assert.Equal(t, 2, pivotCount(t, db))
	})
}

// After a graph with a many-to-many relation is inserted, a further SaveChanges
// has nothing to do for the join table.
func TestProbeM2MInsertedGraphIsIdle(t *testing.T) {
	knownProbe(t, "#39")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)

		d := &Dept{Name: "m", Badges: []Badge{{Name: "n1"}, {Name: "n2"}}}
		require.NoError(t, ses.Depts.Insert(ctx, d))
		idle(t, ses, w)

		d.Name = "m2"

		w.reset()
		require.NoError(t, ses.SaveChanges(ctx))

		var pivotWrites int

		for _, s := range w.writes() {
			if strings.Contains(s, "dept_badges") {
				pivotWrites++
			}
		}

		assert.Zero(t, pivotWrites, "an unrelated edit re-writes the join table")
	})
}

// A link removed with ant.Unlink must stay removed when a later SaveChanges
// runs for unrelated reasons.
func TestProbeM2MUnlinkedLinkStaysUnlinked(t *testing.T) {
	knownProbe(t, "#39")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)

		d := &Dept{Name: "m", Badges: []Badge{{Name: "n1"}, {Name: "n2"}}}
		require.NoError(t, ses.Depts.Insert(ctx, d))
		require.Equal(t, []string{"n1", "n2"}, badgesOf(t, db, d.ID))

		require.NoError(t, ant.Unlink(ctx, ses, d, &Dp.Badges, &d.Badges[0]))
		require.Equal(t, []string{"n2"}, badgesOf(t, db, d.ID))

		d.Name = "renamed"

		require.NoError(t, ses.SaveChanges(ctx))

		assert.Equal(
			t,
			[]string{"n2"},
			badgesOf(t, db, d.ID),
			"SaveChanges resurrected a link that was removed on purpose",
		)
	})
}

// Overwrite a many-to-many element in place.
func TestProbeM2MReplaceElementInPlace(t *testing.T) {
	knownProbe(t, "#39")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		a, _ := seedAB(t, db)
		x, y, z := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, a, &Dp.Badges, x, y))

		ses := ant.Session(db)
		d := loadDeptBadges(t, ses, a.ID)
		d.Badges[0] = *z // x out, z in

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"y", "z"}, badgesOf(t, db, a.ID))
		idle(t, ses, w)
	})
}

// Overwrite a one-to-many element in place: the old child is deleted and the
// new one (another parent's child) is moved.
func TestProbeReplaceElementInPlace(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)
		a, b := loadAB(t, ses)

		a.Emps[0] = b.Emps[0] // a1 replaced by b1
		b.Emps = b.Emps[:0]

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2", "a3", "b1"}, empsOf(t, db, aSeed.ID))
		assert.Empty(t, empsOf(t, db, bSeed.ID))
		idle(t, ses, w)
	})
}

// Replace the badge slice with copies of the same badges: no pivot churn.
func TestProbeM2MReplaceWithSameBadges(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		a, _ := seedAB(t, db)
		x, y, _ := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, a, &Dp.Badges, x, y))

		ses := ant.Session(db)
		d := loadDeptBadges(t, ses, a.ID)
		d.Badges = []Badge{d.Badges[1], d.Badges[0]}

		w.reset()
		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, w.writes())
		assert.Equal(t, []string{"x", "y"}, badgesOf(t, db, a.ID))
	})
}

// The same unlinked badge twice in the slice is linked once.
func TestProbeM2MDuplicateAppend(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		a, _ := seedAB(t, db)
		_, _, z := seedBadges(t, db)

		ses := ant.Session(db)
		d := loadDeptBadges(t, ses, a.ID)
		d.Badges = append(d.Badges, *z, *z)

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"z"}, badgesOf(t, db, a.ID))
		assert.Equal(t, 1, pivotCount(t, db))
		idle(t, ses, w)
	})
}

// A failed save with an Added dept (existing badge in its slice) and a removed
// dept: nothing is saved, the added dept's key is cleared.
func TestProbeM2MAddedAndRemovedParentsFail(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		a, _ := seedAB(t, db)
		x, y, _ := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, a, &Dp.Badges, x))

		ses := ant.Session(db)
		fresh := &Dept{Name: "F", Badges: []Badge{*y}}
		require.NoError(t, ant.Add(ses, fresh))

		d := loadDeptBadges(t, ses, a.ID)
		require.NoError(t, ant.Remove(ses, d))

		// A has emps in the database, so deleting it must fail on the FK.
		require.Error(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"x"}, badgesOf(t, db, a.ID), "failed save left the link")

		cnt, err := db.Depts.NoTracking().Count(ctx)
		require.NoError(t, err)
		assert.EqualValues(t, 2, cnt, "failed save inserted nothing")
		assert.Zero(t, fresh.ID)
		assert.Equal(t, 1, pivotCount(t, db))
	})
}

// ================================================================ soft delete

func seedCrate(t *testing.T, db *ProbeDB) *Crate {
	t.Helper()
	ctx := t.Context()
	c := &Crate{Name: "c"}
	require.NoError(t, db.Crates.InsertOnly(ctx, c))

	require.NoError(t, db.Gems.InsertOnly(ctx,
		&Gem{CrateID: c.ID, Name: "g1"},
		&Gem{CrateID: c.ID, Name: "g2"},
		&Gem{CrateID: c.ID, Name: "g3"}))

	return c
}

func gemCounts(t *testing.T, db *ProbeDB) (live, deleted, all int64) {
	t.Helper()
	ctx := t.Context()

	var err error

	live, err = db.Gems.NoTracking().Count(ctx)
	require.NoError(t, err)

	deleted, err = db.Gems.OnlyDeleted().NoTracking().Count(ctx)
	require.NoError(t, err)

	all, err = db.Gems.WithDeleted().NoTracking().Count(ctx)
	require.NoError(t, err)

	return live, deleted, all
}

func loadCrate(t *testing.T, ses *ProbeDB, id int64) *Crate {
	t.Helper()

	c, err := ses.Crates.Include(&Cr.Gems, func(q *ant.Query[Gem]) *ant.Query[Gem] {
		return q.OrderBy(&Gm.Name)
	}).Find(t.Context(), id)
	require.NoError(t, err)

	return c
}

// A required child removed from a slice is soft-deleted, not destroyed.
func TestProbeSoftDeleteOrphan(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		c := seedCrate(t, db)
		ses := ant.Session(db)
		crate := loadCrate(t, ses, c.ID)

		removed := crate.Gems[1]
		crate.Gems = slices.Delete(crate.Gems, 1, 2)

		require.NoError(t, ses.SaveChanges(ctx))

		live, deleted, all := gemCounts(t, db)
		assert.EqualValues(t, 2, live)
		assert.EqualValues(t, 1, deleted)
		assert.EqualValues(t, 3, all, "the row is still there")

		gone, err := db.Gems.OnlyDeleted().NoTracking().Single(ctx)
		require.NoError(t, err)
		assert.Equal(t, removed.ID, gone.ID)
		idle(t, ses, w)

		// Reloading skips it.
		again := loadCrate(t, ant.Session(db), c.ID)
		assert.Len(t, again.Gems, 2)
	})
}

// A soft-deleted child that was not loaded is untouched by saves.
func TestProbeSoftDeletedChildNotLoadedUntouched(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		c := seedCrate(t, db)
		_, err := db.Gems.Where(ant.Eq(&Gm.Name, "g2")).Delete(ctx)
		require.NoError(t, err)

		ses := ant.Session(db)
		crate := loadCrate(t, ses, c.ID)
		require.Len(t, crate.Gems, 2)

		crate.Gems = append(crate.Gems, Gem{Name: "g4"})
		crate.Gems[0].Name = "g1-edit"

		require.NoError(t, ses.SaveChanges(ctx))
		live, deleted, all := gemCounts(t, db)
		assert.EqualValues(t, 3, live)
		assert.EqualValues(t, 1, deleted)
		assert.EqualValues(t, 4, all)
		idle(t, ses, w)
	})
}

// A slice loaded WithDeleted holds soft-deleted rows; saving without changes
// writes nothing, and removing one does not fail.
func TestProbeSoftDeletedChildLoadedWithDeleted(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		c := seedCrate(t, db)
		_, err := db.Gems.Where(ant.Eq(&Gm.Name, "g2")).Delete(ctx)
		require.NoError(t, err)

		ses := ant.Session(db)
		crate, err := ses.Crates.Include(&Cr.Gems, func(q *ant.Query[Gem]) *ant.Query[Gem] {
			return q.WithDeleted().OrderBy(&Gm.Name)
		}).Find(ctx, c.ID)
		require.NoError(t, err)
		require.Len(t, crate.Gems, 3)

		w.reset()
		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, w.writes())

		crate.Gems = crate.Gems[2:] // g1, g2 (soft-deleted) leave

		require.NoError(t, ses.SaveChanges(ctx))

		live, deleted, all := gemCounts(t, db)
		assert.EqualValues(t, 1, live)
		assert.EqualValues(t, 2, deleted)
		assert.EqualValues(t, 3, all)
	})
}

// Remove a soft-deletable parent together with its slice.
func TestProbeSoftDeleteParentAndChildren(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		c := seedCrate(t, db)
		ses := ant.Session(db)
		crate := loadCrate(t, ses, c.ID)

		require.NoError(t, ant.Remove(ses, crate))
		crate.Gems = nil

		require.NoError(t, ses.SaveChanges(ctx))
		live, deleted, all := gemCounts(t, db)
		assert.EqualValues(t, 0, live)
		assert.EqualValues(t, 3, deleted)
		assert.EqualValues(t, 3, all)

		_, err := db.Crates.NoTracking().Find(ctx, c.ID)
		assert.ErrorIs(t, err, ant.ErrNotFound)

		n, err := db.Crates.OnlyDeleted().NoTracking().Count(ctx)
		require.NoError(t, err)
		assert.EqualValues(t, 1, n)
		assert.Equal(t, ant.Detached, ant.Entry(ses, crate).State())
		idle(t, ses, w)
	})
}

// Soft-delete the parent alone: children keep their rows and stay editable.
func TestProbeSoftDeleteParentOnly(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		c := seedCrate(t, db)
		ses := ant.Session(db)
		crate := loadCrate(t, ses, c.ID)

		require.NoError(t, ant.Remove(ses, crate))
		require.NoError(t, ses.SaveChanges(ctx))

		live, deleted, _ := gemCounts(t, db)
		assert.EqualValues(t, 3, live)
		assert.EqualValues(t, 0, deleted)

		crate.Gems[0].Name = "still-here"

		require.NoError(t, ses.SaveChanges(ctx))
	})
}

// ================================================================ delete parent

// No database cascade: removing the parent alone fails on the FK and leaves the
// session retryable; clearing the slice fixes it.
func TestProbeRemoveParentTrackedChildrenNoCascade(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		require.NoError(t, ant.Remove(ses, a))
		require.Error(t, ses.SaveChanges(ctx), "FK violation expected")
		assert.Equal(t, []string{"a1", "a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.True(t, ses.HasChanges())

		a.Emps = nil

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, empsOf(t, db, aSeed.ID))

		_, err := db.Depts.NoTracking().Find(ctx, aSeed.ID)
		assert.ErrorIs(t, err, ant.ErrNotFound)
		assert.EqualValues(t, 1, empCount(t, db))
		idle(t, ses, w)
	})
}

// Database cascade: the children vanish with the parent; the stale tracked
// children neither break the save nor hide a later conflict.
func TestProbeRemoveParentTrackedChildrenCascade(t *testing.T) {
	probeWith(t, probeCascadeDDL, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		stale := &a.Emps[0]
		require.NoError(t, ant.Remove(ses, a))
		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, empsOf(t, db, aSeed.ID))
		assert.EqualValues(t, 1, empCount(t, db))
		idle(t, ses, w)

		stale.Name = "zombie"

		err := ses.SaveChanges(ctx)
		assert.ErrorIs(t, err, ant.ErrConcurrency, "the row is gone")
	})
}

// Remove parent and each child separately, in the "wrong" order.
func TestProbeRemoveParentAndChildrenSeparately(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		require.NoError(t, ant.Remove(ses, a))

		for i := range a.Emps {
			require.NoError(t, ant.Remove(ses, &a.Emps[i]))
		}

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, empsOf(t, db, aSeed.ID))

		_, err := db.Depts.NoTracking().Find(ctx, aSeed.ID)
		assert.ErrorIs(t, err, ant.ErrNotFound)
	})
}

// ================================================================ insert graphs with generated keys and cycles

func catNames(t *testing.T, db *ProbeDB) map[string]*int64 {
	t.Helper()
	rows, err := db.Cats.NoTracking().All(t.Context())
	require.NoError(t, err)

	out := map[string]*int64{}

	for _, r := range rows {
		out[r.Name] = r.ParentID
	}

	return out
}

// A three level tree of new rows with pointers both ways.
func TestProbeCatTreeBothDirections(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)

		root := &Cat{Name: "root", Children: []Cat{{Name: "a"}}}
		a := &root.Children[0]
		a.Parent = root
		a.Children = []Cat{{Name: "b"}}
		b := &a.Children[0]
		b.Parent = a

		require.NoError(t, ses.Cats.Insert(ctx, root))

		names := catNames(t, db)
		require.Len(t, names, 3)
		assert.Nil(t, names["root"])
		require.NotNil(t, names["a"])
		assert.Equal(t, root.ID, *names["a"])
		require.NotNil(t, names["b"])
		assert.Equal(t, a.ID, *names["b"])
		idle(t, ses, w)

		b.Name = "b2"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Contains(t, catNames(t, db), "b2")
		idle(t, ses, w)
	})
}

// A failure deep in a tree leaves every key cleared and the tree insertable.
func TestProbeCatTreeFailureAndRetry(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)

		root := &Cat{Name: "root", Children: []Cat{{Name: "a"}, {Name: "a2"}}}
		root.Children[0].Children = []Cat{{Name: "dup"}, {Name: "dup"}} // unique violation

		err := ses.Cats.Insert(ctx, root)
		require.Error(t, err)
		assert.Zero(t, root.ID)
		assert.Zero(t, root.Children[0].ID)
		assert.Zero(t, root.Children[1].ID)
		assert.Zero(t, root.Children[0].Children[0].ID)
		assert.Zero(t, root.Children[0].Children[1].ID)
		assert.Empty(t, catNames(t, db), "nothing saved")
		assert.False(t, ses.HasChanges())

		root.Children[0].Children[1].Name = "dup2"
		require.NoError(t, ses.Cats.Insert(ctx, root))

		names := catNames(t, db)
		require.Len(t, names, 5)
		require.NotNil(t, names["dup2"])
		assert.Equal(t, root.Children[0].ID, *names["dup2"])
		idle(t, ses, w)
	})
}

// The same new parent reached from two new children (a diamond), and listed
// itself in the insert call.
func TestProbeDiamondSharedNewParent(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)

		shared := &Dept{Name: "shared"}
		e1 := &Emp{Name: "e1", Dept: shared}
		e2 := &Emp{Name: "e2", Dept: shared}

		require.NoError(t, ses.Emps.Insert(ctx, e1, e2))

		n, err := db.Depts.NoTracking().Count(ctx)
		require.NoError(t, err)
		assert.EqualValues(t, 1, n)
		assert.Equal(t, shared.ID, e1.DeptID)
		assert.Equal(t, shared.ID, e2.DeptID)
		assert.Equal(t, []string{"e1", "e2"}, empsOf(t, db, shared.ID))
		idle(t, ses, w)
	})
}

// A child inserted through a pointer to a loaded parent whose slice was loaded:
// the stale slice does not make the child look removed.
func TestProbeInsertChildViaPointerOfLoadedParent(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		e := &Emp{Name: "viaptr", Dept: a}
		require.NoError(t, ses.Emps.Insert(ctx, e))
		assert.Equal(t, aSeed.ID, e.DeptID)

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a1", "a2", "a3", "viaptr"}, empsOf(t, db, aSeed.ID))

		e.Name = "viaptr2"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, "viaptr2", storedEmp(t, db, e.ID).Name)
		idle(t, ses, w)
	})
}

// Include nested two levels, edit a grandchild, move a subtree to another root.
func TestProbeNestedIncludeMoveSubtree(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		r1, r2 := &Cat{Name: "r1"}, &Cat{Name: "r2"}
		require.NoError(t, db.Cats.InsertOnly(ctx, r1, r2))

		c1 := &Cat{Name: "c1", ParentID: &r1.ID}
		c2 := &Cat{Name: "c2", ParentID: &r1.ID}
		require.NoError(t, db.Cats.InsertOnly(ctx, c1, c2))

		g1 := &Cat{Name: "g1", ParentID: &c1.ID}
		g2 := &Cat{Name: "g2", ParentID: &c1.ID}
		require.NoError(t, db.Cats.InsertOnly(ctx, g1, g2))

		ses := ant.Session(db)
		roots, err := ses.Cats.Where(ant.In(&Ct.Name, "r1", "r2")).OrderBy(&Ct.Name).
			Include(&Ct.Children, func(q *ant.Query[Cat]) *ant.Query[Cat] {
				return q.OrderBy(&Ct.Name).
					Include(&Ct.Children, func(q *ant.Query[Cat]) *ant.Query[Cat] {
						return q.OrderBy(&Ct.Name)
					})
			}).All(ctx)
		require.NoError(t, err)
		require.Len(t, roots, 2)

		x, y := roots[0], roots[1]
		require.Len(t, x.Children, 2)
		require.Len(t, x.Children[0].Children, 2)

		x.Children[0].Children[1].Name = "g2-edit" // grandchild edit
		sub := x.Children[0]                       // c1 with its subtree
		x.Children = x.Children[1:]
		y.Children = append(y.Children, sub)
		y.Children[0].Children[0].Name = "g1-edit" // edit after the move, via new parent

		require.NoError(t, ses.SaveChanges(ctx))

		names := catNames(t, db)
		require.Len(t, names, 6)
		assert.Equal(t, r2.ID, *names["c1"])
		assert.Equal(t, r1.ID, *names["c2"])
		assert.Equal(t, c1.ID, *names["g1-edit"])
		assert.Equal(t, c1.ID, *names["g2-edit"])
		idle(t, ses, w)
	})
}

// ================================================================ nullable pointers

func seedGrpItems(t *testing.T, db *ProbeDB) (g1, g2 *Grp, i1, i2 *Item) {
	t.Helper()
	ctx := t.Context()
	g1, g2 = &Grp{Name: "g1"}, &Grp{Name: "g2"}
	require.NoError(t, db.Grps.InsertOnly(ctx, g1, g2))

	i1 = &Item{GrpID: &g1.ID, Label: "i1"}
	i2 = &Item{GrpID: &g1.ID, Label: "i2"}
	require.NoError(t, db.Items.InsertOnly(ctx, i1, i2))

	return g1, g2, i1, i2
}

// Zero values and NULL stay distinct through insert and a series of saves.
func TestProbeNullableZeroVersusNull(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)

		zero := &Item{Label: "zero", Note: new(""), Qty: new(0)}
		null := &Item{Label: "null"}
		require.NoError(t, ses.Items.Insert(ctx, zero, null))

		got := storedItem(t, db, zero.ID)
		require.NotNil(t, got.Note)
		require.NotNil(t, got.Qty)
		assert.Equal(t, "", *got.Note)
		assert.Equal(t, 0, *got.Qty)
		assert.Nil(t, got.GrpID)

		got = storedItem(t, db, null.ID)
		assert.Nil(t, got.Note)
		assert.Nil(t, got.Qty)

		// Flip each: zero -> nil, nil -> zero, then through the pointer.
		zero.Note, zero.Qty = nil, nil
		null.Note, null.Qty = new(""), new(0)

		require.NoError(t, ses.SaveChanges(ctx))

		got = storedItem(t, db, zero.ID)
		assert.Nil(t, got.Note)
		assert.Nil(t, got.Qty)

		got = storedItem(t, db, null.ID)
		require.NotNil(t, got.Note)
		require.NotNil(t, got.Qty)
		assert.Equal(t, "", *got.Note)
		assert.Equal(t, 0, *got.Qty)
		idle(t, ses, w)

		*null.Note = "x"
		*null.Qty = 5

		require.NoError(t, ses.SaveChanges(ctx))
		got = storedItem(t, db, null.ID)
		assert.Equal(t, "x", *got.Note)
		assert.Equal(t, 5, *got.Qty)

		*null.Note = ""
		*null.Qty = 0

		require.NoError(t, ses.SaveChanges(ctx))
		got = storedItem(t, db, null.ID)
		require.NotNil(t, got.Note)
		assert.Equal(t, "", *got.Note)
		assert.Equal(t, 0, *got.Qty)
		assert.EqualValues(t, 4, got.Ver, "three saves after the insert")
		idle(t, ses, w)
	})
}

// Replacing a pointer with another pointer to an equal value is not a change;
// two items sharing one pointer both change when it is written through.
func TestProbeNullablePointerIdentity(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)

		i1 := &Item{Label: "p1", Note: new("same")}
		i2 := &Item{Label: "p2", Note: new("same")}
		require.NoError(t, ses.Items.Insert(ctx, i1, i2))
		idle(t, ses, w)

		i1.Note = new("same")

		w.reset()
		require.NoError(t, ses.SaveChanges(ctx))
		assert.Empty(t, w.writes(), "an equal value behind a new pointer")

		shared := new("shared")
		i1.Note, i2.Note = shared, shared

		require.NoError(t, ses.SaveChanges(ctx))

		*shared = "shared-2"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, "shared-2", *storedItem(t, db, i1.ID).Note)
		assert.Equal(t, "shared-2", *storedItem(t, db, i2.ID).Note)
	})
}

// A foreign key pointer to zero is a real value, not NULL: the database must
// refuse it.
func TestProbeNullableFKPointerToZero(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		_, _, i1, _ := seedGrpItems(t, db)
		ses := ant.Session(db)

		it, err := ses.Items.Find(ctx, i1.ID)
		require.NoError(t, err)

		it.GrpID = new(int64) // points at 0

		err = ses.SaveChanges(ctx)
		assert.Error(t, err, "0 is not NULL, there is no group 0")
		assert.NotNil(t, storedItem(t, db, i1.ID).GrpID)
	})
}

// IncludeOne, nil the pointer, save; link again; nil again. Every step must
// take effect.
func TestProbeRelationPointerNilRelinkNil(t *testing.T) {
	knownProbe(t, "#41")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		g1, _, i1, _ := seedGrpItems(t, db)
		ses := ant.Session(db)

		it, err := ses.Items.IncludeOne(&It.Grp).Find(ctx, i1.ID)
		require.NoError(t, err)
		require.NotNil(t, it.Grp)

		it.Grp = nil

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Nil(t, storedItem(t, db, i1.ID).GrpID, "step 1: unlinked")
		assert.Nil(t, it.GrpID)

		grp, err := ses.Grps.Find(ctx, g1.ID)
		require.NoError(t, err)

		it.Grp = grp

		require.NoError(t, ses.SaveChanges(ctx))
		require.NotNil(t, storedItem(t, db, i1.ID).GrpID, "step 2: relinked")
		assert.Equal(t, g1.ID, *storedItem(t, db, i1.ID).GrpID)

		it.Grp = nil

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Nil(t, storedItem(t, db, i1.ID).GrpID, "step 3: unlinked again")
		idle(t, ses, w)
	})
}

// Set the pointer to a new parent right after loading, save, then clear it.
func TestProbeRelationPointerAssignedThenCleared(t *testing.T) {
	knownProbe(t, "#41")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		_, _, i1, _ := seedGrpItems(t, db)
		ses := ant.Session(db)

		it, err := ses.Items.Find(ctx, i1.ID) // Grp not loaded
		require.NoError(t, err)

		it.Grp = &Grp{Name: "fresh"}

		require.NoError(t, ses.SaveChanges(ctx))
		require.NotNil(t, storedItem(t, db, i1.ID).GrpID)
		assert.Equal(t, it.Grp.ID, *storedItem(t, db, i1.ID).GrpID)

		it.Grp = nil

		require.NoError(t, ses.SaveChanges(ctx))

		assert.Nil(
			t,
			storedItem(t, db, i1.ID).GrpID,
			"clearing a pointer that was assigned and saved",
		)
	})
}

// Foreign key and pointer disagree: whichever wins, memory and database agree
// afterwards.
func TestProbeFKAndPointerDisagree(t *testing.T) {
	type setup func(it *Item, g1, g2, g3 *Grp)

	cases := map[string]setup{
		"fk to g2, pointer to g3 (both tracked)": func(it *Item, g1, g2, g3 *Grp) {
			it.GrpID = &g2.ID
			it.Grp = g3
		},
		"fk to g2, pointer nil": func(it *Item, g1, g2, g3 *Grp) {
			it.GrpID = &g2.ID
			it.Grp = nil
		},
		"fk to g2, pointer to a new group": func(it *Item, g1, g2, g3 *Grp) {
			it.GrpID = &g2.ID
			it.Grp = &Grp{Name: "brand new"}
		},
		"fk nil, pointer still g1": func(it *Item, g1, g2, g3 *Grp) {
			it.GrpID = nil
		},
		"pointer to g2 only": func(it *Item, g1, g2, g3 *Grp) {
			it.Grp = g2
		},
	}

	for name, apply := range cases {
		t.Run(name, func(t *testing.T) {
			probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
				ctx := t.Context()
				g1, g2, i1, _ := seedGrpItems(t, db)
				g3 := &Grp{Name: "g3"}
				require.NoError(t, db.Grps.InsertOnly(ctx, g3))

				ses := ant.Session(db)

				it, err := ses.Items.IncludeOne(&It.Grp).Find(ctx, i1.ID)
				require.NoError(t, err)
				require.NotNil(t, it.Grp)

				tg1, err := ses.Grps.Find(ctx, g1.ID)
				require.NoError(t, err)

				tg2, err := ses.Grps.Find(ctx, g2.ID)
				require.NoError(t, err)

				tg3, err := ses.Grps.Find(ctx, g3.ID)
				require.NoError(t, err)

				apply(it, tg1, tg2, tg3)

				require.NoError(t, ses.SaveChanges(ctx))

				stored := storedItem(t, db, i1.ID)

				if it.GrpID == nil {
					assert.Nil(t, stored.GrpID, "memory has no FK, database has %v", stored.GrpID)
				} else if assert.NotNil(t, stored.GrpID, "memory has FK %d, database NULL", *it.GrpID) {
					assert.Equal(t, *it.GrpID, *stored.GrpID, "memory vs database FK")
				}

				if it.Grp != nil && it.GrpID != nil {
					assert.Equal(t, it.Grp.ID, *it.GrpID, "pointer vs FK in memory")
				}

				if it.Grp == nil {
					assert.Nil(t, it.GrpID, "pointer nil but FK still set in memory")
				}

				idle(t, ses, w)
			})
		})
	}
}

// Insert an item whose pointer and FK disagree.
func TestProbeInsertFKAndPointerDisagree(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		g1, g2, _, _ := seedGrpItems(t, db)
		ses := ant.Session(db)

		it := &Item{Label: "both", GrpID: &g1.ID, Grp: g2}
		require.NoError(t, ses.Items.Insert(ctx, it))

		stored := storedItem(t, db, it.ID)
		require.NotNil(t, stored.GrpID)
		require.NotNil(t, it.GrpID)
		assert.Equal(t, *it.GrpID, *stored.GrpID, "memory vs database")
		assert.Equal(t, g2.ID, *stored.GrpID, "pointer wins on insert")
	})
}

// Unlinking an optional child by dropping it from the slice is the same write
// as setting its foreign key to nil: version and updated_at move too.
func TestProbeUnlinkViaSliceStampsLikeDirectEdit(t *testing.T) {
	knownProbe(t, "#44")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		g1, _, i1, i2 := seedGrpItems(t, db)
		before1, before2 := storedItem(t, db, i1.ID), storedItem(t, db, i2.ID)

		ses := ant.Session(db)
		g, err := ses.Grps.Include(&Gp.Items, func(q *ant.Query[Item]) *ant.Query[Item] {
			return q.OrderBy(&It.Label)
		}).Find(ctx, g1.ID)
		require.NoError(t, err)
		require.Len(t, g.Items, 2)

		g.Items[0].GrpID = nil // direct edit of i1 (still in the slice)
		g.Items = g.Items[:1]  // i2 dropped from the slice

		// i1 is both edited and kept; i2 is unlinked via the slice.
		require.NoError(t, ses.SaveChanges(ctx))

		got1, got2 := storedItem(t, db, i1.ID), storedItem(t, db, i2.ID)
		assert.Nil(t, got1.GrpID)
		assert.Nil(t, got2.GrpID)
		assert.Greater(t, got1.Ver, before1.Ver, "direct edit bumps version")
		assert.Greater(t, got2.Ver, before2.Ver, "unlink via slice should bump version too")
		assert.True(t, got1.UpdatedAt.After(before1.UpdatedAt), "direct edit stamps updated_at")

		assert.True(
			t,
			got2.UpdatedAt.After(before2.UpdatedAt),
			"unlink via slice should stamp updated_at too",
		)
	})
}

// A child edited and then dropped from the slice of an optional relation: the
// row survives unlinked; its other edit is not silently thrown away.
func TestProbeEditThenUnlinkKeepsEdit(t *testing.T) {
	knownProbe(t, "#45")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		g1, _, i1, _ := seedGrpItems(t, db)
		ses := ant.Session(db)
		g, err := ses.Grps.Include(&Gp.Items, func(q *ant.Query[Item]) *ant.Query[Item] {
			return q.OrderBy(&It.Label)
		}).Find(ctx, g1.ID)
		require.NoError(t, err)

		g.Items[0].Label = "i1-edited"
		g.Items = g.Items[1:]

		require.NoError(t, ses.SaveChanges(ctx))
		got := storedItem(t, db, i1.ID)
		assert.Nil(t, got.GrpID)
		assert.Equal(t, "i1-edited", got.Label, "the label edit was thrown away")
	})
}

// ================================================================ refetching with pending edits

// A later query that includes a relation on an already tracked parent must not
// silently cancel pending relation edits.
func TestProbeRefetchIncludeKeepsPendingAppend(t *testing.T) {
	knownProbe(t, "#45")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps = append(a.Emps, Emp{Name: "pending"})

		_, _ = loadAB(t, ses) // a second query for the list page

		require.NoError(t, ses.SaveChanges(ctx))

		assert.Contains(
			t,
			empsOf(t, db, aSeed.ID),
			"pending",
			"a pending append vanished after a second Include query",
		)
	})
}

func TestProbeRefetchIncludeKeepsPendingRemoval(t *testing.T) {
	knownProbe(t, "#45")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps = a.Emps[1:]

		again, _ := loadAB(t, ses)
		require.Same(t, a, again)

		require.NoError(t, ses.SaveChanges(ctx))

		assert.Equal(
			t,
			[]string{"a2", "a3"},
			empsOf(t, db, aSeed.ID),
			"a pending removal was cancelled by a second Include query",
		)
	})
}

// Loading the relation again drops the in-memory edit from view; what is shown
// must be what is saved.
func TestProbeLoadTwiceVisibleMatchesSaved(t *testing.T) {
	knownProbe(t, "#45")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)
		id := a.Emps[0].ID

		a.Emps[0].Name = "edited-before-reload"
		require.NoError(t, ses.Depts.Load(ctx, &Dp.Emps, a))

		shown := ""

		for _, e := range a.Emps {
			if e.ID == id {
				shown = e.Name
			}
		}

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, shown, storedEmp(t, db, id).Name, "visible vs saved")
	})
}

// ================================================================ concurrent sessions

// Two sessions move the same child to different parents; the loser must see a
// conflict, not a silent overwrite, and not a lost child.
func TestProbeTwoSessionsMoveSameChild(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		c := &Dept{Name: "C"}
		require.NoError(t, db.Depts.InsertOnly(ctx, c))

		one, two := ant.Session(db), ant.Session(db)
		a1, b1 := loadAB(t, one)
		a2, _ := loadAB(t, two)

		c2, err := two.Depts.Include(&Dp.Emps).Find(ctx, c.ID)
		require.NoError(t, err)

		moved := a1.Emps[0]
		a1.Emps = a1.Emps[1:]
		b1.Emps = append(b1.Emps, moved)

		require.NoError(t, one.SaveChanges(ctx))

		moved2 := a2.Emps[0]
		a2.Emps = a2.Emps[1:]
		c2.Emps = append(c2.Emps, moved2)

		err = two.SaveChanges(ctx)
		assert.ErrorIs(t, err, ant.ErrConcurrency, "the second move is based on stale data")
		assert.Equal(t, []string{"a1", "b1"}, empsOf(t, db, bSeed.ID))
		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.Empty(t, empsOf(t, db, c.ID))
	})
}

// ================================================================ more transactions

// A failed SaveChanges inside a Tx leaves the Tx usable and the changes
// pending, so the call can be repeated (decision 31): one write.
func TestProbeTxFailedSingleWriteThenRetryInsideTx(t *testing.T) {
	knownProbe(t, "#43")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps[1].Name = "a1" // unique (dept_id, name) violation; one UPDATE

		err := ses.Tx(ctx, func(tx *ProbeDB) error {
			require.Error(t, tx.SaveChanges(ctx))

			a.Emps[1].Name = "a2-renamed"

			return tx.SaveChanges(ctx)
		})
		assert.NoError(t, err, "a failed save inside a Tx can be retried inside it")
		assert.Equal(t, []string{"a1", "a2-renamed", "a3"}, empsOf(t, db, aSeed.ID))
	})
}

// The same with several writes in the failed unit.
func TestProbeTxFailedMultiWriteThenRetryInsideTx(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps[0].Name = "a1-renamed"
		a.Emps[1].Name = "a3" // clashes with the current a3
		a.Emps = append(a.Emps, Emp{Name: "a4"})

		err := ses.Tx(ctx, func(tx *ProbeDB) error {
			require.Error(t, tx.SaveChanges(ctx))

			a.Emps[1].Name = "a2-renamed"

			return tx.SaveChanges(ctx)
		})
		assert.NoError(t, err)

		assert.Equal(t,
			[]string{"a1-renamed", "a2-renamed", "a3", "a4"},
			empsOf(t, db, aSeed.ID))

		idle(t, ses, w)
	})
}

// An inner Tx (savepoint) rolls back; the outer commits. The tracker keeps the
// inner edit pending.
func TestProbeTxInnerSavepointRollbackOuterCommit(t *testing.T) {
	knownProbe(t, "#38")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps[0].Name = "outer-edit"

		err := ses.Tx(ctx, func(tx *ProbeDB) error {
			require.NoError(t, tx.SaveChanges(ctx))

			a.Emps[1].Name = "inner-edit"
			a.Emps = append(a.Emps, Emp{Name: "inner-new"})

			inner := tx.Tx(ctx, func(tx2 *ProbeDB) error {
				require.NoError(t, tx2.SaveChanges(ctx))

				return errProbeStop
			})
			require.ErrorIs(t, inner, errProbeStop)

			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"a2", "a3", "outer-edit"}, empsOf(t, db, aSeed.ID))
		assert.True(t, ses.HasChanges(), "the inner edit is pending again")

		require.NoError(t, ses.SaveChanges(ctx))
		got := empsOf(t, db, aSeed.ID)
		slices.Sort(got)
		assert.Equal(t, []string{"a3", "inner-edit", "inner-new", "outer-edit"}, got)
		idle(t, ses, w)
	})
}

// ================================================================ graph inserts: stamps

func TestProbeGraphInsertStampsVersions(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		ses := ant.Session(db)

		g := &Grp{Name: "g", Items: []Item{{Label: "a"}, {Label: "b"}}}
		require.NoError(t, ses.Grps.Insert(ctx, g))

		for i := range g.Items {
			assert.EqualValues(t, 1, g.Items[i].Ver, "item %d in memory", i)
			assert.EqualValues(t, 1, storedItem(t, db, g.Items[i].ID).Ver)
			require.NotNil(t, g.Items[i].GrpID)
			assert.Equal(t, g.ID, *g.Items[i].GrpID)
			assert.False(t, g.Items[i].CreatedAt.IsZero())
		}

		g.Items[1].Label = "b2"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.EqualValues(t, 2, storedItem(t, db, g.Items[1].ID).Ver)
		assert.EqualValues(t, 1, storedItem(t, db, g.Items[0].ID).Ver)
		idle(t, ses, w)
	})
}

// ================================================================ more relation edits

// Replacing children with new objects that have the same unique natural key:
// the old row must go before the new one comes.
func TestProbeReplaceChildWithSameUniqueName(t *testing.T) {
	knownProbe(t, "#45")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps = []Emp{{Name: "a1"}, {Name: "a2"}, {Name: "a3"}} // all brand new objects

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a1", "a2", "a3"}, empsOf(t, db, aSeed.ID))

		for _, e := range a.Emps {
			assert.NotZero(t, e.ID)
		}
	})
}

// A nullable child moved between two parents' slices: one write, version +1.
func TestProbeNullableChildMovedBetweenSlices(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		g1, g2, i1, i2 := seedGrpItems(t, db)
		ses := ant.Session(db)

		groups, err := ses.Grps.Include(&Gp.Items, func(q *ant.Query[Item]) *ant.Query[Item] {
			return q.OrderBy(&It.Label)
		}).OrderBy(&Gp.Name).All(ctx)
		require.NoError(t, err)
		require.Len(t, groups, 2)

		x, y := groups[0], groups[1]
		moved := x.Items[0]
		x.Items = x.Items[1:]
		y.Items = append(y.Items, moved)

		require.NoError(t, ses.SaveChanges(ctx))

		got1, got2 := storedItem(t, db, i1.ID), storedItem(t, db, i2.ID)
		require.NotNil(t, got1.GrpID)
		assert.Equal(t, g2.ID, *got1.GrpID)
		require.NotNil(t, got2.GrpID)
		assert.Equal(t, g1.ID, *got2.GrpID)
		assert.EqualValues(t, 2, got1.Ver)
		assert.EqualValues(t, 1, got2.Ver)
		idle(t, ses, w)
	})
}

// The same child in two loaded parents' slices at once: it is not deleted.
func TestProbeChildInTwoSlices(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)
		a, b := loadAB(t, ses)

		b.Emps = append(b.Emps, a.Emps[0])

		require.NoError(t, ses.SaveChanges(ctx))

		inA, inB := empsOf(t, db, aSeed.ID), empsOf(t, db, bSeed.ID)
		assert.Len(t, inA, 2+btoi(slices.Contains(inA, "a1")), "a1 is in exactly one dept")
		assert.True(t, slices.Contains(inA, "a1") != slices.Contains(inB, "a1"))
		idle(t, ses, w)
	})
}

func btoi(b bool) int {
	if b {
		return 1
	}

	return 0
}

// Two instances of the same parent row (slice copy and pointer) do not make a
// child look removed.
func TestProbeParentLoadedAsSliceElement(t *testing.T) {
	knownProbe(t, "#40")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		x, _, _ := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, aSeed, &Dp.Badges, x))

		ses := ant.Session(db)
		// Dept reached as a slice element of Badge, with its own emps loaded.
		bx, err := ses.Badges.Include(&Bd.Depts, func(q *ant.Query[Dept]) *ant.Query[Dept] {
			return q.Include(&Dp.Emps, byName)
		}).Find(ctx, x.ID)
		require.NoError(t, err)
		require.Len(t, bx.Depts, 1)
		require.Len(t, bx.Depts[0].Emps, 3)

		bx.Depts[0].Emps = bx.Depts[0].Emps[:2]
		bx.Depts[0].Name = "A-renamed"

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a1", "a2"}, empsOf(t, db, aSeed.ID))

		d, err := db.Depts.NoTracking().Find(ctx, aSeed.ID)
		require.NoError(t, err)
		assert.Equal(t, "A-renamed", d.Name)
		idle(t, ses, w)
	})
}

// ================================================================ concurrency of SaveChanges

// Parallel SaveChanges on one session serialise; no statement is repeated.
func TestProbeParallelSaveChangesSameSession(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps[0].Name = "edited"
		a.Emps = append(a.Emps, Emp{Name: "added"})

		var wg sync.WaitGroup

		errs := make(chan error, 4)

		for range 4 {
			wg.Go(func() { errs <- ses.SaveChanges(ctx) })
		}

		wg.Wait()
		close(errs)

		for err := range errs {
			assert.NoError(t, err)
		}

		assert.Equal(t, []string{"a2", "a3", "added", "edited"}, empsOf(t, db, aSeed.ID))
		assert.EqualValues(t, 2, storedEmp(t, db, a.Emps[0].ID).Version)
	})
}

// Edits to the second level of a nested Include: remove and append below a
// slice element (the children of a child).
func TestProbeNestedIncludeRemoveAndAppendBelowSliceElement(t *testing.T) {
	knownProbe(t, "#40")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		r1 := &Cat{Name: "r1"}
		require.NoError(t, db.Cats.InsertOnly(ctx, r1))

		c1 := &Cat{Name: "c1", ParentID: &r1.ID}
		require.NoError(t, db.Cats.InsertOnly(ctx, c1))

		g1, g2 := &Cat{Name: "g1", ParentID: &c1.ID}, &Cat{Name: "g2", ParentID: &c1.ID}
		require.NoError(t, db.Cats.InsertOnly(ctx, g1, g2))

		ses := ant.Session(db)
		root, err := ses.Cats.Include(&Ct.Children, func(q *ant.Query[Cat]) *ant.Query[Cat] {
			return q.Include(&Ct.Children, func(q *ant.Query[Cat]) *ant.Query[Cat] {
				return q.OrderBy(&Ct.Name)
			})
		}).Find(ctx, r1.ID)
		require.NoError(t, err)
		require.Len(t, root.Children, 1)
		require.Len(t, root.Children[0].Children, 2)

		root.Children[0].Children = root.Children[0].Children[:1] // g2 leaves c1

		root.Children[0].Children = append(
			root.Children[0].Children,
			Cat{Name: "g3-new"},
		) // g3 joins

		require.NoError(t, ses.SaveChanges(ctx))

		names := catNames(t, db)
		assert.Nil(t, names["g2"], "g2 unlinked from c1")
		require.NotNil(t, names["g3-new"], "g3 inserted")
		assert.Equal(t, c1.ID, *names["g3-new"])
		require.NotNil(t, names["g1"])
		assert.Equal(t, c1.ID, *names["g1"])
		idle(t, ses, w)
	})
}

// The same one level up: removing from the nested slice of a slice element
// that is itself a parent in a different relation.
func TestProbeNestedIncludeRemoveBelowManyToManyElement(t *testing.T) {
	knownProbe(t, "#40")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		x, _, _ := seedBadges(t, db)
		require.NoError(t, ant.Link(ctx, db, aSeed, &Dp.Badges, x))

		ses := ant.Session(db)
		bx, err := ses.Badges.Include(&Bd.Depts, func(q *ant.Query[Dept]) *ant.Query[Dept] {
			return q.Include(&Dp.Emps, byName)
		}).Find(ctx, x.ID)
		require.NoError(t, err)

		bx.Depts[0].Name = "A-renamed"
		bx.Depts[0].Emps[0].Name = "a1-renamed"
		bx.Depts[0].Emps = bx.Depts[0].Emps[:2]

		require.NoError(t, ses.SaveChanges(ctx))

		d, err := db.Depts.NoTracking().Find(ctx, aSeed.ID)
		require.NoError(t, err)
		assert.Equal(t, "A-renamed", d.Name)
		assert.Equal(t, []string{"a1-renamed", "a2"}, empsOf(t, db, aSeed.ID))
	})
}

// ================================================================ loading styles

// Truncate then append reuses the slot of the removed element.
func TestProbeTruncateThenAppendReusesSlot(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		a.Emps = a.Emps[:1]
		a.Emps = append(a.Emps, Emp{Name: "x"})

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a1", "x"}, empsOf(t, db, aSeed.ID))
		idle(t, ses, w)
	})
}

// An element overwritten by a new struct carrying the same key.
func TestProbeOverwriteElementSameKey(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)
		a, _ := loadAB(t, ses)

		old := a.Emps[0]
		a.Emps[0] = Emp{
			ID:      old.ID,
			DeptID:  old.DeptID,
			Name:    "dto",
			Version: old.Version,
		}

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2", "a3", "dto"}, empsOf(t, db, aSeed.ID))
		assert.EqualValues(t, 2, storedEmp(t, db, old.ID).Version)
		idle(t, ses, w)
	})
}

// Relations loaded with Load (not Include) are tracked the same way.
func TestProbeExplicitLoadThenRemove(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, _ := seedAB(t, db)
		ses := ant.Session(db)

		a, err := ses.Depts.Find(ctx, aSeed.ID)
		require.NoError(t, err)
		require.NoError(t, ses.Depts.Load(ctx, &Dp.Emps, a))
		require.Len(t, a.Emps, 3)

		slices.SortFunc(a.Emps, func(x, y Emp) int { return strings.Compare(x.Name, y.Name) })
		a.Emps = a.Emps[1:]

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))
		idle(t, ses, w)
	})
}

// IncludeJoined loads a parent in the same query; clearing and editing it works.
func TestProbeIncludeJoinedThenNilAndEdit(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		g1, _, i1, i2 := seedGrpItems(t, db)
		ses := ant.Session(db)

		items, err := ses.Items.IncludeJoined(&It.Grp).OrderBy(&It.Label).All(ctx)
		require.NoError(t, err)
		require.Len(t, items, 2)
		require.NotNil(t, items[0].Grp)

		items[0].Grp.Name = "g1-renamed" // edit the joined parent
		items[1].Grp = nil               // unlink

		require.NoError(t, ses.SaveChanges(ctx))

		g, err := db.Grps.NoTracking().Find(ctx, g1.ID)
		require.NoError(t, err)
		assert.Equal(t, "g1-renamed", g.Name)
		assert.NotNil(t, storedItem(t, db, i1.ID).GrpID)
		assert.Nil(t, storedItem(t, db, i2.ID).GrpID)
		idle(t, ses, w)
	})
}

// Move a required child by assigning its parent pointer.
func TestProbeBelongsToPointerMoveRequired(t *testing.T) {
	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		aSeed, bSeed := seedAB(t, db)
		ses := ant.Session(db)

		e, err := ses.Emps.IncludeOne(&Em.Dept).Where(ant.Eq(&Em.Name, "a1")).Single(ctx)
		require.NoError(t, err)
		require.NotNil(t, e.Dept)

		b, err := ses.Depts.Find(ctx, bSeed.ID)
		require.NoError(t, err)

		e.Dept = b

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Equal(t, bSeed.ID, e.DeptID)
		assert.Equal(t, []string{"a2", "a3"}, empsOf(t, db, aSeed.ID))
		assert.Equal(t, []string{"a1", "b1"}, empsOf(t, db, bSeed.ID))
		assert.EqualValues(t, 2, storedEmp(t, db, e.ID).Version)
		idle(t, ses, w)
	})
}

// Removal only (no slot reuse) below a slice element of a nested Include.
func TestProbeNestedIncludeRemoveOnlyBelowSliceElement(t *testing.T) {
	knownProbe(t, "#40")

	probe(t, func(t *testing.T, db *ProbeDB, w *wlog) {
		ctx := t.Context()
		r1 := &Cat{Name: "r1"}
		require.NoError(t, db.Cats.InsertOnly(ctx, r1))

		c1 := &Cat{Name: "c1", ParentID: &r1.ID}
		require.NoError(t, db.Cats.InsertOnly(ctx, c1))

		g1, g2 := &Cat{Name: "g1", ParentID: &c1.ID}, &Cat{Name: "g2", ParentID: &c1.ID}
		require.NoError(t, db.Cats.InsertOnly(ctx, g1, g2))

		ses := ant.Session(db)
		root, err := ses.Cats.Include(&Ct.Children, func(q *ant.Query[Cat]) *ant.Query[Cat] {
			return q.Include(&Ct.Children, func(q *ant.Query[Cat]) *ant.Query[Cat] {
				return q.OrderBy(&Ct.Name)
			})
		}).Find(ctx, r1.ID)
		require.NoError(t, err)

		root.Children[0].Children = root.Children[0].Children[:1] // g2 leaves c1

		require.NoError(t, ses.SaveChanges(ctx))
		assert.Nil(t, catNames(t, db)["g2"], "g2 should be unlinked from c1")
	})
}

// knownProbe skips a probe that fails for a tracked issue, unless
// ANT_RUN_KNOWN is set, as it is when working on that issue.
func knownProbe(t *testing.T, issue string) {
	t.Helper()

	if os.Getenv("ANT_RUN_KNOWN") == "" {
		t.Skipf("known failure, %s; ANT_RUN_KNOWN=1 runs it", issue)
	}
}
