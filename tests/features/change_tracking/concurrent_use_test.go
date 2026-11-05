package change_tracking

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skudra-go/ant"
	"github.com/skudra-go/ant/tests/features/fixture"
)

// hold marks the context.Context of an operation that a holder pauses.
type hold struct{}

// holder pauses the statements of an operation marked with hold until the test
// lets it go, so a second operation can start while the first still runs.
type holder struct {
	mu      sync.Mutex
	started chan struct{}
	let     chan struct{}
}

// Before implements ant.Interceptor.
func (h *holder) Before(ctx context.Context, _ *ant.Statement) (context.Context, error) {
	if ctx.Value(hold{}) == nil {
		return ctx, nil
	}

	h.mu.Lock()
	started, let := h.started, h.let

	select {
	case <-started:
	default:
		close(started)
	}

	h.mu.Unlock()
	<-let

	return ctx, nil
}

// After implements ant.Interceptor.
func (*holder) After(context.Context, *ant.Statement, ant.Outcome) {}

// while starts op in another goroutine, runs during once op is waiting on the
// database, then lets op finish and returns its error. If during fails the test,
// op is still let go and waited for, so it cannot run on into the next test.
func (h *holder) while(t *testing.T, op func(ctx context.Context) error, during func()) error {
	t.Helper()

	h.mu.Lock()
	h.started, h.let = make(chan struct{}), make(chan struct{})
	let := h.let
	h.mu.Unlock()

	done := make(chan error, 1)

	go func() { done <- op(context.WithValue(t.Context(), hold{}, true)) }()

	finished := false

	defer func() {
		if !finished {
			close(let)
			<-done
		}
	}()

	select {
	case <-h.started:
	case err := <-done:
		finished = true

		t.Fatalf("the operation ended before it reached the database: %v", err)
	}

	during()

	finished = true

	close(let)

	return <-done
}

// byTitle orders included posts.
func byTitle(q *ant.Query[Post]) *ant.Query[Post] { return q.OrderBy(&P.Title) }

// postTitles returns the titles of a user's loaded posts.
func postTitles(u *User) []string {
	out := make([]string, len(u.Posts))

	for i, p := range u.Posts {
		out[i] = p.Title
	}

	return out
}

