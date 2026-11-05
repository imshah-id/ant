// Package track implements the change tracker: identity map, snapshots, and
// diffing. Entities are held by pointer, so the tracker keeps them alive until
// it is cleared.
package track

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/skudra-go/ant/internal/meta"
)

// State is the lifecycle state of an entity.
type State int

// Entity states, matching EF Core's.
const (
	Detached State = iota
	Added
	Unchanged
	Modified
	Deleted
)

// String returns the state's name.
func (s State) String() string {
	return [...]string{"Detached", "Added", "Unchanged", "Modified", "Deleted"}[s]
}

// Entry is one tracked entity.
type Entry struct {
	Model  *meta.Model
	Entity unsafe.Pointer
	// State is Added, Unchanged, Modified, or Deleted. Unchanged means nothing
	// has been marked. Changed tells whether the fields differ.
	State    State
	snapshot unsafe.Pointer
	id       identity
	keyed    bool
	seq      uint64
	saves    uint64 // counts writes saved, so an undo can tell it came too late
	// Members holds the entries of a loaded one-to-many relation by key, and
	// Parents the key of a loaded many-to-one relation, to detect changes.
	Members map[*meta.Relation]map[any]*Entry
	Parents map[*meta.Relation]any
}

// Refresh makes the current values the snapshot.
func (e *Entry) Refresh() { e.snapshot = snapshotOf(e.Model, e.Entity) }

// Changed returns the stored fields that differ from the snapshot. The key is
// never included.
func (e *Entry) Changed() []*meta.Field {
	if e.snapshot == nil {
		return nil
	}

	var out []*meta.Field

	for _, f := range e.Model.Updatable() {
		if !equal(f, e.Entity, e.snapshot) {
			out = append(out, f)
		}
	}

	return out
}

// KeyChanged reports whether the primary key differs from the snapshot.
func (e *Entry) KeyChanged() bool {
	if e.snapshot == nil {
		return false
	}

	return slices.ContainsFunc(
		e.Model.Keys,
		func(f *meta.Field) bool { return !equal(f, e.Entity, e.snapshot) },
	)
}

// Holds reports whether the entity at entity is this entry's row. Slices reuse
// memory, so an address alone does not say which row it holds.
func (e *Entry) Holds(entity unsafe.Pointer) bool {
	if !e.keyed {
		return true
	}

	key, ok := e.Model.KeyOf(entity)

	return ok && key == e.id.key
}

// Original returns the snapshot's copy of the entity, or nil if there is none.
func (e *Entry) Original() unsafe.Pointer { return e.snapshot }

// Scratch returns a private copy of the snapshot that a caller may change.
func (e *Entry) Scratch() unsafe.Pointer {
	if e.snapshot == nil {
		return nil
	}

	return snapshotOf(e.Model, e.snapshot)
}

// Saved records a state and snapshot as saved, and returns how to undo that.
func (e *Entry) Saved(state State) (undo func()) {
	oldState, oldSnapshot := e.State, e.snapshot
	e.State = state
	e.Refresh()

	return e.undoSave(func() { e.State, e.snapshot = oldState, oldSnapshot })
}

// SetMembers records the entries a loaded one-to-many relation holds. The maps
// are replaced, never changed, so an undo keeps the ones it saw.
func (e *Entry) SetMembers(r *meta.Relation, members map[any]*Entry) {
	next := maps.Clone(e.Members)
	if next == nil {
		next = map[*meta.Relation]map[any]*Entry{}
	}

	next[r] = members
	e.Members = next
}

// SetParent records the key a loaded many-to-one relation holds.
func (e *Entry) SetParent(r *meta.Relation, key any) {
	next := maps.Clone(e.Parents)
	if next == nil {
		next = map[*meta.Relation]any{}
	}

	next[r] = key
	e.Parents = next
}

