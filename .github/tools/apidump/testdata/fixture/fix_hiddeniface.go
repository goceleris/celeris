package fix

import (
	"io"
	"time"
)

// Methods that a type gets from another module or the standard library
// through an embedded exported type are that package's API, and the embedded
// line names the type. Through an unexported interface nothing names it, so
// its methods are listed.

type hiddenReader interface{ io.Reader }

// HidIface embeds an unexported interface of the standard library's io.Reader.
type HidIface struct {
	hiddenReader
	N int
}

type hiddenNested struct{ hiddenReader }

// HidNested reaches the same method through two unexported types.
type HidNested struct{ hiddenNested }

// ExpIface embeds io.Reader itself: the embedded line names it, no method
// line is written.
type ExpIface struct{ io.Reader }

type hiddenTime struct{ time.Time }

// HidStruct reaches time.Time's methods through an unexported struct: they are
// time's API and a Go release that adds one must not change these files.
type HidStruct struct{ hiddenTime }

// ExpStruct embeds time.Time itself.
type ExpStruct struct{ time.Time }

type hiddenPtrReader struct{ *hiddenReaderBox }

type hiddenReaderBox struct{ io.ReadCloser }

// HidPtr reaches io.ReadCloser through an unexported pointer to an unexported
// struct that embeds an exported interface.
type HidPtr struct{ hiddenPtrReader }