// A context with change tracking is one unit of work. Two operations on it at
// once would load into the same entities while both change them: two requests
// including the same user's posts would each see the other's posts too. So the
// second operation is refused with ErrConcurrentUse. Give each request its own
// ant.Session, and share a context only with tracking off.
func TestConcurrentUse(t *testing.T) {
	fixture.Each(t, func(t *testing.T, target fixture.Target) {
		h := new(holder)
		db := fixture.Open[AppDB](t, target, ant.WithTracking(true), ant.WithInterceptor(h))
		ctx := t.Context()
		alice, bob, _ := fixture.Seed(t, db)
		require.NoError(t, db.Posts.Insert(ctx,
			&Post{UserID: alice.ID, Title: "A1"},
			&Post{UserID: alice.ID, Title: "A2"},
			&Post{UserID: bob.ID, Title: "B1"}))

		load := func(ctx context.Context) error {
			_, err := db.Users.Include(&U.Posts).All(ctx)

			return err
		}

		t.Run("every tracked operation is refused while another runs", func(t *testing.T) {
			stranger := func() *User { return &User{Name: "Dan", Email: "dan@example.com"} }

			ops := []struct {
				name string
				op   func(ctx context.Context) error
			}{
				{"All", func(ctx context.Context) error {
					_, err := db.Users.All(ctx)

					return err
				}},
				{"First", func(ctx context.Context) error {
					_, err := db.Users.OrderBy(&U.Name).First(ctx)

					return err
				}},
				{"Single", func(ctx context.Context) error {
					_, err := db.Users.Where(ant.Eq(&U.Name, "Alice")).Single(ctx)

					return err
				}},
				{"Query.Find", func(ctx context.Context) error {
					_, err := db.Users.Query().Find(ctx, bob.ID)

					return err
				}},
				{"Table.Find of a tracked row", func(ctx context.Context) error {
					_, err := db.Users.Find(ctx, bob.ID)

					return err
				}},
				{"Page", func(ctx context.Context) error {
					_, err := db.Users.OrderBy(&U.ID).Page(ctx, 1, 2)

					return err
				}},
				{"Chunk", func(ctx context.Context) error {
					return db.Users.Query().Chunk(ctx, 2, func([]*User) error { return nil })
				}},
				{"Include", load},
				{"IncludeOne", func(ctx context.Context) error {
					_, err := db.Posts.IncludeOne(&P.User).All(ctx)

					return err
				}},
				{"IncludeJoined", func(ctx context.Context) error {
					_, err := db.Posts.IncludeJoined(&P.User).All(ctx)

					return err
				}},
				{"Load", func(ctx context.Context) error {
					return db.Users.Load(ctx, &U.Posts, &User{Base: ant.Base{ID: bob.ID}})
				}},
				{"LoadOne", func(ctx context.Context) error {
					return db.Posts.LoadOne(ctx, &P.User, &Post{UserID: bob.ID})
				}},
				{"Insert", func(ctx context.Context) error {
					return db.Users.Insert(ctx, stranger())
				}},
				{"Upsert", func(ctx context.Context) error {
					_, err := db.Users.Upsert(ctx, stranger(), ant.OnConflict(&U.Email).DoNothing())

					return err
				}},
				{"DeleteByID", func(ctx context.Context) error {
					return db.Users.DeleteByID(ctx, int64(999999))
				}},
				{"Reload", func(ctx context.Context) error {
					return db.Users.Reload(ctx, &User{Base: ant.Base{ID: bob.ID}})
				}},
				{"SaveChanges", db.SaveChanges},
			}

			for _, tt := range ops {
				t.Run(tt.name, func(t *testing.T) {
					err := h.while(t, load, func() {
						require.ErrorIs(t, tt.op(ctx), ant.ErrConcurrentUse)
					})
					require.NoError(t, err, "the first operation is not disturbed")
				})
			}

			n, err := db.Users.NoTracking().Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 3, n, "no refused write ran")
		})

		t.Run("SaveChanges holds the tracker as a load does", func(t *testing.T) {
			bob.Age = 18

			err := h.while(t, db.SaveChanges, func() {
				require.ErrorIs(t, load(ctx), ant.ErrConcurrentUse)
			})
			require.NoError(t, err)
			require.Equal(t, 18, stored(t, db, bob.ID).Age)
		})

		t.Run("a transaction shares its context's tracker", func(t *testing.T) {
			err := h.while(
				t,
				func(ctx context.Context) error {
					return db.Tx(ctx, func(tx *AppDB) error {
						_, err := tx.Users.All(ctx)

						return err
					})
				},
				func() {
					require.ErrorIs(t, load(ctx), ant.ErrConcurrentUse)
				},
			)
			require.NoError(t, err)
		})

		// A rollback reverses what SaveChanges recorded in the tracker. It does
		// not wait for an operation that holds the tracker, which may be waiting
		// for the rollback's connection: the operation reverses it on release.
		t.Run("a rollback reverses the tracker when the tracker is free", func(t *testing.T) {
			saved, abort := make(chan struct{}), make(chan struct{})
			finished := make(chan error, 1)

			var carol *User

			go func() {
				finished <- db.Tx(ctx, func(tx *AppDB) error {
					var err error

					carol, err = tx.Users.Where(ant.Eq(&U.Name, "Carol")).First(ctx)
					if err != nil {
						return err
					}

					carol.Age = 41

					if err := tx.SaveChanges(ctx); err != nil {
						return err
					}

					close(saved)
					<-abort

					return errors.New("roll back")
				})
			}()

			<-saved

			err := h.while(t, load, func() {
				close(abort)
				require.ErrorContains(t, <-finished, "roll back", "the rollback did not wait")

				require.Equal(
					t,
					ant.Unchanged,
					ant.Entry(db, carol).State(),
					"not while the load holds the tracker",
				)
			})
			require.NoError(t, err)

			require.Equal(t, 41, carol.Age, "the entity keeps its change")
			require.Equal(t, 40, stored(t, db, carol.ID).Age, "the row rolled back")

			require.Equal(
				t,
				ant.Modified,
				ant.Entry(db, carol).State(),
				"and once the load let go, the tracker knows the change is not saved",
			)
		})

		t.Run(
			"a rollback does not wait for a load that waits for its connection",
			func(t *testing.T) {
				one := fixture.Open[AppDB](t, target, ant.WithTracking(true))
				one.DB().SetMaxOpenConns(1)

				done := make(chan error, 1)
				dan := &User{Name: "Dan", Email: "dan.tx@example.com"}

				err := one.Tx(ctx, func(tx *AppDB) error {
					if err := ant.Add(tx, dan); err != nil {
						return err
					}

					if err := tx.SaveChanges(ctx); err != nil {
						return err
					}

					// This load claims the tracker, then waits for the only connection,
					// which the transaction holds until it rolls back.
					go func() {
						_, err := one.Users.All(ctx)
						done <- err
					}()

					time.Sleep(50 * time.Millisecond)

					return errors.New("roll back")
				})
				require.ErrorContains(t, err, "roll back")

				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(10 * time.Second):
					t.Fatal("the load and the rollback waited for each other")
				}

				require.Equal(
					t,
					ant.Added,
					ant.Entry(one, dan).State(),
					"once the load let go, the insert is pending again",
				)
			},
		)

		// A session made inside a transaction has its own tracker, and the
		// rollback reverses that one, after a load on the session lets go of it.
		// The rollback is reversed after a newer insert of the same entity: the
		// newer write stands, so the next save does not insert the row twice.
		t.Run("a late rollback leaves a newer write alone", func(t *testing.T) {
			dan := &User{Name: "Dan", Email: "dan.late@example.com"}
			require.NoError(t, ant.Add(db, dan))

			saved, abort := make(chan struct{}), make(chan struct{})
			finished := make(chan error, 1)

			go func() {
				finished <- db.Tx(ctx, func(tx *AppDB) error {
					if err := tx.SaveChanges(ctx); err != nil {
						return err
					}

					close(saved)
					<-abort

					return errors.New("roll back")
				})
			}()

			<-saved
			require.Equal(t, ant.Unchanged, ant.Entry(db, dan).State())

			err := h.while(
				t,
				func(ctx context.Context) error {
					return db.Users.Insert(ctx, dan)
				},
				func() {
					close(abort)
					require.ErrorContains(t, <-finished, "roll back")
				},
			)
			require.NoError(t, err, "the second insert went in after the rollback")

			require.NoError(t, db.SaveChanges(ctx), "and is not inserted again")
			require.Equal(t, ant.Unchanged, ant.Entry(db, dan).State())

			n, err := db.Users.Where(ant.Eq(&U.Email, dan.Email)).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
		})

		// Loading a relation reads, and writes nothing, so a rollback after it
		// still reverses the insert before it.
		t.Run("a load after a save does not stop the rollback", func(t *testing.T) {
			fay := &User{Name: "Fay", Email: "fay.load@example.com"}
			gus := &User{Name: "Gus", Email: "gus.load@example.com"}

			err := db.Tx(ctx, func(tx *AppDB) error {
				require.NoError(t, ant.Add(tx, fay))
				require.NoError(t, tx.SaveChanges(ctx))
				require.NoError(t, tx.Users.Load(ctx, &U.Posts, fay))

				require.NoError(t, tx.Users.Insert(ctx, gus))
				require.NoError(t, tx.Users.Load(ctx, &U.Posts, gus))

				return errors.New("roll back")
			})
			require.ErrorContains(t, err, "roll back")

			require.Equal(t, ant.Added, ant.Entry(db, fay).State(), "Fay's insert is pending again")
			require.Equal(t, ant.Detached, ant.Entry(db, gus).State(), "Gus was never stored")

			require.NoError(t, db.SaveChanges(ctx), "so the next save inserts Fay")
			require.NotZero(t, stored(t, db, fay.ID).ID)
		})

		// Removing an entity after saving it, in a transaction that then rolls
		// back, still removes it: a new one is forgotten, as its row never was,
		// and a stored one is deleted by the next save.
		t.Run("a removal after a save outlives the rollback", func(t *testing.T) {
			ivy := &User{Name: "Ivy", Email: "ivy.removed@example.com"}
			require.NoError(t, db.Users.Insert(ctx, ivy))

			hal := &User{Name: "Hal", Email: "hal.removed@example.com"}

			err := db.Tx(ctx, func(tx *AppDB) error {
				require.NoError(t, ant.Add(tx, hal))

				ivy.Age = 50

				require.NoError(t, tx.SaveChanges(ctx))

				require.NoError(t, ant.Remove(tx, hal))
				require.NoError(t, ant.Remove(tx, ivy))

				return errors.New("roll back")
			})
			require.ErrorContains(t, err, "roll back")

			require.Equal(t, ant.Detached, ant.Entry(db, hal).State())
			require.Equal(t, ant.Deleted, ant.Entry(db, ivy).State())

			require.NoError(t, db.SaveChanges(ctx))

			n, err := db.Users.Where(ant.In(&U.Email, hal.Email, ivy.Email)).Count(ctx)
			require.NoError(t, err)
			require.Zero(t, n, "Hal was never stored, and Ivy is deleted")
		})

		// A rollback takes back everything done to the tracker inside its
		// transaction, reads included, since they read rows it takes back.
		t.Run(
			"a reload between a save and its rollback does not stop the rollback",
			func(t *testing.T) {
				jo := &User{Name: "Jo", Email: "jo.reload@example.com"}
				require.NoError(t, ant.Add(db, jo))

				err := db.Tx(ctx, func(tx *AppDB) error {
					require.NoError(t, tx.SaveChanges(ctx))
					require.NoError(t, tx.Users.Reload(ctx, jo))

					return errors.New("roll back")
				})
				require.ErrorContains(t, err, "roll back")

				require.Equal(
					t,
					ant.Added,
					ant.Entry(db, jo).State(),
					"the insert is pending again",
				)

				require.NoError(t, db.SaveChanges(ctx))

				n, err := db.Users.Where(ant.Eq(&U.Email, jo.Email)).Count(ctx)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
			},
		)

		t.Run("a reload in a rolled back transaction is taken back", func(t *testing.T) {
			kim := &User{Name: "Kim", Email: "kim.reload@example.com", Age: 1}
			require.NoError(t, db.Users.Insert(ctx, kim))

			err := db.Tx(ctx, func(tx *AppDB) error {
				_, err := tx.Users.Where(ant.Eq(&U.ID, kim.ID)).Update(ctx, ant.Set(&U.Age, 50))
				require.NoError(t, err)
				require.NoError(t, tx.Users.Reload(ctx, kim))
				require.Equal(t, 50, kim.Age)

				return errors.New("roll back")
			})
			require.ErrorContains(t, err, "roll back")

			require.Equal(t, 1, kim.Age, "the value the reload read went with the rows")
			require.Equal(t, ant.Unchanged, ant.Entry(db, kim).State())
			require.Equal(t, 1, stored(t, db, kim.ID).Age)
		})

		t.Run("an edit made after a reload outlives the rollback", func(t *testing.T) {
			max := &User{Name: "Max", Email: "max.reload@example.com", Age: 1}
			require.NoError(t, db.Users.Insert(ctx, max))

			err := db.Tx(ctx, func(tx *AppDB) error {
				require.NoError(t, tx.Users.Reload(ctx, max))

				max.Name = "Maxine"

				return errors.New("roll back")
			})
			require.ErrorContains(t, err, "roll back")
			require.Equal(t, "Maxine", max.Name)
			require.Equal(t, ant.Modified, ant.Entry(db, max).State(), "and is still to be saved")
		})

		t.Run(
			"a relation load between a save and its rollback keeps a removal pending",
			func(t *testing.T) {
				lee := &User{Name: "Lee", Email: "lee.posts@example.com"}
				require.NoError(t, db.Users.Insert(ctx, lee))

				require.NoError(t, db.Posts.Insert(
					ctx,
					&Post{UserID: lee.ID, Title: "L1"},
					&Post{UserID: lee.ID, Title: "L2"},
				))

				require.NoError(t, db.Users.Load(ctx, &U.Posts, lee))
				require.Len(t, lee.Posts, 2)

				lee.Posts = lee.Posts[1:]

				err := db.Tx(ctx, func(tx *AppDB) error {
					require.NoError(t, tx.SaveChanges(ctx))
					require.NoError(t, tx.Users.Load(ctx, &U.Posts, lee))
					require.Len(t, lee.Posts, 1, "inside, the post is gone")

					return errors.New("roll back")
				})
				require.ErrorContains(t, err, "roll back")
				require.Len(t, storedTitles(t, db, lee.ID), 2, "the rollback brought it back")

				require.NoError(t, db.SaveChanges(ctx))
				require.Len(t, storedTitles(t, db, lee.ID), 1, "and the removal was still pending")
			},
		)

		t.Run("a session made in a transaction reverses its own tracker", func(t *testing.T) {
			type opened struct {
				session *AppDB
				bob     *User
			}

			ready, abort := make(chan opened), make(chan struct{})
			finished := make(chan error, 1)

			go func() {
				finished <- db.Tx(ctx, func(tx *AppDB) error {
					session := ant.Session(tx)

					bob, err := session.Users.Where(ant.Eq(&U.Name, "Bob")).First(ctx)
					if err != nil {
						return err
					}

					bob.Age = 99

					if err := session.SaveChanges(ctx); err != nil {
						return err
					}

					ready <- opened{session, bob}

					<-abort

					return errors.New("roll back")
				})
			}()

			o := <-ready

			_ = h.while(
				t,
				func(ctx context.Context) error {
					_, err := o.session.Users.All(ctx) // fails: its transaction is over

					return err
				},
				func() {
					close(abort)
					require.ErrorContains(t, <-finished, "roll back")

					require.Equal(
						t,
						ant.Unchanged,
						ant.Entry(o.session, o.bob).State(),
						"not while the load holds the session's tracker",
					)
				},
			)

			require.Equal(t, ant.Modified, ant.Entry(o.session, o.bob).State())
		})

		t.Run("a transaction from UseTx reverses the tracker", func(t *testing.T) {
			raw, err := db.DB().BeginTx(ctx, nil)
			require.NoError(t, err)

			defer raw.Rollback()

			tx, err := ant.UseTx(db, raw)
			require.NoError(t, err)

			eve := &User{Name: "Eve", Email: "eve.tx@example.com"}

			err = tx.Tx(ctx, func(inner *AppDB) error {
				require.NoError(t, ant.Add(inner, eve))
				require.NoError(t, inner.SaveChanges(ctx))

				return errors.New("roll back to the savepoint")
			})
			require.ErrorContains(t, err, "savepoint")
			require.Equal(t, ant.Added, ant.Entry(db, eve).State(), "the insert is pending again")
			ant.Detach(db, eve)
		})

		t.Run("a context that a finished operation gave out claims nothing", func(t *testing.T) {
			var kept context.Context

			hook := ant.WithInterceptor(keeper{&kept})
			keeping := fixture.Open[AppDB](
				t,
				target,
				ant.WithTracking(true),
				hook,
				ant.WithInterceptor(h),
			)
			_, err := keeping.Users.All(ctx)
			require.NoError(t, err)
			require.NotNil(t, kept)

			old := kept

			err = h.while(
				t,
				func(ctx context.Context) error {
					_, err := keeping.Users.All(ctx)

					return err
				},
				func() {
					_, err := keeping.Users.All(old)
					require.ErrorIs(t, err, ant.ErrConcurrentUse)
				},
			)
			require.NoError(t, err)
		})

		t.Run("work that leaves the tracker alone goes on alongside", func(t *testing.T) {
			count, err := db.Users.Count(ctx)
			require.NoError(t, err)

			err = h.while(t, load, func() {
				n, err := db.Users.Count(ctx)
				require.NoError(t, err)
				require.Equal(t, count, n)

				found, err := db.Users.Where(ant.Eq(&U.Name, "Bob")).Exists(ctx)
				require.NoError(t, err)
				require.True(t, found)

				users, err := db.Users.NoTracking().
					Include(&U.Posts, byTitle).
					OrderBy(&U.Name).
					All(ctx)
				require.NoError(t, err)
				require.Equal(t, []string{"A1", "A2"}, postTitles(users[0]))

				names, err := db.Users.Query().Column(&U.Name).All(ctx)
				require.NoError(t, err)
				require.Len(t, names, int(count), "a partial row is never tracked")

				changed, err := db.Users.Where(ant.Eq(&U.Name, "nobody")).
					Update(ctx, ant.Set(&U.Age, 1))
				require.NoError(t, err)
				require.Zero(t, changed)

				session, err := ant.Session(db).Users.Include(&U.Posts).All(ctx)
				require.NoError(t, err, "a session has a tracker of its own")
				require.Len(t, session, int(count))
			})
			require.NoError(t, err)
		})

		t.Run("the claim ends with the operation, however it ends", func(t *testing.T) {
			canceled, cancel := context.WithCancel(ctx)
			cancel()

			_, err := db.Users.Include(&U.Posts).All(canceled)
			require.ErrorIs(t, err, context.Canceled)

			_, err = db.Users.Where(ant.SQLCond("no_such_column")).All(ctx)
			require.Error(t, err)

			require.NoError(t, load(ctx))
		})

		t.Run("a query inside the operation shares its claim", func(t *testing.T) {
			users, err := db.Users.Include(&U.Posts, func(q *ant.Query[Post]) *ant.Query[Post] {
				return byTitle(q).IncludeOne(&P.User)
			}).OrderBy(&U.Name).All(ctx)
			require.NoError(t, err, "a nested include")
			require.Equal(t, []string{"A1", "A2"}, postTitles(users[0]))
			require.Equal(t, "Alice", users[0].Posts[0].User.Name)

			posts, err := db.Posts.Count(ctx)
			require.NoError(t, err)

			seen := 0
			err = db.Users.OrderBy(&U.ID).Chunk(ctx, 2, func(batch []*User) error {
				for _, u := range batch {
					if err := db.Users.Load(ctx, &U.Posts, u); err != nil {
						return err
					}

					seen += len(u.Posts)
				}

				return nil
			})
			require.NoError(t, err, "Chunk lets go between batches")
			require.EqualValues(t, posts, seen)
		})
	})
}

