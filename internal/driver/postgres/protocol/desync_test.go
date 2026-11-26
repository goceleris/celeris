package protocol

import "testing"

// A state machine answers a message it does not expect for its request with
// (done=false, err): the driver reads that as "the reply stream no longer matches
// the request queue" and fails the conn. The server's own error comes with
// done=true, at ReadyForQuery. These tests pin the two apart.

func TestSimpleQueryReadyForQueryNeedsAResult(t *testing.T) {
	q := &SimpleQueryState{}
	done, err := q.Handle(BackendReadyForQuery, []byte{'I'}, nil)
	if done || err == nil {
		t.Fatalf("a ReadyForQuery before any result: done=%v err=%v, want (false, error)", done, err)
	}
	for _, first := range []byte{BackendCommandComplete, BackendEmptyQuery} {
		q = &SimpleQueryState{}
		payload := []byte("SELECT 1\x00")
		if first == BackendEmptyQuery {
			payload = nil
		}
		if _, err := q.Handle(first, payload, nil); err != nil {
			t.Fatal(err)
		}
		if done, err := q.Handle(BackendReadyForQuery, []byte{'I'}, nil); !done || err != nil {
			t.Fatalf("first %q: ReadyForQuery done=%v err=%v", first, done, err)
		}
	}
	q = &SimpleQueryState{}
	_, _ = q.Handle(BackendErrorResponse, []byte("SERROR\x00C42601\x00Mboom\x00\x00"), nil)
	done, err = q.Handle(BackendReadyForQuery, []byte{'I'}, nil)
	if !done || err == nil {
		t.Fatalf("a server error: done=%v err=%v, want (true, *PGError)", done, err)
	}
}

func TestExtendedQueryReadyForQueryNeedsACompleteReply(t *testing.T) {
	e := &ExtendedQueryState{}
	_, _ = e.Handle(BackendParseComplete, nil, nil)
	_, _ = e.Handle(BackendBindComplete, nil, nil)
	if done, err := e.Handle(BackendReadyForQuery, []byte{'I'}, nil); done || err == nil {
		t.Fatalf("ReadyForQuery before CommandComplete: done=%v err=%v, want (false, error)", done, err)
	}
	// An ErrorResponse completes the reply at any phase.
	e = &ExtendedQueryState{}
	_, _ = e.Handle(BackendParseComplete, nil, nil)
	_, _ = e.Handle(BackendErrorResponse, []byte("SERROR\x00C42601\x00Mboom\x00\x00"), nil)
	if done, err := e.Handle(BackendReadyForQuery, []byte{'I'}, nil); !done || err == nil {
		t.Fatalf("after ErrorResponse: done=%v err=%v, want (true, *PGError)", done, err)
	}
	// After an ErrorResponse the server skips to Sync and sends only the
	// ReadyForQuery: anything else is out of step, in every state.
	errResp := []byte("SERROR\x00C42601\x00Mboom\x00\x00")
	e = &ExtendedQueryState{}
	_, _ = e.Handle(BackendErrorResponse, errResp, nil)
	if done, err := e.Handle(BackendCopyData, nil, nil); done || err == nil {
		t.Fatalf("extended: a message after ErrorResponse: done=%v err=%v, want (false, error)", done, err)
	}
	// A FATAL after an ERROR keeps the first error, in every state.
	fatal := []byte("SFATAL\x00C57P01\x00Mshutting down\x00\x00")
	e = &ExtendedQueryState{}
	_, _ = e.Handle(BackendErrorResponse, errResp, nil)
	if _, err := e.Handle(BackendErrorResponse, fatal, nil); err != nil {
		t.Fatalf("extended: a second ErrorResponse: %v", err)
	}
	if _, err := e.Handle(BackendReadyForQuery, []byte{'I'}, nil); err == nil || e.Err.Code != "42601" {
		t.Fatalf("extended: want the first error 42601, got %+v / %v", e.Err, err)
	}
	e = &ExtendedQueryState{}
	_, _ = e.Handle(BackendErrorResponse, errResp, nil)
	_, _ = e.Handle(BackendNoticeResponse, nil, nil) // out-of-band messages stay harmless
	if done, err := e.Handle(BackendReadyForQuery, []byte{'I'}, nil); !done || err == nil {
		t.Fatalf("extended: ReadyForQuery after a notice: done=%v err=%v, want (true, *PGError)", done, err)
	}
	q := &SimpleQueryState{}
	_, _ = q.Handle(BackendErrorResponse, errResp, nil)
	if done, err := q.Handle(BackendCommandComplete, []byte("SELECT 1\x00"), nil); done || err == nil {
		t.Fatalf("simple: a message after ErrorResponse: done=%v err=%v, want (false, error)", done, err)
	}
	in := &CopyInState{}
	_, _ = in.Handle(BackendErrorResponse, errResp)
	if done, err := in.Handle(BackendCopyData, nil); done || err == nil {
		t.Fatalf("COPY IN: a message after ErrorResponse: done=%v err=%v", done, err)
	}
	out := &CopyOutState{}
	_, _ = out.Handle(BackendErrorResponse, errResp, nil)
	if done, err := out.Handle(BackendCopyData, nil, nil); done || err == nil {
		t.Fatalf("COPY OUT: a message after ErrorResponse: done=%v err=%v", done, err)
	}
}

