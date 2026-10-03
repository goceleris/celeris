// Package fix exercises every line form apidump writes.
package fix

import (
	"io"
	"time"

	"example.com/fix/internal/hid"
)

const (
	Untyped      = 1 << 20
	Typed   Kind = iota
	Second
	Ratio      = 0.75
	Name       = "fix"
	Long       = "0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789+"
	unexported = 7
)

// Kind is a defined integer type with value methods.
type Kind uint8

func (Kind) String() string { return "" }

var (
	ErrX     error
	Handlers map[string]func(w io.Writer, n int) (int, error)
	Ch       chan<- (<-chan int)
	Arr      [4]byte
	Anon     struct {
		A int `json:"a"`
		b string
	}
	Empty  interface{}
	Ptr    *hid.Hidden
	hidden int
)

// Variadic has no parameter names in the golden file.
func Variadic(prefix string, rest ...int) (n int, err error) { return 0, nil }

// Map is generic.
func Map[T any, U comparable](in []T, f func(T) U) []U { return nil }

// Number is a type-set constraint.
type Number interface {
	~int | ~int64 | float64
}

// Sum uses the constraint, and an inline one.
func Sum[N Number, M interface{ ~uint }](xs ...N) N { var z N; return z }

// Base is embedded by Outer.
type Base struct {
	Shared int
	Own    string
}

func (Base) BaseMethod() {}

type inner struct {
	Promoted int
	Shared   string // shadowed by Base.Shared? no: same depth, ambiguous
	deep
}

type deep struct{ Deeper bool }

func (*inner) InnerMethod() {}

// Outer has every kind of field.
type Outer struct {
	Base
	inner
	*hid.Hidden
	time.Time
	Exported  int
	unexpFlag bool
	A, B      []string
}

func (o *Outer) PtrMethod(x int) error { return nil }
func (o Outer) ValMethod()             {}
func (o Outer) unexpMethod()           {}

// Iface embeds io.Reader and has an unexported method.
type Iface interface {
	io.Reader
	Close() error
	sealed()
}

// Box is a generic type with methods.
type Box[T any] struct {
	Val T
}

func (b *Box[T]) Get() T                { return b.Val }
func (b Box[T]) Map(f func(T) T) Box[T] { return b }

// Aliases.
type (
	HiddenAlias = hid.Hidden
	LocalAlias  = local
	TimeAlias   = time.Time
	BoxInt      = Box[int]
)

type local struct{ Visible int }

func (local) Visible2() {}

// NewLocal returns an unexported type.
func NewLocal() *local { return nil }

// Func is a function type.
type Func func(ctx any, xs ...string) (bool, error)