// keeper keeps the context.Context of the last statement, as a careless
// interceptor might.
type keeper struct{ kept *context.Context }

func (k keeper) Before(ctx context.Context, _ *ant.Statement) (context.Context, error) {
	*k.kept = ctx

	return ctx, nil
}

func (keeper) After(context.Context, *ant.Statement, ant.Outcome) {}

// With tracking off, Add and Attach still put entities in the tracker, so a
// write that may change one of them claims the tracker while it holds any.
func TestConcurrentUseWithTrackingOff(t *testing.T) {
	fixture.Each(t, func(t *testing.T, target fixture.Target) {
		h := new(holder)
		db := fixture.Open[AppDB](t, target, ant.WithInterceptor(h))
		ctx := t.Context()
		alice, bob, _ := fixture.Seed(t, db)

		t.Run("an empty tracker is never claimed", func(t *testing.T) {
			err := h.while(
				t,
				func(ctx context.Context) error {
					return db.Users.Insert(ctx, &User{Name: "Dan", Email: "dan@example.com"})
				},
				func() {
					require.NoError(
						t,
						db.Users.Insert(ctx, &User{Name: "Eve", Email: "eve@example.com"}),
					)

					require.NoError(t, db.Users.Reload(ctx, &User{Base: ant.Base{ID: bob.ID}}))

					require.NoError(
						t,
						db.Users.Load(ctx, &U.Posts, &User{Base: ant.Base{ID: bob.ID}}),
					)
				},
			)
			require.NoError(t, err)
		})

		require.NoError(t, ant.Attach(db, alice))

		ops := []struct {
			name string
			op   func(ctx context.Context) error
		}{
			{"Insert", func(ctx context.Context) error {
				return db.Users.Insert(ctx, &User{Name: "Fay", Email: "fay@example.com"})
			}},
			{"Upsert", func(ctx context.Context) error {
				_, err := db.Users.Upsert(
					ctx,
					&User{Name: "Gus", Email: "gus@example.com"},
					ant.OnConflict(&U.Email).DoNothing(),
				)

				return err
			}},
			{"Reload", func(ctx context.Context) error { return db.Users.Reload(ctx, alice) }},
			{"Load", func(ctx context.Context) error {
				return db.Users.Load(ctx, &U.Posts, alice)
			}},
			{"SaveChanges", db.SaveChanges},
		}

		for _, tt := range ops {
			t.Run("with an attached entity: "+tt.name, func(t *testing.T) {
				alice.Age++

				err := h.while(t, db.SaveChanges, func() {
					require.ErrorIs(t, tt.op(ctx), ant.ErrConcurrentUse)

					users, err := db.Users.All(ctx)
					require.NoError(t, err, "an untracked read claims nothing")
					require.Len(t, users, 5)
				})
				require.NoError(t, err)
			})
		}
	})
}