func TestExtendedQueryRejectsInlineCloseAndEarlyPortalSuspended(t *testing.T) {
	e := &ExtendedQueryState{SkipParse: true}
	if _, err := e.Handle(BackendCloseComplete, nil, nil); err == nil {
		t.Fatal("CloseComplete: the driver sends no Close inline with an extended query")
	}
	e = &ExtendedQueryState{SkipParse: true}
	if _, err := e.Handle(BackendPortalSuspended, nil, nil); err == nil {
		t.Fatal("PortalSuspended before BindComplete")
	}
}

func TestCopyStatesNeedTheirOwnReply(t *testing.T) {
	in := &CopyInState{}
	if done, err := in.Handle(BackendReadyForQuery, []byte{'I'}); done || err == nil {
		t.Fatalf("COPY IN: ReadyForQuery before CopyInResponse: done=%v err=%v", done, err)
	}
	in = &CopyInState{}
	if _, err := in.Handle(BackendCommandComplete, []byte("COPY 1\x00")); err == nil {
		t.Fatal("COPY IN: CommandComplete before CopyInResponse")
	}
	out := &CopyOutState{}
	if done, err := out.Handle(BackendReadyForQuery, []byte{'I'}, nil); done || err == nil {
		t.Fatalf("COPY OUT: ReadyForQuery before CopyOutResponse: done=%v err=%v", done, err)
	}
	out = &CopyOutState{}
	if _, err := out.Handle(BackendCommandComplete, []byte("COPY 1\x00"), nil); err == nil {
		t.Fatal("COPY OUT: CommandComplete before CopyDone")
	}
	// A whole COPY OUT reply is still accepted.
	out = &CopyOutState{}
	for _, m := range []struct {
		typ     byte
		payload []byte
	}{
		{BackendCopyOutResponse, []byte{0, 0, 1, 0, 0}},
		{BackendCopyData, []byte("x\n")},
		{BackendCopyDone, nil},
		{BackendCommandComplete, []byte("COPY 1\x00")},
	} {
		if _, err := out.Handle(m.typ, m.payload, nil); err != nil {
			t.Fatalf("%q: %v", m.typ, err)
		}
	}
	if done, err := out.Handle(BackendReadyForQuery, []byte{'I'}, nil); !done || err != nil {
		t.Fatalf("COPY OUT ReadyForQuery: done=%v err=%v", done, err)
	}
}

func TestStartupReadyForQueryNeedsAuthenticationOK(t *testing.T) {
	s := &StartupState{User: "u"}
	w := NewWriter()
	_ = s.Start(w)
	if _, done, err := s.Handle(BackendReadyForQuery, []byte{'I'}, w); done || err == nil {
		t.Fatalf("ReadyForQuery before AuthenticationOk: done=%v err=%v", done, err)
	}
}
