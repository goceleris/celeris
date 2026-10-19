package fix

// A struct with an unexported field cannot be written as an unkeyed composite
// literal outside its package; one without can. The line marks the first kind,
// so the first unexported field added to the second shows.

// AllExp has only exported fields.
type AllExp struct {
	A int
	B string
}

// OneUnexp has one unexported field.
type OneUnexp struct {
	A int
	b int
}

// OnlyUnexp has no exported field.
type OnlyUnexp struct{ a int }

// BlankFld has a blank field, which no other package can write either.
type BlankFld struct {
	A int
	_ struct{}
}

type quiet struct{ N int }

// EmbUnexp embeds a type with an unexported name.
type EmbUnexp struct {
	quiet
	A int
}

// EmbExp embeds an exported type, and has only exported fields.
type EmbExp struct {
	AllExp
	C int
}

// GenUnexp is generic, with an unexported field.
type GenUnexp[T any] struct {
	V T
	n int
}

// EmptyStruct has no field at all.
type EmptyStruct struct{}