// SetLinks replaces what the loaded relations hold, and returns how to undo that.
func (e *Entry) SetLinks(
	members map[*meta.Relation]map[any]*Entry,
	parents map[*meta.Relation]any,
) (undo func()) {
	oldMembers, oldParents := e.Members, e.Parents
	e.Members, e.Parents = members, parents

	// This undo always applies. A load made inside the same transaction read
	// rows the rollback takes back, and one made outside it read the committed
	// rows, which are what this restores.
	return func() { e.Members, e.Parents = oldMembers, oldParents }
}

// Reloaded records an entry as read back from the database over before, a copy
// of the entity as it was, and returns how to undo that, so that a rollback
// takes the read back with the rows it read: the state and snapshot, and each
// stored value the read replaced, unless it was changed since, which stands.
func (e *Entry) Reloaded(before unsafe.Pointer) (undo func()) {
	oldState, oldSnapshot := e.State, e.snapshot
	e.State = Unchanged
	e.Refresh()
	read := e.snapshot

	return e.undoSave(func() {
		for _, f := range e.Model.Stored() {
			if equal(f, e.Entity, read) {
				f.Value(e.Entity).Set(f.Value(before))
			}
		}

		e.State, e.snapshot = oldState, oldSnapshot
	})
}

// undoSave returns restore for a rollback of a saved write, made safe against a
// later one. A rollback can be reversed late, once another operation has let go
// of the tracker, and that operation may have saved the entry since. Its write
// then stands, and an entry it left unchanged becomes Modified: neither write
// knew what the other left in the database, so the next save writes every
// column. Only saves, and reloads, count, and those made inside the same
// transaction record undos of their own, which run first: loading a relation, or
// Add and Remove in memory, do not make a rollback late.
//
// Undos run newest first, and each puts the count back to what it was before its
// write, so the one before it still finds the entry as it left it.
func (e *Entry) undoSave(restore func()) func() {
	e.saves++
	saves := e.saves

	return func() {
		switch {
		case e.saves == saves:
			restore()

			e.saves = saves - 1
		case e.State == Unchanged:
			e.State = Modified
			e.saves++
		}
	}
}

type identity struct {
	model *meta.Model
	key   any
}

// Tracker holds the tracked entities of one context. It is safe for concurrent
// use, but an entity should not change while SaveChanges reads it.
type Tracker struct {
	mu      sync.Mutex
	held    chan struct{} // holds a token while an operation claims the tracker
	owner   atomic.Pointer[Claim]
	undo    []func() // rollback work waiting for the claim, under mu
	seq     uint64
	entries map[unsafe.Pointer]*Entry
	first   map[identity]*Entry
	// OnTrack and OnState report entities starting to be tracked and changing
	// state. They run with the tracker locked, so they must not call back into it.
	OnTrack func(*Entry)
	OnState func(e *Entry, from, to State)
}

// Claim is one operation's hold on a tracker. Operations that load into the
// tracker or save from it write into the entities it holds, so two at once would
// write into the same ones.
type Claim struct {
	_ byte // a zero-size value could share its address with another
}

// TryClaim reserves the tracker for one operation, or returns nil if another
// holds it.
func (t *Tracker) TryClaim() *Claim {
	select {
	case t.held <- struct{}{}:
		return t.own()
	default:
		return nil
	}
}

// own makes a new claim the owner. Rollback work that was queued before it ran
// first, so the operation starts from what the tracker should hold.
func (t *Tracker) own() *Claim {
	c := new(Claim)
	t.owner.Store(c)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.letGo()

			panic(recovered)
		}
	}()

	t.runUndo()

	return c
}

// Owns reports whether c is the claim that holds the tracker now. A claim that
// was released no longer counts, whoever kept it.
func (t *Tracker) Owns(c *Claim) bool { return c != nil && t.owner.Load() == c }

// Release ends c's hold on the tracker, first running any rollback work that
// waited for it. It lets go even if that work panics, so the tracker cannot stay
// held. Releasing it twice does nothing.
func (t *Tracker) Release(c *Claim) {
	for t.Owns(c) {
		func() {
			defer t.letGo()

			t.runUndo()
		}()

		// Work queued after runUndo found the tracker held, so it is run here,
		// unless another operation has the tracker by now and will run it.
		if !t.hasUndo() {
			return
		}

		c = t.TryClaim()
	}
}

