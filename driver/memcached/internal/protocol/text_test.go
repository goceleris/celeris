package protocol

import (
	"bytes"
	"errors"
	"testing"
)

func TestTextReaderSingleLineReplies(t *testing.T) {
	cases := []struct {
		in   string
		want Kind
	}{
		{"STORED\r\n", KindStored},
		{"NOT_STORED\r\n", KindNotStored},
		{"EXISTS\r\n", KindExists},
		{"NOT_FOUND\r\n", KindNotFound},
		{"DELETED\r\n", KindDeleted},
		{"TOUCHED\r\n", KindTouched},
		{"OK\r\n", KindOK},
		{"END\r\n", KindEnd},
		{"ERROR\r\n", KindError},
	}
	for _, c := range cases {
		r := NewTextReader()
		r.Feed([]byte(c.in))
		got, err := r.Next()
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got.Kind != c.want {
			t.Fatalf("%q: got kind %v want %v", c.in, got.Kind, c.want)
		}
	}
}

func TestTextReaderServerError(t *testing.T) {
	r := NewTextReader()
	r.Feed([]byte("SERVER_ERROR out of memory\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindServerError || string(v.Data) != "out of memory" {
		t.Fatalf("got %#v", v)
	}
}

func TestTextReaderClientError(t *testing.T) {
	r := NewTextReader()
	r.Feed([]byte("CLIENT_ERROR bad command line\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindClientError || string(v.Data) != "bad command line" {
		t.Fatalf("got %#v", v)
	}
}

func TestTextReaderValue(t *testing.T) {
	r := NewTextReader()
	r.Feed([]byte("VALUE foo 0 5\r\nhello\r\nEND\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindValue || string(v.Key) != "foo" || string(v.Data) != "hello" || v.Flags != 0 {
		t.Fatalf("got %#v", v)
	}
	end, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if end.Kind != KindEnd {
		t.Fatalf("expected END, got %v", end.Kind)
	}
}

func TestTextReaderValueWithCAS(t *testing.T) {
	r := NewTextReader()
	r.Feed([]byte("VALUE foo 7 5 42\r\nhello\r\nEND\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindValue || v.CAS != 42 || v.Flags != 7 {
		t.Fatalf("got %#v", v)
	}
}

func TestTextReaderIncomplete(t *testing.T) {
	r := NewTextReader()
	r.Feed([]byte("VALUE foo 0 5\r\nhel"))
	_, err := r.Next()
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
	// Feeding the rest completes the parse.
	r.Feed([]byte("lo\r\nEND\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindValue || string(v.Data) != "hello" {
		t.Fatalf("got %#v", v)
	}
}

func TestTextReaderNumber(t *testing.T) {
	r := NewTextReader()
	r.Feed([]byte("42\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindNumber || v.Int != 42 {
		t.Fatalf("got %#v", v)
	}
}

func TestTextReaderVersion(t *testing.T) {
	r := NewTextReader()
	r.Feed([]byte("VERSION 1.6.21\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindVersion || string(v.Data) != "1.6.21" {
		t.Fatalf("got %#v", v)
	}
}

func TestTextReaderStats(t *testing.T) {
	r := NewTextReader()
	r.Feed([]byte("STAT pid 42\r\nSTAT uptime 100\r\nEND\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindStat || string(v.Key) != "pid" || string(v.Data) != "42" {
		t.Fatalf("got %#v", v)
	}
	v, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindStat || string(v.Key) != "uptime" || string(v.Data) != "100" {
		t.Fatalf("got %#v", v)
	}
}

func TestTextWriterStorage(t *testing.T) {
	w := NewTextWriter()
	buf := w.AppendStorage("set", "foo", 7, 60, []byte("hello"), 0, false)
	want := "set foo 7 60 5\r\nhello\r\n"
	if string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

func TestTextWriterStorageCAS(t *testing.T) {
	w := NewTextWriter()
	buf := w.AppendStorage("cas", "foo", 0, 0, []byte("v"), 42, false)
	want := "cas foo 0 0 1 42\r\nv\r\n"
	if string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

func TestTextWriterStorageNoreply(t *testing.T) {
	w := NewTextWriter()
	buf := w.AppendStorage("set", "foo", 0, 0, []byte("v"), 0, true)
	want := "set foo 0 0 1 noreply\r\nv\r\n"
	if string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

func TestTextWriterRetrieval(t *testing.T) {
	w := NewTextWriter()
	buf := w.AppendRetrieval("get", "a", "b", "c")
	want := "get a b c\r\n"
	if string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

func TestTextWriterArith(t *testing.T) {
	w := NewTextWriter()
	buf := w.AppendArith("incr", "ctr", 3)
	want := "incr ctr 3\r\n"
	if string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

func TestTextWriterDelete(t *testing.T) {
	w := NewTextWriter()
	buf := w.AppendDelete("foo")
	want := "delete foo\r\n"
	if string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

func TestTextWriterTouch(t *testing.T) {
	w := NewTextWriter()
	buf := w.AppendTouch("foo", 60)
	want := "touch foo 60\r\n"
	if string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

func TestTextWriterFlushAll(t *testing.T) {
	w := NewTextWriter()
	buf := w.AppendFlushAll(-1)
	if string(buf) != "flush_all\r\n" {
		t.Fatalf("got %q", buf)
	}
	w.Reset()
	buf = w.AppendFlushAll(30)
	if string(buf) != "flush_all 30\r\n" {
		t.Fatalf("got %q", buf)
	}
}

func TestTextWriterRetrievalTouch(t *testing.T) {
	w := NewTextWriter()
	buf := w.AppendRetrievalTouch("gat", 120, "a", "b")
	if string(buf) != "gat 120 a b\r\n" {
		t.Fatalf("got %q", buf)
	}
}

func TestTextReaderOversizedValue(t *testing.T) {
	// Length header past MaxValueLen must be rejected without infinite grow.
	r := NewTextReader()
	r.Feed([]byte("VALUE k 0 999999999999\r\n"))
	_, err := r.Next()
	if err == nil || errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected protocol error, got %v", err)
	}
}

func TestTextWriterReset(t *testing.T) {
	w := NewTextWriter()
	_ = w.AppendRetrieval("get", "a")
	w.Reset()
	buf := w.AppendDelete("b")
	if !bytes.Equal(buf, []byte("delete b\r\n")) {
		t.Fatalf("Reset did not clear the buffer: %q", buf)
	}
}
