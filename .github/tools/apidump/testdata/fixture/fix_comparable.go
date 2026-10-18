package fix

// Comparable structs print nothing; one that is not comparable says so. A
// blank or unexported field decides it as much as an exported one.

// Cmp is comparable.
type Cmp struct {
	A int
	B string
	C *Cmp
	D [2]Kind
	E any
}

// NotCmpFunc holds a function.
type NotCmpFunc struct {
	A int
	F func()
}

// NotCmpBlank has no exported field of a type that cannot be compared: the
// zero-length array of functions in the blank field does it.
type NotCmpBlank struct {
	A int
	_ [0]func()
}

// NotCmpUnexported hides a slice in an unexported field.
type NotCmpUnexported struct {
	A int
	s []int
}

// NotCmpNested is not comparable through a field of a struct type.
type NotCmpNested struct{ N NotCmpFunc }

// NotCmpArray is not comparable through an array element.
type NotCmpArray struct{ M [2]map[string]int }

// NotCmpEmbedded is not comparable through an embedded field.
type NotCmpEmbedded struct{ NotCmpUnexported }

// GenCmp is comparable for any comparable T.
type GenCmp[T any] struct{ V T }

// GenNotCmp is not comparable whatever T is.
type GenNotCmp[T any] struct {
	V T
	F func(T)
}
