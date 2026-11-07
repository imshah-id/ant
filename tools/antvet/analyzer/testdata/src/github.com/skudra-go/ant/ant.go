// Package ant is a stub with the signatures the analyzer looks at.
package ant

import (
	"context"
	"time"
)

type Cond struct{}

func Fields[T any]() *T                           { return new(T) }
func Eq[V comparable](field *V, v V) Cond         { return Cond{} }
func Gt[V any](field *V, v V) Cond                { return Cond{} }
func EqCol[V comparable](a, b *V) Cond            { return Cond{} }
func AnyOf[V comparable](v V, fields ...*V) Cond  { return Cond{} }
func NoneOf[V comparable](v V, fields ...*V) Cond { return Cond{} }
func Exists(q any) Cond                           { return Cond{} }
func Has[R any](rel *[]R, conds ...Cond) Cond     { return Cond{} }
func And(c ...Cond) Cond                          { return Cond{} }
func Or(c ...Cond) Cond                           { return Cond{} }
func Not(c Cond) Cond                             { return Cond{} }
func Past(field *time.Time) Cond                  { return Cond{} }

type Sub[V any] struct{}

func SubOf[V any](q any, field *V) Sub[V]    { return Sub[V]{} }
func InSub[V any](field *V, sub Sub[V]) Cond { return Cond{} }

type Expr[V any] struct{}

func Col[V any](field *V) Expr[V] { return Expr[V]{} }

type Assignment struct{}

func Set[V any](field *V, v V) Assignment { return Assignment{} }

type Mapping struct{}

func Into[V any](dst *V, src *V) Mapping { return Mapping{} }

type Query[T any] struct{}

func (q *Query[T]) Where(c ...Cond) *Query[T]                     { return q }
func (q *Query[T]) Join() *Query[T]                               { return q }
func (q *Query[T]) JoinUsing[J, V any](field *V) *Join2[T, J]     { return nil }
func (q *Query[T]) LeftJoin[J any](rel **J) *Join2[T, J]          { return nil }
func (q *Query[T]) All(ctx context.Context) ([]*T, error)         { return nil, nil }
func (q *Query[T]) Count(ctx context.Context) (int64, error)      { return 0, nil }
func (q *Query[T]) OrderBy[V any](field *V) *Query[T]             { return q }
func (q *Query[T]) Column[V any](field *V) *Query[T]              { return q }
func (q *Query[T]) With[V any](field *V, value Expr[V]) *Query[T] { return q }
func (q *Query[T]) IncludeJoined[R any](rel **R) *Query[T]        { return q }
func (q *Query[T]) Update(ctx context.Context, a ...Assignment) (int64, error) {
	return 0, nil
}

func (q *Query[T]) Include[R any](rel *[]R, shape ...func(*Query[R]) *Query[R]) *Query[T] {
	return q
}

func (q *Query[T]) IncludeOne[R any](rel **R, shape ...func(*Query[R]) *Query[R]) *Query[T] {
	return q
}

func (q *Query[T]) When(ok bool, f func(*Query[T]) *Query[T]) *Query[T] { return q }

func (q *Query[T]) GroupBy[V any](field *V) *Grouped { return nil }

type Grouped struct{}

func (g *Grouped) Having(c ...Cond) *Grouped { return g }

type Table[T any] struct{}

func (t *Table[T]) Where(c ...Cond) *Query[T]         { return nil }
func (t *Table[T]) OrderBy[V any](field *V) *Query[T] { return nil }
func (t *Table[T]) Find[K comparable](ctx context.Context, key K) (*T, error) {
	return nil, nil
}

type Join2[A, B any] struct{}

func (j *Join2[A, B]) Where(c ...Cond) *Join2[A, B]                  { return j }
func (j *Join2[A, B]) OrderBy[V any](field *V) *Join2[A, B]          { return j }
func (j *Join2[A, B]) Select[R any](f func(*A, *B) R) *Projection[R] { return nil }
func (j *Join2[A, B]) JoinOn[C any](on ...Cond) *Join3[A, B, C]      { return nil }

type Join3[A, B, C any] struct{}

func (j *Join3[A, B, C]) JoinOn[D any](on ...Cond) *Join4[A, B, C, D] { return nil }

type Join4[A, B, C, D any] struct{}

func (j *Join4[A, B, C, D]) Where(c ...Cond) *Join4[A, B, C, D] { return j }
func (j *Join4[A, B, C, D]) Select[R any](f func(*A, *B, *C, *D) R) *Projection[R] {
	return nil
}

func JoinSub[T, J any](q *Query[T], sub *Query[J], on ...Cond) *Join2[T, J]     { return nil }
func JoinLateral[T, J any](q *Query[T], sub *Query[J], on ...Cond) *Join2[T, J] { return nil }

type Projection[R any] struct{}

func (p *Projection[R]) All(ctx context.Context) ([]R, error) { return nil, nil }

func Project[R any](q any, mappings ...Mapping) *Projection[R] { return nil }

func SumOf[T any, V any](ctx context.Context, q *Query[T], field *V) (V, error) {
	var v V

	return v, nil
}

func Pluck[T, V any](ctx context.Context, q *Query[T], field *V) ([]V, error) { return nil, nil }

type Merger[T any] struct{}

func Merge[T, S any](t *Table[T], source *Query[S], on ...Cond) *Merger[T] { return nil }

func (m *Merger[T]) WhenMatched(set ...Assignment) *Merger[T] { return m }
func (m *Merger[T]) Exec(ctx context.Context) (int64, error)  { return 0, nil }

// Add takes an entity, not a field.
func Add[T any](h any, entity *T) error { return nil }

type FieldRef struct{}

func F[V any](field *V) FieldRef { return FieldRef{} }

type ModelBuilder[T any] struct{}

func (b *ModelBuilder[T]) Table(name string)               {}
func (b *ModelBuilder[T]) Key[V any](field *V) *KeyBuilder { return nil }
func (b *ModelBuilder[T]) CompositeKey(fields ...FieldRef) {}
func (b *ModelBuilder[T]) NoKey()                          {}

type KeyBuilder struct{}

func (k *KeyBuilder) NotGenerated() *KeyBuilder { return k }

func GenerateKey[E, V any](b *ModelBuilder[E], field *V, fn func() V) {}