// Requests that share one context with tracking on get either their own rows or
// ErrConcurrentUse, never another request's. A Session per request, or a shared
// context with tracking off, serves every request.
func TestConcurrentRequests(t *testing.T) {
	const requests = 64

	fixture.Each(t, func(t *testing.T, target fixture.Target) {
		tracked := fixture.Open[AppDB](t, target, ant.WithTracking(true))
		untracked := fixture.Open[AppDB](t, target)

		for _, db := range []*AppDB{tracked, untracked} {
			db.DB().SetMaxOpenConns(8)
		}

		ctx := t.Context()
		alice, bob, carol := fixture.Seed(t, untracked)
		require.NoError(t, untracked.Posts.Insert(ctx,
			&Post{UserID: alice.ID, Title: "A1"},
			&Post{UserID: alice.ID, Title: "A2"},
			&Post{UserID: bob.ID, Title: "B1"}))

		want := map[int64][]string{
			alice.ID: {"A1", "A2"},
			bob.ID:   {"B1"},
			carol.ID: {},
		}

		// request loads every user with posts, then a post with its author, as
		// a handler would. With check, it also reads what it loaded: each
		// relation must hold exactly its rows.
		request := func(db *AppDB, check bool) error {
			users, err := db.Users.Include(&U.Posts, byTitle).All(ctx)
			if err != nil || !check {
				return err
			}

			for _, u := range users {
				if got := postTitles(u); !equalTitles(got, want[u.ID]) {
					return errors.New(u.Name + " has posts " + joinTitles(got))
				}
			}

			posts, err := db.Posts.IncludeOne(&P.User).Where(ant.Eq(&P.Title, "B1")).All(ctx)
			if err != nil {
				return err
			}

			if len(posts) != 1 || posts[0].User == nil || posts[0].User.ID != bob.ID {
				return errors.New("B1 lost its author")
			}

			return nil
		}

		run := func(each func() *AppDB, check bool) (refused int, errs []error) {
			var (
				wg sync.WaitGroup
				mu sync.Mutex
			)

			for range requests {
				wg.Go(func() {
					err := request(each(), check)

					mu.Lock()
					defer mu.Unlock()

					switch {
					case errors.Is(err, ant.ErrConcurrentUse):
						refused++
					case err != nil:
						errs = append(errs, err)
					}
				})
			}

			wg.Wait()

			return refused, errs
		}

		// The requests do not read what they loaded here: one tracking context
		// gives every request the same User for Alice, so reading her posts while
		// the next request includes them again is a data race in the
		// application. That is the reason to share only contexts with tracking off.
		t.Run("one tracking context: refused, never a crash", func(t *testing.T) {
			_, errs := run(func() *AppDB { return tracked }, false)
			require.Empty(t, errs)
		})

		t.Run("a session each: all served", func(t *testing.T) {
			refused, errs := run(func() *AppDB { return ant.Session(tracked) }, true)
			require.Empty(t, errs)
			require.Zero(t, refused)
		})

		t.Run("one context with tracking off: all served", func(t *testing.T) {
			refused, errs := run(func() *AppDB { return untracked }, true)
			require.Empty(t, errs)
			require.Zero(t, refused)
		})

		t.Run("tracked sessions saving at once each save their own", func(t *testing.T) {
			var wg sync.WaitGroup

			errs := make(chan error, requests)

			for i := range requests {
				wg.Go(func() {
					session := ant.Session(tracked)

					users, err := session.Users.Include(&U.Posts).OrderBy(&U.ID).All(ctx)
					if err != nil {
						errs <- err

						return
					}

					users[i%3].Age++
					users[i%3].Posts = append(users[i%3].Posts, Post{Title: "new"})

					errs <- session.SaveChanges(ctx)
				})
			}

			wg.Wait()
			close(errs)

			for err := range errs {
				require.NoError(t, err)
			}

			n, err := untracked.Posts.Where(ant.Eq(&P.Title, "new")).Count(ctx)
			require.NoError(t, err)
			require.EqualValues(t, requests, n, "no post was saved twice or lost")
		})
	})
}

