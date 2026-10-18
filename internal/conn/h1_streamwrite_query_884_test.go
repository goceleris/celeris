package conn

import (
	"bytes"
	"errors"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// fakeQuery884 is an engine's write-error query with a limit on what is
// staged: it refuses what would take the staged bytes past limit, like the
// engines' pre-Detach query (celeris#884 item 45, #885 item 30), and reports a
// closed conn once closed is set.
type fakeQuery884 struct {
	staged *int
	limit  int
	closed bool
	asked  []int
}

func (q *fakeQuery884) StreamWriteErr(n int) error {
	q.asked = append(q.asked, n)
	if q.closed {
		return ErrConnClosed
	}
	if *q.staged+n > q.limit {
		return ErrWriteRefused
	}
	return nil
}

// The adapter asks the query with the size of the chunk before it stages it,
// stages nothing a refusal covers, and keeps the refusal for the rest of the
// response; WriteHeader starts the next one.
func TestH1StreamWriteQuery884(t *testing.T) {
	var out bytes.Buffer
	staged := 0
	q := &fakeQuery884{staged: &staged, limit: 100}
	a := &h1ResponseAdapter{write: func(p []byte) { out.Write(p); staged += len(p) }, keepAlive: true, writeErr: q}
	var s *stream.Stream

	// Chunked framing of 40 bytes: "28\r\n", the data, "\r\n" = 46.
	if err := a.Write(s, make([]byte, 40)); err != nil {
		t.Fatalf("a chunk under the limit: %v", err)
	}
	if got := q.asked; len(got) != 2 || got[0] != 46 || got[1] != 0 {
		t.Fatalf("the query was asked %v, want [46 0]: the framed size before staging, then the state after the hook", got)
	}
	if staged != 46 {
		t.Fatalf("staged %d bytes, want 46", staged)
	}

	// 46 + 60 + framing > 100: refused, and nothing of it is staged.
	err := a.Write(s, make([]byte, 60))
	if !errors.Is(err, ErrWriteRefused) {
		t.Fatalf("a chunk over the limit: %v, want ErrWriteRefused", err)
	}
	if staged != 46 {
		t.Fatalf("a refused chunk was staged: %d bytes, want 46", staged)
	}

	// The refusal is kept: a smaller chunk that would fit is not staged
	// behind the gap, and Flush and Close report it.
	asked := len(q.asked)
	if err := a.Write(s, []byte("x")); !errors.Is(err, ErrWriteRefused) {
		t.Fatalf("a Write after a refusal: %v", err)
	}
	if err := a.Flush(s); !errors.Is(err, ErrWriteRefused) {
		t.Fatalf("Flush after a refusal: %v", err)
	}
	if err := a.Close(s); !errors.Is(err, ErrWriteRefused) {
		t.Fatalf("Close after a refusal: %v", err)
	}
	if staged != 46 {
		t.Fatalf("writes after a refusal were staged: %d bytes, want 46", staged)
	}
	if len(q.asked) != asked {
		t.Fatalf("the engine was asked %d more times after the refusal, want none: the adapter keeps the answer", len(q.asked)-asked)
	}

	// The next response starts clean.
	staged, out = 0, bytes.Buffer{}
	if err := a.WriteHeader(s, 200, nil); err != nil {
		t.Fatalf("WriteHeader of the next response: %v", err)
	}
	if err := a.Write(s, []byte("ok")); err != nil {
		t.Fatalf("a Write of the next response: %v", err)
	}

	// A conn that has been torn down: every call reports it, and Write
	// stages nothing.
	q.closed = true
	before := staged
	if err := a.Flush(s); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("Flush on a closed conn: %v", err)
	}
	if err := a.Write(s, []byte("late")); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("Write on a closed conn: %v", err)
	}
	if staged != before {
		t.Fatalf("a Write on a closed conn was staged")
	}
}

// The last chunk is asked for like any other write, so the edge is exact: a
// response that fits with the last chunk is closed, one that does not stages
// nothing of it and Close reports the refusal (a Close never stages bytes and
// reports them refused), and a head that does not fit is not staged.
func TestH1StreamCloseEdge884(t *testing.T) {
	for _, tc := range []struct {
		name   string
		staged int // bytes staged before Close, of a limit of 100
		ok     bool
	}{{"last chunk fits exactly", 95, true}, {"one byte over", 96, false}, {"far over", 100, false}} {
		t.Run(tc.name, func(t *testing.T) {
			staged := tc.staged
			q := &fakeQuery884{staged: &staged, limit: 100}
			a := &h1ResponseAdapter{write: func(p []byte) { staged += len(p) }, keepAlive: true, writeErr: q}
			err := a.Close(nil)
			if tc.ok {
				if err != nil || staged != 100 {
					t.Fatalf("Close = %v with %d staged, want nil and 100", err, staged)
				}
				if got := q.asked; len(got) != 2 || got[0] != 5 || got[1] != 0 {
					t.Fatalf("the query was asked %v, want [5 0]: the last chunk's size before staging, then the state", got)
				}
				return
			}
			if !errors.Is(err, ErrWriteRefused) {
				t.Fatalf("Close = %v, want ErrWriteRefused", err)
			}
			if staged != tc.staged {
				t.Fatalf("a refused Close staged %d bytes", staged-tc.staged)
			}
		})
	}
	t.Run("head", func(t *testing.T) {
		staged := 0
		q := &fakeQuery884{staged: &staged, limit: 10}
		a := &h1ResponseAdapter{write: func(p []byte) { staged += len(p) }, keepAlive: true, writeErr: q}
		if err := a.WriteHeader(nil, 200, nil); !errors.Is(err, ErrWriteRefused) || staged != 0 {
			t.Fatalf("a head over the limit: %v with %d staged, want ErrWriteRefused and 0", err, staged)
		}
	})
}
