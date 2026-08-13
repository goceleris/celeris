package celeris_test

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func TestBindQueryDefaultsAndConversion(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/x",
		celeristest.WithQuery("page", "3"),
		celeristest.WithQuery("tag", "a"),
		celeristest.WithQuery("tag", "b"),
	)
	defer celeristest.ReleaseContext(ctx)

	var got struct {
		Page  int      `query:"page"  default:"1"`
		Limit int      `query:"limit" default:"20"`
		Tags  []string `query:"tag"`
	}
	if err := ctx.BindQuery(&got); err != nil {
		t.Fatalf("BindQuery: %v", err)
	}
	if got.Page != 3 {
		t.Errorf("Page = %d, want 3", got.Page)
	}
	if got.Limit != 20 {
		t.Errorf("Limit = %d, want 20 from the default tag", got.Limit)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b" {
		t.Errorf("Tags = %v, want [a b]", got.Tags)
	}
}

func TestBindParamsAndHeader(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/items/42",
		celeristest.WithParam("id", "42"),
		celeristest.WithHeader("X-Request-Id", "trace-abc"),
	)
	defer celeristest.ReleaseContext(ctx)

	var got struct {
		ID    int    `param:"id"`
		Trace string `header:"X-Request-Id"`
	}
	if err := ctx.BindParams(&got); err != nil {
		t.Fatalf("BindParams: %v", err)
	}
	if err := ctx.BindHeader(&got); err != nil {
		t.Fatalf("BindHeader: %v", err)
	}
	if got.ID != 42 {
		t.Errorf("ID = %d, want 42", got.ID)
	}
	if got.Trace != "trace-abc" {
		t.Errorf("Trace = %q, want trace-abc", got.Trace)
	}
}

func TestBindAllBodyWinsOverQuery(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "POST", "/x",
		celeristest.WithQuery("name", "fromquery"),
		celeristest.WithQuery("page", "7"),
		celeristest.WithContentType("application/json"),
		celeristest.WithBody([]byte(`{"name":"frombody"}`)),
	)
	defer celeristest.ReleaseContext(ctx)

	var got struct {
		Name string `query:"name" json:"name"`
		Page int    `query:"page" json:"-"`
	}
	if err := ctx.BindAll(&got); err != nil {
		t.Fatalf("BindAll: %v", err)
	}
	// The body is applied last, so it wins for a field it mentions.
	if got.Name != "frombody" {
		t.Errorf("Name = %q, want frombody (body must win)", got.Name)
	}
	// A field the body does not mention keeps its query-derived value.
	if got.Page != 7 {
		t.Errorf("Page = %d, want 7 (query value preserved)", got.Page)
	}
}

func TestBindAllWithoutBodyIsNotAnError(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/x", celeristest.WithQuery("page", "5"))
	defer celeristest.ReleaseContext(ctx)

	var got struct {
		Page int `query:"page"`
	}
	if err := ctx.BindAll(&got); err != nil {
		t.Fatalf("BindAll on a bodyless GET should succeed, got %v", err)
	}
	if got.Page != 5 {
		t.Errorf("Page = %d, want 5", got.Page)
	}
}

func TestBindConversionErrorIsTyped(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/x", celeristest.WithQuery("page", "notanumber"))
	defer celeristest.ReleaseContext(ctx)

	var v struct {
		Page int `query:"page"`
	}
	err := ctx.BindQuery(&v)
	if err == nil {
		t.Fatal("expected an error binding a non-numeric value into an int")
	}
	var be *celeris.BindError
	if !errors.As(err, &be) {
		t.Fatalf("error %v is not a *celeris.BindError", err)
	}
	if be.Field != "Page" || be.Source != "query" || be.Key != "page" || be.Value != "notanumber" {
		t.Errorf("BindError = %+v, want Field=Page Source=query Key=page Value=notanumber", be)
	}
}

func TestBindDurationAndBool(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/x",
		celeristest.WithQuery("timeout", "1500ms"),
		celeristest.WithQuery("debug", "true"),
	)
	defer celeristest.ReleaseContext(ctx)

	var got struct {
		Timeout time.Duration `query:"timeout"`
		Debug   bool          `query:"debug"`
	}
	if err := ctx.BindQuery(&got); err != nil {
		t.Fatalf("BindQuery: %v", err)
	}
	if got.Timeout != 1500*time.Millisecond {
		t.Errorf("Timeout = %v, want 1.5s (must parse as a duration, not an int64)", got.Timeout)
	}
	if !got.Debug {
		t.Error("Debug = false, want true")
	}
}

func TestBindPointerFieldStaysNilWhenAbsent(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/x")
	defer celeristest.ReleaseContext(ctx)

	var got struct {
		Cursor *string `query:"cursor"`
	}
	if err := ctx.BindQuery(&got); err != nil {
		t.Fatalf("BindQuery: %v", err)
	}
	if got.Cursor != nil {
		t.Errorf("Cursor = %q, want nil when the param is absent", *got.Cursor)
	}
}

func TestBindPointerFieldSetWhenPresent(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/x", celeristest.WithQuery("cursor", "abc"))
	defer celeristest.ReleaseContext(ctx)

	var got struct {
		Cursor *string `query:"cursor"`
	}
	if err := ctx.BindQuery(&got); err != nil {
		t.Fatalf("BindQuery: %v", err)
	}
	if got.Cursor == nil || *got.Cursor != "abc" {
		t.Errorf("Cursor = %v, want pointer to abc", got.Cursor)
	}
}