func equalTitles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func joinTitles(titles []string) string {
	out := "["

	for i, title := range titles {
		if i > 0 {
			out += " "
		}

		out += title
	}

	return out + "]"
}

// An entity passed to Load twice gets its related rows once.
func TestLoadRepeatedParent(t *testing.T) {
	fixture.RunTracked(t, func(t *testing.T, db *AppDB) {
		ctx := t.Context()
		alice, _, _ := fixture.Seed(t, db)
		require.NoError(t, db.Posts.Insert(ctx,
			&Post{UserID: alice.ID, Title: "A1"},
			&Post{UserID: alice.ID, Title: "A2"}))

		require.NoError(t, db.Users.Load(ctx, &U.Posts, alice, alice))
		require.Len(t, alice.Posts, 2)

		posts := []*Post{{UserID: alice.ID}, {UserID: alice.ID}}
		require.NoError(t, db.Posts.LoadOne(ctx, &P.User, posts[0], posts[0], posts[1]))
		require.Equal(t, "Alice", posts[0].User.Name)
		require.Equal(t, "Alice", posts[1].User.Name)
	})
}

// Note queries the database from its hooks, as an audit hook might.
type Note struct {
	ID     int64
	Author string
}

// noteHook is what Note's hooks run, set by each test.
var noteHook func(ctx context.Context) error

