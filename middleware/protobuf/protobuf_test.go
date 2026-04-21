package protobuf

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// --- ProtoBuf response tests ---

func TestProtoBufResponse(t *testing.T) {
	msg := wrapperspb.String("hello")
	ctx, rec := celeristest.NewContextT(t, "GET", "/test")
	err := Write(ctx, 200, msg)
	if err != nil {
		t.Fatal(err)
	}
	if rec.StatusCode != 200 {
		t.Errorf("status = %d, want 200", rec.StatusCode)
	}
	if rec.Header("content-type") != ContentType {
		t.Errorf("content-type = %q, want %q", rec.Header("content-type"), ContentType)
	}
	// Verify body is valid protobuf.
	var out wrapperspb.StringValue
	if err := proto.Unmarshal(rec.Body, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.GetValue() != "hello" {
		t.Errorf("value = %q, want hello", out.GetValue())
	}
}

func TestProtoBufNilMessage(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/test")
	err := Write(ctx, 200, nil)
	if err != ErrNilMessage {
		t.Errorf("err = %v, want ErrNilMessage", err)
	}
}

func TestProtoBufTimestamp(t *testing.T) {
	msg := timestamppb.Now()
	ctx, rec := celeristest.NewContextT(t, "GET", "/test")
	if err := Write(ctx, 200, msg); err != nil {
		t.Fatal(err)
	}
	var out timestamppb.Timestamp
	if err := proto.Unmarshal(rec.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.GetSeconds() != msg.GetSeconds() {
		t.Errorf("seconds = %d, want %d", out.GetSeconds(), msg.GetSeconds())
	}
}

// --- BindProtoBuf tests ---

func TestBindProtoBuf(t *testing.T) {
	msg := wrapperspb.String("test-value")
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := celeristest.NewContextT(t, "POST", "/test",
		celeristest.WithBody(data),
		celeristest.WithContentType(ContentType))
	var out wrapperspb.StringValue
	if err := BindProtoBuf(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.GetValue() != "test-value" {
		t.Errorf("value = %q, want test-value", out.GetValue())
	}
}

func TestBindProtoBufEmptyBody(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "POST", "/test",
		celeristest.WithContentType(ContentType))
	var out wrapperspb.StringValue
	err := BindProtoBuf(ctx, &out)
	if err != celeris.ErrEmptyBody {
		t.Errorf("err = %v, want ErrEmptyBody", err)
	}
}

func TestBindProtoBufInvalidData(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "POST", "/test",
		celeristest.WithBody([]byte("not-protobuf")),
		celeristest.WithContentType(ContentType))
	var out wrapperspb.StringValue
	err := BindProtoBuf(ctx, &out)
	if err == nil {
		t.Fatal("expected error for invalid data")
	}
	if !strings.Contains(err.Error(), "invalid protobuf") {
		t.Errorf("err = %v, want to contain 'invalid protobuf'", err)
	}
}

// --- Bind (auto-detect) tests ---

func TestBindContentTypeXProtobuf(t *testing.T) {
	msg := wrapperspb.Int64(42)
	data, _ := proto.Marshal(msg)
	ctx, _ := celeristest.NewContextT(t, "POST", "/test",
		celeristest.WithBody(data),
		celeristest.WithContentType("application/x-protobuf"))
	var out wrapperspb.Int64Value
	if err := Bind(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.GetValue() != 42 {
		t.Errorf("value = %d, want 42", out.GetValue())
	}
}

func TestBindContentTypeProtobuf(t *testing.T) {
	msg := wrapperspb.Int64(99)
	data, _ := proto.Marshal(msg)
	ctx, _ := celeristest.NewContextT(t, "POST", "/test",
		celeristest.WithBody(data),
		celeristest.WithContentType("application/protobuf"))
	var out wrapperspb.Int64Value
	if err := Bind(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.GetValue() != 99 {
		t.Errorf("value = %d, want 99", out.GetValue())
	}
}

func TestBindContentTypeWithCharset(t *testing.T) {
	msg := wrapperspb.String("charset-test")
	data, _ := proto.Marshal(msg)
	ctx, _ := celeristest.NewContextT(t, "POST", "/test",
		celeristest.WithBody(data),
		celeristest.WithContentType("application/x-protobuf; charset=utf-8"))
	var out wrapperspb.StringValue
	if err := Bind(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.GetValue() != "charset-test" {
		t.Errorf("value = %q", out.GetValue())
	}
}

func TestBindContentTypeMismatch(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "POST", "/test",
		celeristest.WithBody([]byte(`{"key":"val"}`)),
		celeristest.WithContentType("application/json"))
	var out wrapperspb.StringValue
	err := Bind(ctx, &out)
	if err != ErrNotProtoBuf {
		t.Errorf("err = %v, want ErrNotProtoBuf", err)
	}
}

// --- Respond (content negotiation) tests ---

func TestRespondAcceptsProtobuf(t *testing.T) {
	msg := wrapperspb.String("negotiated")
	ctx, rec := celeristest.NewContextT(t, "GET", "/test",
		celeristest.WithHeader("accept", ContentType))
	if err := Respond(ctx, 200, msg, map[string]string{"value": "negotiated"}); err != nil {
		t.Fatal(err)
	}
	if rec.Header("content-type") != ContentType {
		t.Errorf("content-type = %q, want protobuf", rec.Header("content-type"))
	}
}

func TestRespondFallsBackToJSON(t *testing.T) {
	msg := wrapperspb.String("negotiated")
	ctx, rec := celeristest.NewContextT(t, "GET", "/test",
		celeristest.WithHeader("accept", "application/json"))
	if err := Respond(ctx, 200, msg, map[string]string{"value": "negotiated"}); err != nil {
		t.Fatal(err)
	}
	if rec.Header("content-type") != "application/json" {
		t.Errorf("content-type = %q, want json", rec.Header("content-type"))
	}
	if !strings.Contains(rec.BodyString(), "negotiated") {
		t.Errorf("body = %q, want to contain 'negotiated'", rec.BodyString())
	}
}

// --- Round-trip tests ---

func TestRoundTrip(t *testing.T) {
	original := wrapperspb.String("round-trip-test")
	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	// Write response.
	ctx1, rec := celeristest.NewContextT(t, "GET", "/test")
	if err := Write(ctx1, 200, original); err != nil {
		t.Fatal(err)
	}

	// Parse response body as request.
	ctx2, _ := celeristest.NewContextT(t, "POST", "/test",
		celeristest.WithBody(rec.Body),
		celeristest.WithContentType(ContentType))
	var parsed wrapperspb.StringValue
	if err := BindProtoBuf(ctx2, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.GetValue() != "round-trip-test" {
		t.Errorf("value = %q, want round-trip-test", parsed.GetValue())
	}

	_ = data
}

// --- Middleware + Helper tests ---

func TestMiddlewareAndHelper(t *testing.T) {
	msg := wrapperspb.String("helper-test")
	mw := New(Config{
		MarshalOptions: proto.MarshalOptions{Deterministic: true},
	})
	handler := func(c *celeris.Context) error {
		pb := FromContext(c)
		return pb.Write(200, msg)
	}
	ctx, rec := celeristest.NewContextT(t, "GET", "/test",
		celeristest.WithHandlers(mw, handler))
	if err := ctx.Next(); err != nil {
		t.Fatal(err)
	}
	if rec.Header("content-type") != ContentType {
		t.Errorf("content-type = %q", rec.Header("content-type"))
	}
	var out wrapperspb.StringValue
	if err := proto.Unmarshal(rec.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.GetValue() != "helper-test" {
		t.Errorf("value = %q", out.GetValue())
	}
}

func TestHelperBind(t *testing.T) {
	msg := wrapperspb.String("bind-helper")
	data, _ := proto.Marshal(msg)
	mw := New(Config{
		UnmarshalOptions: proto.UnmarshalOptions{DiscardUnknown: true},
	})
	var result string
	handler := func(c *celeris.Context) error {
		pb := FromContext(c)
		var out wrapperspb.StringValue
		if err := pb.Bind(&out); err != nil {
			return err
		}
		result = out.GetValue()
		return c.NoContent(200)
	}
	ctx, _ := celeristest.NewContextT(t, "POST", "/test",
		celeristest.WithBody(data),
		celeristest.WithContentType(ContentType),
		celeristest.WithHandlers(mw, handler))
	if err := ctx.Next(); err != nil {
		t.Fatal(err)
	}
	if result != "bind-helper" {
		t.Errorf("result = %q, want bind-helper", result)
	}
}

func TestFromContextWithoutMiddleware(t *testing.T) {
	msg := wrapperspb.String("no-mw")
	ctx, rec := celeristest.NewContextT(t, "GET", "/test")
	pb := FromContext(ctx)
	if err := pb.Write(200, msg); err != nil {
		t.Fatal(err)
	}
	var out wrapperspb.StringValue
	if err := proto.Unmarshal(rec.Body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.GetValue() != "no-mw" {
		t.Errorf("value = %q", out.GetValue())
	}
}

// TestWriteDoesNotRetainBuffer pins the invariant that c.Blob copies the
// payload internally, so the pooled scratch buffer returned to the pool
// after Write is safe to mutate without corrupting in-flight responses.
// If c.Blob ever switches to a zero-copy strategy, this test will catch it.
func TestWriteDoesNotRetainBuffer(t *testing.T) {
	msg := wrapperspb.String("retain-test")
	ctx, rec := celeristest.NewContextT(t, "GET", "/test")
	if err := Write(ctx, 200, msg); err != nil {
		t.Fatal(err)
	}

	// Snapshot the recorded body before any pool reuse.
	want := append([]byte(nil), rec.Body...)

	// Force pool reuse by performing several more marshal cycles. If
	// c.Blob retained a reference to the pooled buffer, the next pool
	// borrower would observe (and overwrite) the response bytes.
	for i := 0; i < 100; i++ {
		filler := wrapperspb.String(strings.Repeat("X", 64))
		c2, _ := celeristest.NewContext("GET", "/x")
		_ = Write(c2, 200, filler)
		celeristest.ReleaseContext(c2)
	}

	if string(rec.Body) != string(want) {
		t.Fatalf("response body mutated by pool reuse:\n  got=%v\n  want=%v", rec.Body, want)
	}
}

// --- Content type detection ---

func TestIsProtoBufContentType(t *testing.T) {
	tests := []struct {
		ct   string
		want bool
	}{
		{"application/x-protobuf", true},
		{"application/protobuf", true},
		{"APPLICATION/X-PROTOBUF", true},
		{"application/x-protobuf; charset=utf-8", true},
		{"application/json", false},
		{"text/plain", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isProtoBufContentType(tt.ct); got != tt.want {
			t.Errorf("isProtoBufContentType(%q) = %v, want %v", tt.ct, got, tt.want)
		}
	}
}

// TestPoolEvictionCounter verifies that marshaling a message larger than
// maxPooledBufSize increments the PoolEvictions counter.
func TestPoolEvictionCounter(t *testing.T) {
	before := PoolEvictions.Load()
	// 40KB payload — exceeds the 32KB pool threshold.
	msg := wrapperspb.Bytes(make([]byte, 40*1024))
	ctx, _ := celeristest.NewContextT(t, "GET", "/test")
	if err := Write(ctx, 200, msg); err != nil {
		t.Fatal(err)
	}
	after := PoolEvictions.Load()
	if after <= before {
		t.Errorf("PoolEvictions did not increment: before=%d after=%d", before, after)
	}
}

// --- Benchmarks ---

func BenchmarkMarshalSmall(b *testing.B) {
	msg := wrapperspb.String("hello world benchmark")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench")
		_ = Write(ctx, 200, msg)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkMarshalLarge(b *testing.B) {
	msg := wrapperspb.Bytes(make([]byte, 10*1024)) // 10KB
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench")
		_ = Write(ctx, 200, msg)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkUnmarshalSmall(b *testing.B) {
	msg := wrapperspb.String("hello world benchmark")
	data, _ := proto.Marshal(msg)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/bench",
			celeristest.WithBody(data),
			celeristest.WithContentType(ContentType))
		var out wrapperspb.StringValue
		_ = BindProtoBuf(ctx, &out)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkUnmarshalLarge(b *testing.B) {
	msg := wrapperspb.Bytes(make([]byte, 10*1024))
	data, _ := proto.Marshal(msg)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/bench",
			celeristest.WithBody(data),
			celeristest.WithContentType(ContentType))
		var out wrapperspb.BytesValue
		_ = BindProtoBuf(ctx, &out)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkProtoBufResponse(b *testing.B) {
	msg := wrapperspb.String("response benchmark")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/bench")
		_ = Write(ctx, 200, msg)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkVsJSON(b *testing.B) {
	type JSONMsg struct {
		Value string `json:"value"`
	}
	pbMsg := wrapperspb.String("comparison benchmark value")
	jsonMsg := JSONMsg{Value: "comparison benchmark value"}

	b.Run("protobuf", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ctx, _ := celeristest.NewContext("GET", "/bench")
			_ = Write(ctx, 200, pbMsg)
			celeristest.ReleaseContext(ctx)
		}
	})
	b.Run("json", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ctx, _ := celeristest.NewContext("GET", "/bench")
			_ = ctx.JSON(200, jsonMsg)
			celeristest.ReleaseContext(ctx)
		}
	})
}

// BenchmarkUnmarshalInvalid exercises the invalid-proto error path.
// Hit on malformed client requests, content-type mismatches, or
// mis-configured clients sending JSON over application/x-protobuf.
func BenchmarkUnmarshalInvalid(b *testing.B) {
	// Garbage bytes that won't parse as any wrapperspb type.
	data := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("POST", "/bench",
			celeristest.WithBody(data),
			celeristest.WithContentType(ContentType))
		var out wrapperspb.StringValue
		_ = BindProtoBuf(ctx, &out)
		celeristest.ReleaseContext(ctx)
	}
}
