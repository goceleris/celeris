// Package hid is internal: no golden file, exposed where the API reaches it.
package hid

type Hidden struct {
	X     int
	Inner *More
}

func (Hidden) M() {}

type More struct{ Y string }

type Unreached struct{ Z int }