func (n *Note) AfterLoad(ctx context.Context) error { return runNoteHook(ctx) }

func (n *Note) BeforeUpdate(ctx context.Context) error { return runNoteHook(ctx) }

// running is set while noteHook runs, so the notes its own queries load do not
// run it again, unless reentrant lets them.
var running, reentrant bool

func runNoteHook(ctx context.Context) error {
	if noteHook == nil || running && !reentrant {
		return nil
	}

	running = true

	defer func() { running = false }()

	return noteHook(ctx)
}

// NotesDB is a context for Note.
type NotesDB struct {
	ant.Context
	Notes ant.Table[Note]
}

// A hook runs inside the operation that called it. Its queries share that
// operation's claim when they use the context.Context the hook was given. One
// made with another context looks like a second operation, and is refused.
func TestConcurrentUseInHooks(t *testing.T) {
	const ddl = `CREATE TABLE notes (id bigserial PRIMARY KEY, author text NOT NULL)`

	fixture.RunTrackedWith(t, ddl, func(t *testing.T, db *NotesDB) {
		ctx := t.Context()
		t.Cleanup(func() { noteHook = nil })

		require.NoError(t, db.Notes.Insert(ctx, &Note{Author: "ann"}, &Note{Author: "ben"}))

		others := func(ctx context.Context) error {
			_, err := db.Notes.Where(ant.Eq(&N.Author, "ben")).All(ctx)

			return err
		}

		t.Run("with the hook's context", func(t *testing.T) {
			noteHook = others

			notes, err := db.Notes.OrderBy(&N.Author).All(ctx)
			require.NoError(t, err, "AfterLoad inside a load")

			notes[0].Author = "anne"

			require.NoError(t, db.SaveChanges(ctx), "BeforeUpdate inside SaveChanges")
		})

		t.Run("through two contexts and back", func(t *testing.T) {
			session := ant.Session(db)
			step := 0
			reentrant = true

			t.Cleanup(func() { reentrant = false })

			noteHook = func(ctx context.Context) error {
				step++

				switch step {
				case 1:
					_, err := session.Notes.All(ctx)

					return err
				case 2:
					_, err := db.Notes.All(ctx)

					return err
				}

				return nil
			}

			_, err := db.Notes.All(ctx)
			require.NoError(t, err, "db, then the session, then db again")
			require.Greater(t, step, 2)
		})

		t.Run("with another context", func(t *testing.T) {
			noteHook = func(context.Context) error { return others(context.Background()) }

			_, err := db.Notes.All(ctx)
			require.ErrorIs(t, err, ant.ErrConcurrentUse)

			noteHook = nil

			require.NoError(t, others(ctx), "the refused load let go")
		})
	})
}

// N is the probe for Note.
var N = ant.Fields[Note]()