func (t *Tracker) letGo() {
	t.owner.Store(nil)
	<-t.held
}

// Undo runs rollback work that writes to tracked entries. It needs the claim, but
// a rollback must not wait for one: the operation that holds it may be waiting
// for the rollback's connection. So if another operation holds the tracker, the
// work runs when that operation releases it.
func (t *Tracker) Undo(f func()) {
	t.mu.Lock()
	t.undo = append(t.undo, f)
	t.mu.Unlock()

	if c := t.TryClaim(); c != nil {
		t.Release(c)
	}
}

func (t *Tracker) hasUndo() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.undo) > 0
}

// runUndo runs the queued rollback work in order. The work calls back into the
// tracker, so it runs without the lock.
func (t *Tracker) runUndo() {
	t.mu.Lock()
	work := t.undo
	t.undo = nil
	t.mu.Unlock()

	for _, f := range work {
		f()
	}
}

// Empty reports whether nothing is tracked.
func (t *Tracker) Empty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.entries) == 0
}

// New returns an empty tracker.
func New() *Tracker {
	return &Tracker{
		held:    make(chan struct{}, 1),
		entries: map[unsafe.Pointer]*Entry{},
		first:   map[identity]*Entry{},
	}
}

func keyOf(m *meta.Model, entity unsafe.Pointer) (identity, bool) {
	v, ok := m.KeyOf(entity)

	return identity{m, v}, ok
}

func (t *Tracker) add(m *meta.Model, entity unsafe.Pointer, state State) *Entry {
	t.seq++

	e := &Entry{
		Model:  m,
		Entity: entity,
		State:  state,
		seq:    t.seq,
	}

	if state != Added {
		e.Refresh()
	}

	t.entries[entity] = e
	t.identify(e)

	if t.OnTrack != nil {
		t.OnTrack(e)
	}

	return e
}

// identify records the entry's key, if it has one yet. An entity's memory can
// later hold another row, as when a slice shifts, so the key is kept here.
func (t *Tracker) identify(e *Entry) {
	if id, ok := keyOf(e.Model, e.Entity); ok {
		e.id, e.keyed = id, true

		if _, seen := t.first[id]; !seen {
			t.first[id] = e
		}
	}
}

// Load tracks a loaded entity and returns the instance to use: an already
// tracked one with the same key, or entity itself. Models with no key are not
// tracked.
func (t *Tracker) Load(m *meta.Model, entity unsafe.Pointer) unsafe.Pointer {
	t.mu.Lock()
	defer t.mu.Unlock()

	id, ok := keyOf(m, entity)
	if !ok {
		return entity
	}

	if existing, seen := t.first[id]; seen {
		return existing.Entity
	}

	return t.add(m, entity, Unchanged).Entity
}

// Copy tracks a loaded value that lives inside a relation slice. Such a value
// cannot be swapped for an existing instance, so it gets its own entry, and
// changes to either are saved.
func (t *Tracker) Copy(m *meta.Model, entity unsafe.Pointer) *Entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(m.Keys) == 0 {
		return nil
	}

	return t.put(m, entity, Unchanged)
}

// Track starts tracking entity in the given state, or changes its state.
func (t *Tracker) Track(m *meta.Model, entity unsafe.Pointer, state State) *Entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.put(m, entity, state)
}

func (t *Tracker) put(m *meta.Model, entity unsafe.Pointer, state State) *Entry {
	if e, ok := t.entries[entity]; ok {
		if e.State != state && t.OnState != nil {
			t.OnState(e, e.State, state)
		}

		e.State = state

		return e
	}

	return t.add(m, entity, state)
}

// Saved records e as saved in state, and returns how to undo that if the write
// rolls back. A removal asked for after the save outlives the undo: an entry
// that was new before it is forgotten, since its row goes with the rollback, and
// any other stays Deleted, as its row comes back.
func (t *Tracker) Saved(e *Entry, state State) (undo func()) {
	return t.keepRemoval(e, e.Saved(state))
}

