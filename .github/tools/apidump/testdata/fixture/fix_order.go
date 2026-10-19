package fix

// Where an unkeyed composite literal can build a struct outside its package
// (two or more fields, all exported), the order of the fields is API. The
// field lines are sorted, so the order has a line of its own.

// Ordered has three exported fields.
type Ordered struct {
	A int
	B string
	C bool
}

// Grouped declares two fields in one line, and embeds an exported type.
type Grouped struct {
	X, Y int
	Base
	Z []byte
}

// SoloField has one field: the order is moot.
type SoloField struct{ Only int }

// PrivOrdered has an unexported field, so no unkeyed literal of it compiles
// elsewhere and the order is not API.
type PrivOrdered struct {
	A int
	B string
	c bool
}

// GenOrdered is generic.
type GenOrdered[T any, U any] struct {
	First  T
	Second U
}