func TestBindRequiresNonNilStructPointer(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/x")
	defer celeristest.ReleaseContext(ctx)

	var s struct{}
	if err := ctx.BindQuery(s); err == nil {
		t.Error("binding into a non-pointer should error")
	}
	var p *struct{}
	if err := ctx.BindQuery(p); err == nil {
		t.Error("binding into a nil pointer should error")
	}
	n := 5
	if err := ctx.BindQuery(&n); err == nil {
		t.Error("binding into a pointer-to-non-struct should error")
	}
}

func TestUntaggedFieldsAreLeftAlone(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/x",
		celeristest.WithQuery("page", "1"),
		celeristest.WithQuery("Internal", "hacked"),
	)
	defer celeristest.ReleaseContext(ctx)

	got := struct {
		Page     int `query:"page"`
		Internal string
	}{Internal: "preset"}

	if err := ctx.BindQuery(&got); err != nil {
		t.Fatalf("BindQuery: %v", err)
	}
	if got.Internal != "preset" {
		t.Errorf("Internal = %q, want preset — an untagged field must never bind", got.Internal)
	}
}

func TestBindEmbeddedStruct(t *testing.T) {
	type Pagination struct {
		Page  int `query:"page"  default:"1"`
		Limit int `query:"limit" default:"10"`
	}
	ctx, _ := celeristest.NewContextT(t, "GET", "/x", celeristest.WithQuery("page", "4"))
	defer celeristest.ReleaseContext(ctx)

	var got struct {
		Pagination
		Q string `query:"q"`
	}
	if err := ctx.BindQuery(&got); err != nil {
		t.Fatalf("BindQuery: %v", err)
	}
	if got.Page != 4 {
		t.Errorf("embedded Page = %d, want 4", got.Page)
	}
	if got.Limit != 10 {
		t.Errorf("embedded Limit = %d, want 10 from the default tag", got.Limit)
	}
}

func TestBindForm(t *testing.T) {
	form := url.Values{"name": {"ada"}, "age": {"36"}}
	ctx, _ := celeristest.NewContextT(t, "POST", "/x",
		celeristest.WithContentType("application/x-www-form-urlencoded"),
		celeristest.WithBody([]byte(form.Encode())),
	)
	defer celeristest.ReleaseContext(ctx)

	var got struct {
		Name string `form:"name"`
		Age  int    `form:"age"`
	}
	if err := ctx.BindForm(&got); err != nil {
		t.Fatalf("BindForm: %v", err)
	}
	if got.Name != "ada" || got.Age != 36 {
		t.Errorf("got %+v, want {ada 36}", got)
	}
}

// --- validator hook -------------------------------------------------------

type stubValidator struct{ err error }

func (s stubValidator) Validate(any) error { return s.err }

func TestValidateWithoutValidatorIsAnError(t *testing.T) {
	celeris.SetValidator(nil)
	ctx, _ := celeristest.NewContextT(t, "GET", "/x")
	defer celeristest.ReleaseContext(ctx)

	err := ctx.Validate(&struct{}{})
	if !errors.Is(err, celeris.ErrNoValidator) {
		t.Fatalf("Validate() = %v, want ErrNoValidator — passing silently would hide a missing SetValidator", err)
	}
}

func TestBindAndValidateRunsTheValidator(t *testing.T) {
	sentinel := errors.New("Page is required")
	celeris.SetValidator(stubValidator{err: sentinel})
	t.Cleanup(func() { celeris.SetValidator(nil) })

	ctx, _ := celeristest.NewContextT(t, "GET", "/x", celeristest.WithQuery("page", "2"))
	defer celeristest.ReleaseContext(ctx)

	var v struct {
		Page int `query:"page"`
	}
	err := ctx.BindAndValidate(&v)
	if !errors.Is(err, sentinel) {
		t.Fatalf("BindAndValidate() = %v, want the validator's error", err)
	}
	if v.Page != 2 {
		t.Errorf("Page = %d, want 2 — binding must run before validation", v.Page)
	}
}

func TestBindAndValidateSucceedsWhenValidatorPasses(t *testing.T) {
	celeris.SetValidator(stubValidator{err: nil})
	t.Cleanup(func() { celeris.SetValidator(nil) })

	ctx, _ := celeristest.NewContextT(t, "GET", "/x", celeristest.WithQuery("page", "9"))
	defer celeristest.ReleaseContext(ctx)

	var v struct {
		Page int `query:"page"`
	}
	if err := ctx.BindAndValidate(&v); err != nil {
		t.Fatalf("BindAndValidate: %v", err)
	}
	if v.Page != 9 {
		t.Errorf("Page = %d, want 9", v.Page)
	}
}

func TestBindErrorMessageNamesFieldAndSource(t *testing.T) {
	be := &celeris.BindError{Field: "Page", Source: "query", Key: "page", Value: "x", Err: strconv.ErrSyntax}
	msg := be.Error()
	for _, want := range []string{"query", "page", "Page"} {
		if !strings.Contains(msg, want) {
			t.Errorf("BindError.Error() = %q, missing %q", msg, want)
		}
	}
	if !errors.Is(be, strconv.ErrSyntax) {
		t.Error("BindError should unwrap to the underlying conversion error")
	}
}