// Reloaded is Entry.Reloaded, keeping a removal asked for after the reload as
// Saved does.
func (t *Tracker) Reloaded(e *Entry, before unsafe.Pointer) (undo func()) {
	return t.keepRemoval(e, e.Reloaded(before))
}

// keepRemoval wraps an undo so that a removal asked for after the change it
// undoes survives it: an entry that was new before is forgotten, since its row
// goes with the rollback, and any other stays Deleted.
func (t *Tracker) keepRemoval(e *Entry, restore func()) func() {
	return func() {
		removed := e.State == Deleted

		restore()

		switch {
		case !removed:
		case e.State == Added:
			t.Forget(e)
		default:
			e.State = Deleted
		}
	}
}

// Added tracks a written entity, and returns how to undo that if the write rolls
// back: stop tracking it, unless something changed it since.
func (t *Tracker) Added(m *meta.Model, entity unsafe.Pointer) (undo func()) {
	t.mu.Lock()
	e := t.put(m, entity, Unchanged)
	t.mu.Unlock()

	return e.undoSave(func() { t.Remove(entity) })
}

// Rekey records the key of an entity that received one on insert.
func (t *Tracker) Rekey(e *Entry) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.identify(e)
}

// Move records that an entry's entity now lives at a new address, as happens
// when appending to a relation slice reallocates it.
func (t *Tracker) Move(e *Entry, entity unsafe.Pointer) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.entries[e.Entity] == e {
		delete(t.entries, e.Entity)
	}

	e.Entity = entity
	t.entries[entity] = e
}

// Get returns the entry for an entity, or nil.
func (t *Tracker) Get(entity unsafe.Pointer) *Entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.entries[entity]
}

// Find returns the first tracked instance with the given key, or nil.
func (t *Tracker) Find(m *meta.Model, key any) *Entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.first[identity{m, key}]
}

// Remove stops tracking an entity.
func (t *Tracker) Remove(entity unsafe.Pointer) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if e, ok := t.entries[entity]; ok {
		t.forget(e)
	}
}

// Forget stops tracking an entry, which may no longer own its address, and
// returns how to undo that.
func (t *Tracker) Forget(e *Entry) (undo func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.forget(e)

	return func() { t.Restore(e) }
}

// ForgetRow stops tracking every instance of a row, and returns how to undo that.
func (t *Tracker) ForgetRow(m *meta.Model, key any) (undo func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	id := identity{m, key}

	var gone []*Entry

	for _, e := range t.entries {
		if e.keyed && e.id == id {
			gone = append(gone, e)
		}
	}

	for _, e := range gone {
		t.forget(e)
	}

	return func() {
		for _, e := range gone {
			t.Restore(e)
		}
	}
}

func (t *Tracker) forget(e *Entry) {
	if t.entries[e.Entity] == e {
		delete(t.entries, e.Entity)
	}

	if e.keyed && t.first[e.id] == e {
		delete(t.first, e.id)
	}
}

// Restore tracks an entry again, undoing Forget, unless another entry has taken
// its address since.
func (t *Tracker) Restore(e *Entry) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if other, ok := t.entries[e.Entity]; ok && other != e {
		return
	}

	t.entries[e.Entity] = e

	if e.keyed {
		if _, seen := t.first[e.id]; !seen {
			t.first[e.id] = e
		}
	}
}

// Clear stops tracking everything.
func (t *Tracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()

	clear(t.entries)
	clear(t.first)
}

// Entries returns every entry, oldest first.
func (t *Tracker) Entries() []*Entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]*Entry, 0, len(t.entries))

	for _, e := range t.entries {
		out = append(out, e)
	}

	slices.SortFunc(out, func(a, b *Entry) int { return cmp.Compare(a.seq, b.seq) })

	return out
}

// Len returns the number of tracked entities.
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.entries)
}
