package track

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/skudra-go/ant/internal/meta"
)

type address struct {
	City string
	Tags []string
}

type row struct {
	ID    int64
	Name  string
	Bio   *string
	Data  []byte
	At    time.Time
	Tags  []string
	Home  address
	Attrs map[string]string `ant:"column=attrs"`
	Peers []row
}

func fresh() *row {
	return &row{
		ID:    1,
		Name:  "a",
		Bio:   new("b"),
		Data:  []byte("c"),
		Tags:  []string{"d"},
		At:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Home:  address{City: "e", Tags: []string{"f"}},
		Attrs: map[string]string{"g": "h"},
	}
}

func changed(e *Entry) []string {
	var out []string

	for _, f := range e.Changed() {
		out = append(out, f.Short)
	}

	return out
}

func TestChanged(t *testing.T) {
	cases := []struct {
		name   string
		change func(*row)
		want   string
	}{
		{"nothing", func(*row) {}, ""},
		{"plain field", func(r *row) { r.Name = "x" }, "Name"},
		{"set to zero", func(r *row) { r.Name = "" }, "Name"},
		{"pointer replaced", func(r *row) { r.Bio = new("x") }, "Bio"},
		{"pointer replaced with equal value", func(r *row) { r.Bio = new("b") }, ""},
		{"pointer set to nil", func(r *row) { r.Bio = nil }, "Bio"},
		{"written through pointer", func(r *row) { *r.Bio = "x" }, "Bio"},
		{"bytes edited in place", func(r *row) { r.Data[0] = 'x' }, "Data"},
		{"slice edited in place", func(r *row) { r.Tags[0] = "x" }, "Tags"},
		{"slice appended", func(r *row) { r.Tags = append(r.Tags, "x") }, "Tags"},
		{"map edited in place", func(r *row) { r.Attrs["g"] = "x" }, "Attrs"},
		{"nested struct field", func(r *row) { r.Home.City = "x" }, "Home"},
		{"slice inside struct", func(r *row) { r.Home.Tags[0] = "x" }, "Home"},
		{
			"same instant in another zone",
			func(r *row) { r.At = r.At.In(time.FixedZone("x", 3600)) },
			"",
		},
		{"another instant", func(r *row) { r.At = r.At.Add(time.Second) }, "At"},
		{"the key is not a change", func(r *row) { r.ID = 2 }, ""},
		{"a relation is not a change", func(r *row) { r.Peers = []row{{}} }, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := fresh()
			e := New().Track(meta.Of[row](), unsafe.Pointer(r), Unchanged)
			c.change(r)

			got := ""

			if names := changed(e); len(names) > 0 {
				got = names[0]
			}

			if got != c.want {
				t.Errorf("changed = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSavedAndUndo(t *testing.T) {
	r := fresh()
	e := New().Track(meta.Of[row](), unsafe.Pointer(r), Unchanged)
	r.Name = "x"

	undo := e.Saved(Unchanged)

	if len(e.Changed()) != 0 {
		t.Error("a saved value still counts as changed")
	}

	undo()

	if got := changed(e); len(got) != 1 || got[0] != "Name" {
		t.Errorf("after undo: %v", got)
	}
}

func TestLoadReturnsTheFirstInstance(t *testing.T) {
	tr, m := New(), meta.Of[row]()

	a, b := fresh(), fresh()
	if got := tr.Load(m, unsafe.Pointer(a)); got != unsafe.Pointer(a) {
		t.Fatal("first load replaced")
	}

	if got := tr.Load(m, unsafe.Pointer(b)); got != unsafe.Pointer(a) {
		t.Fatal("second load not resolved to the first")
	}

	if tr.Len() != 1 {
		t.Errorf("len = %d", tr.Len())
	}
}

func TestCopyKeepsBothInstances(t *testing.T) {
	tr, m := New(), meta.Of[row]()
	a, b := fresh(), fresh()
	tr.Load(m, unsafe.Pointer(a))
	tr.Copy(m, unsafe.Pointer(b))

	if tr.Len() != 2 {
		t.Fatalf("len = %d", tr.Len())
	}

	if got := tr.Find(m, int64(1)); got == nil || got.Entity != unsafe.Pointer(a) {
		t.Error("identity moved to the copy")
	}
}

func TestRemoveMovesNothingElse(t *testing.T) {
	tr, m := New(), meta.Of[row]()
	a := fresh()
	tr.Track(m, unsafe.Pointer(a), Unchanged)
	tr.Remove(unsafe.Pointer(a))

	if tr.Len() != 0 || tr.Find(m, int64(1)) != nil {
		t.Error("entry survived Remove")
	}

	tr.Remove(unsafe.Pointer(a)) // a second Remove is harmless
}

func TestNoKeyIsNotTracked(t *testing.T) {
	type keyless struct{ Name string }

	tr, m := New(), meta.Of[keyless]()

	v := &keyless{"a"}
	if got := tr.Load(m, unsafe.Pointer(v)); got != unsafe.Pointer(v) || tr.Len() != 0 {
		t.Error("keyless entity was tracked")
	}
}

func TestEntriesAreOldestFirst(t *testing.T) {
	tr, m := New(), meta.Of[row]()

	for i := range int64(50) {
		r := fresh()
		r.ID = 100 - i
		tr.Track(m, unsafe.Pointer(r), Unchanged)
	}

	entries := tr.Entries()
	for i := 1; i < len(entries); i++ {
		if entries[i-1].seq >= entries[i].seq {
			t.Fatal("out of order")
		}
	}
}

func TestMoveFollowsTheEntity(t *testing.T) {
	tr, m := New(), meta.Of[row]()
	a := fresh()
	e := tr.Track(m, unsafe.Pointer(a), Unchanged)
	b := *a
	b.Name = "changed in the new location"
	tr.Move(e, unsafe.Pointer(&b))

	if tr.Get(unsafe.Pointer(a)) != nil || tr.Get(unsafe.Pointer(&b)) != e {
		t.Error("the address map did not move")
	}

	if got := changed(e); len(got) != 1 || got[0] != "Name" {
		t.Errorf("changed = %v", got)
	}
}

func TestForgetUsesTheStoredKey(t *testing.T) {
	tr, m := New(), meta.Of[row]()
	a := fresh()
	e := tr.Track(m, unsafe.Pointer(a), Unchanged)
	a.ID = 99 // memory now describes another row

	tr.Forget(e)

	if tr.Find(m, int64(1)) != nil || tr.Len() != 0 {
		t.Error("entry survived Forget")
	}
}

func TestScratchIsPrivate(t *testing.T) {
	tr, m := New(), meta.Of[row]()
	a := fresh()
	e := tr.Track(m, unsafe.Pointer(a), Unchanged)
	scratch := (*row)(e.Scratch())
	*scratch.Bio = "x"
	scratch.Name = "x"

	if len(e.Changed()) != 0 {
		t.Error("changing the scratch copy changed the snapshot")
	}
}

func TestKeyChanged(t *testing.T) {
	a := fresh()

	e := New().Track(meta.Of[row](), unsafe.Pointer(a), Unchanged)
	if e.KeyChanged() {
		t.Error("key reported changed")
	}

	a.ID = 2

	if !e.KeyChanged() {
		t.Error("key change missed")
	}
}

// Rollback work runs at once on a free tracker, and otherwise when the operation
// that holds it lets go, in the order it came.
func TestUndoWaitsForTheClaim(t *testing.T) {
	tr := New()

	var ran []int

	tr.Undo(func() { ran = append(ran, 1) })

	if len(ran) != 1 {
		t.Fatalf("on a free tracker the work runs at once, ran %v", ran)
	}

	c := tr.TryClaim()
	tr.Undo(func() { ran = append(ran, 2) })
	tr.Undo(func() { ran = append(ran, 3) })

	if len(ran) != 1 {
		t.Fatalf("while held the work waits, ran %v", ran)
	}

	if tr.TryClaim() != nil {
		t.Fatal("a second claim succeeded")
	}

	tr.Release(c)
	tr.Release(c)

	if fmt.Sprint(ran) != "[1 2 3]" {
		t.Fatalf("the release ran %v", ran)
	}

	if c := tr.TryClaim(); c == nil {
		t.Fatal("the tracker stayed held")
	} else if tr.Owns(c) {
		tr.Release(c)
	}
}

// However claims and rollbacks interleave, every piece of work runs once.
func TestUndoUnderContention(t *testing.T) {
	tr := New()

	var (
		wg  sync.WaitGroup
		ran atomic.Int64
	)

	for range 50 {
		wg.Go(func() {
			for range 100 {
				if c := tr.TryClaim(); c != nil {
					tr.Release(c)
				}

				tr.Undo(func() { ran.Add(1) })
			}
		})
	}

	wg.Wait()

	if got := ran.Load(); got != 5000 {
		t.Fatalf("ran %d of 5000", got)
	}
}

// A claim runs rollback work that was queued while the tracker was free, which
// happens when the work arrives as the last holder lets go.
func TestClaimRunsQueuedUndo(t *testing.T) {
	tr := New()
	ran := false

	tr.mu.Lock()
	tr.undo = append(tr.undo, func() { ran = true })
	tr.mu.Unlock()

	c := tr.TryClaim()
	defer tr.Release(c)

	if !ran {
		t.Fatal("the claim came back before the queued work ran")
	}
}

// An undo restores what its change replaced, unless the entry changed since:
// then the newer change stands, and an unchanged entry becomes Modified, so the
// next save writes every column.
func TestUndoAfterALaterChange(t *testing.T) {
	m := meta.OfType(reflect.TypeFor[row]())
	tr := New()
	r := &row{ID: 1, Name: "a"}
	e := tr.Track(m, unsafe.Pointer(r), Added)

	t.Run("in time, it restores", func(t *testing.T) {
		undo := e.Saved(Unchanged)
		undo()

		if e.State != Added {
			t.Fatalf("state %s", e.State)
		}
	})

	t.Run("newest first, each finds the entry as it left it", func(t *testing.T) {
		saved := e.Saved(Unchanged)
		links := e.SetLinks(nil, nil)
		links()
		saved()

		if e.State != Added {
			t.Fatalf("state %s", e.State)
		}
	})

	t.Run("too late, the newer write stands", func(t *testing.T) {
		undo := e.Saved(Unchanged)
		e.Saved(Unchanged) // a newer save, by another operation
		undo()

		if e.State != Modified {
			t.Fatalf("state %s, want Modified", e.State)
		}
	})

	t.Run("loading a relation since does not make it late", func(t *testing.T) {
		e := New().Track(m, unsafe.Pointer(&row{ID: 2}), Added)
		undo := e.Saved(Unchanged)
		e.SetMembers(nil, nil)
		e.SetParent(nil, int64(1))
		undo()

		if e.State != Added {
			t.Fatalf("state %s, want Added", e.State)
		}
	})

	t.Run("a later load does not stop the relations coming back", func(t *testing.T) {
		e := New().Track(m, unsafe.Pointer(&row{ID: 3}), Unchanged)
		e.SetLinks(nil, nil)

		undo := e.SetLinks(map[*meta.Relation]map[any]*Entry{nil: {}}, nil)
		e.SetMembers(nil, map[any]*Entry{})
		undo()

		if e.Members != nil {
			t.Fatal("the rollback left the relations as a later load had them")
		}
	})

	t.Run("a reload undoes with the values it replaced", func(t *testing.T) {
		r := &row{ID: 4, Name: "before"}
		e := New().Track(m, unsafe.Pointer(r), Added)

		saved := e.Saved(Unchanged)
		reloaded := e.Reloaded(func() { r.Name = "before" })
		r.Name = "read"

		reloaded()
		saved()

		if e.State != Added || r.Name != "before" {
			t.Fatalf("state %s, name %q, want Added and before", e.State, r.Name)
		}
	})
}

// Restore does not take back an address another entry holds now.
func TestRestoreKeepsANewerEntry(t *testing.T) {
	m := meta.OfType(reflect.TypeFor[row]())
	tr := New()
	r := &row{ID: 1}
	old := tr.Track(m, unsafe.Pointer(r), Unchanged)
	undo := tr.Forget(old)

	newer := tr.Track(m, unsafe.Pointer(r), Added)

	undo()

	if tr.Get(unsafe.Pointer(r)) != newer {
		t.Fatal("the forgotten entry took the address back")
	}
}

// A removal asked for after a save outlives the save's rollback.
func TestRemovalOutlivesRollback(t *testing.T) {
	m := meta.OfType(reflect.TypeFor[row]())

	t.Run("a new row is forgotten, since it was never stored", func(t *testing.T) {
		tr := New()
		r := &row{ID: 1}
		e := tr.Track(m, unsafe.Pointer(r), Added)
		undo := tr.Saved(e, Unchanged)
		tr.Track(m, unsafe.Pointer(r), Deleted)
		undo()

		if tr.Get(unsafe.Pointer(r)) != nil {
			t.Fatalf("still tracked as %s", e.State)
		}
	})

	t.Run("a stored row stays to be deleted", func(t *testing.T) {
		tr := New()
		r := &row{ID: 2}
		e := tr.Track(m, unsafe.Pointer(r), Modified)
		undo := tr.Saved(e, Unchanged)
		tr.Track(m, unsafe.Pointer(r), Deleted)
		undo()

		if e.State != Deleted {
			t.Fatalf("state %s, want Deleted", e.State)
		}
	})

	t.Run("without a removal, the save is undone", func(t *testing.T) {
		tr := New()
		r := &row{ID: 3}
		e := tr.Track(m, unsafe.Pointer(r), Added)
		undo := tr.Saved(e, Unchanged)
		undo()

		if e.State != Added {
			t.Fatalf("state %s, want Added", e.State)
		}
	})
}

// Rollback work that panics still lets the tracker go.
func TestPanickingUndoLetsGo(t *testing.T) {
	tr := New()
	c := tr.TryClaim()
	tr.Undo(func() { panic("broken undo") })

	func() {
		defer func() { _ = recover() }()

		tr.Release(c)
	}()

	if c := tr.TryClaim(); c == nil {
		t.Fatal("the tracker stayed held after a panic in Release")
	} else {
		tr.Release(c)
	}

	tr.mu.Lock()
	tr.undo = append(tr.undo, func() { panic("broken undo") })
	tr.mu.Unlock()

	func() {
		defer func() { _ = recover() }()

		tr.TryClaim()
	}()

	if c := tr.TryClaim(); c == nil {
		t.Fatal("the tracker stayed held after a panic in a claim")
	} else {
		tr.Release(c)
	}
}
