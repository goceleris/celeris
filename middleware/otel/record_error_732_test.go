package otel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/goceleris/celeris"
)

type namedError732 string

func (e namedError732) Error() string { return string(e) }

// TestRecordErrorMatchesSDK pins that recordError, which copies the
// handler's error message (celeris#732), records what the middleware did
// before it with span.RecordError and span.SetStatus: the same exception
// event (name, exception.type, exception.message) and the same status, a
// message longer than the status limit included.
func TestRecordErrorMatchesSDK(t *testing.T) {
	for _, err := range []error{
		errors.New("plain"),
		celeris.NewHTTPError(400, "bad id"),
		namedError732("named"),
		fmt.Errorf("wrapped: %w", errors.New("inner")),
		errors.New(strings.Repeat("é", maxErrorLen)),
	} {
		exp := tracetest.NewInMemoryExporter()
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)).Tracer("t")
		_, before := tracer.Start(context.Background(), "before")
		before.RecordError(err)
		before.SetStatus(codes.Error, truncateString(err.Error(), maxErrorLen))
		before.End()
		_, after := tracer.Start(context.Background(), "after")
		recordError(after, err)
		after.End()

		spans := exp.GetSpans()
		if len(spans) != 2 {
			t.Fatalf("%T: %d spans", err, len(spans))
		}
		render := func(s tracetest.SpanStub) string {
			var b strings.Builder
			for _, ev := range s.Events {
				b.WriteString(ev.Name)
				for _, kv := range ev.Attributes {
					b.WriteString(" " + string(kv.Key) + "=" + kv.Value.AsString())
				}
				b.WriteString("; ")
			}
			fmt.Fprintf(&b, "status %v %q", s.Status.Code, s.Status.Description)
			return b.String()
		}
		if got, want := render(spans[1]), render(spans[0]); got != want {
			t.Errorf("%T: recordError records\n  %s\nwhere RecordError and SetStatus record\n  %s", err, got, want)
		}
	}
}

// TestRecordErrorNotRecordingCopiesNothing pins that the copy is made only
// for a span that records: an unsampled span costs nothing.
func TestRecordErrorNotRecordingCopiesNothing(t *testing.T) {
	span := trace.SpanFromContext(context.Background())
	if span.IsRecording() {
		t.Fatal("the background context's span records")
	}
	err := errors.New("not sampled")
	if n := testing.AllocsPerRun(100, func() { recordError(span, err) }); n != 0 {
		t.Errorf("recordError on a span that does not record: %v allocations, want 0", n)
	}
}
