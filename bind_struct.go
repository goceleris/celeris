package celeris

import (
	"encoding"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Struct binding: fill a typed struct from the request's path params, query
// string, headers, form values and body in one call.
//
// Before this, everything except the body was read field-by-field
// (c.Query("page"), c.Param("id"), …) — the most visible gap for anyone
// arriving from gin/echo/fiber.
//
// Source tags, one per field:
//
//	type ListReq struct {
//	    ID     int      `param:"id"`
//	    Page   int      `query:"page"   default:"1"`
//	    Limit  int      `query:"limit"  default:"20"`
//	    Tags   []string `query:"tag"`
//	    Trace  string   `header:"X-Request-Id"`
//	    Name   string   `form:"name"`
//	}
//
// A field with no tag for the source being bound is skipped, so one struct can
// carry several sources and [Context.BindAll] fills them all.
//
// Body binding is unchanged: [Context.Bind] stays body-only (JSON/XML by
// Content-Type) so existing code keeps working. [Context.BindAll] is the
// one-call form that also reads the body.

// Validator validates a value that has just been bound, typically a struct
// with `validate:"..."` tags.
//
// celeris deliberately ships no validator implementation: the root module has
// no third-party dependencies and that is worth keeping. Plug one in — for
// example a thin wrapper over go-playground/validator — with [SetValidator]:
//
//	type pgv struct{ v *validator.Validate }
//	func (p pgv) Validate(x any) error { return p.v.Struct(x) }
//
//	celeris.SetValidator(pgv{v: validator.New()})
//
// Implementations must be safe for concurrent use: one Validator serves every
// request.
type Validator interface {
	Validate(any) error
}

// validatorHolder boxes the interface so it can live in an atomic.Pointer
// (an interface value is two words and cannot be stored atomically directly).
type validatorHolder struct{ v Validator }

var activeValidator atomic.Pointer[validatorHolder]

// SetValidator installs the process-wide [Validator] used by
// [Context.Validate]. Passing nil clears it.
//
// Process-wide rather than per-Server because Context carries no server
// backref, and adding one would grow every pooled Context on the hot path for
// a feature used by a minority of handlers. This mirrors gin's package-level
// binding.Validator. Call it once during start-up, before serving.
func SetValidator(v Validator) {
	if v == nil {
		activeValidator.Store(nil)
		return
	}
	activeValidator.Store(&validatorHolder{v: v})
}

// ErrNoValidator is returned by [Context.Validate] when no [Validator] has
// been installed with [SetValidator].
var ErrNoValidator = errors.New("celeris: no Validator installed (see celeris.SetValidator)")

// Validate runs the installed [Validator] against v.
//
// Returns [ErrNoValidator] if none is installed — silently succeeding would
// let a missing SetValidator call ship as "validation passes".
func (c *Context) Validate(v any) error {
	h := activeValidator.Load()
	if h == nil || h.v == nil {
		return ErrNoValidator
	}
	return h.v.Validate(v)
}

// BindError reports a field that could not be bound. It carries the source,
// the wire name and the offending value so a handler can return a useful 400
// instead of a bare "invalid request".
type BindError struct {
	// Field is the Go struct field name.
	Field string
	// Source is "param", "query", "header" or "form".
	Source string
	// Key is the wire name from the tag.
	Key string
	// Value is the raw string that failed to convert.
	Value string
	// Err is the underlying conversion error.
	Err error
}

func (e *BindError) Error() string {
	return fmt.Sprintf("celeris: binding %s %q into field %s: %v", e.Source, e.Key, e.Field, e.Err)
}

func (e *BindError) Unwrap() error { return e.Err }

// BindQuery fills v from the query string using `query:"name"` tags.
func (c *Context) BindQuery(v any) error {
	return c.bindSource(v, "query", func(key string) ([]string, bool) {
		vals := c.QueryValues(key)
		return vals, len(vals) > 0
	})
}

// BindParams fills v from the route's path parameters using `param:"name"`
// tags.
func (c *Context) BindParams(v any) error {
	return c.bindSource(v, "param", func(key string) ([]string, bool) {
		s := c.Param(key)
		if s == "" {
			return nil, false
		}
		return []string{s}, true
	})
}

// BindHeader fills v from the request headers using `header:"Name"` tags.
// Header lookup is case-insensitive, as elsewhere in celeris.
func (c *Context) BindHeader(v any) error {
	return c.bindSource(v, "header", func(key string) ([]string, bool) {
		s := c.Header(key)
		if s == "" {
			return nil, false
		}
		return []string{s}, true
	})
}

// BindForm fills v from the request's form values using `form:"name"` tags.
// Parses the body as a form if it has not been parsed already, so it is only
// meaningful for form content types.
func (c *Context) BindForm(v any) error {
	return c.bindSource(v, "form", func(key string) ([]string, bool) {
		vals := c.FormValues(key)
		return vals, len(vals) > 0
	})
}

// BindAll fills v from every source: path params, query string, headers, form
// values, and finally the body when the request has one.
//
// Order matters — later sources overwrite earlier ones for a field carrying
// several tags. The body runs last so an explicit JSON payload wins.
//
// A body that is absent or empty is not an error here (unlike [Context.Bind]),
// because BindAll is routinely used on GETs that carry only query params.
func (c *Context) BindAll(v any) error {
	if err := c.BindParams(v); err != nil {
		return err
	}
	if err := c.BindQuery(v); err != nil {
		return err
	}
	if err := c.BindHeader(v); err != nil {
		return err
	}

	ct := c.Header("content-type")
	switch {
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"),
		strings.HasPrefix(ct, "multipart/form-data"):
		if err := c.BindForm(v); err != nil {
			return err
		}
	case len(c.Body()) > 0:
		// Body-shaped payload (JSON/XML): reuse the existing content-type
		// dispatch rather than duplicating it.
		if err := c.Bind(v); err != nil && !errors.Is(err, ErrEmptyBody) {
			return err
		}
	}
	return nil
}

// BindAndValidate is the common path: fill v from every source, then run the
// installed [Validator].
func (c *Context) BindAndValidate(v any) error {
	if err := c.BindAll(v); err != nil {
		return err
	}
	return c.Validate(v)
}

// lookupFunc reports the values for a wire key and whether it was present.
type lookupFunc func(key string) ([]string, bool)

// bindSource walks v's fields and assigns from lookup for every field tagged
// with tagName. v must be a non-nil pointer to a struct.
func (c *Context) bindSource(v any, tagName string, lookup lookupFunc) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("celeris: Bind%s requires a non-nil pointer to a struct, got %T",
			strings.ToUpper(tagName[:1])+tagName[1:], v)
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("celeris: Bind%s requires a pointer to a struct, got pointer to %s",
			strings.ToUpper(tagName[:1])+tagName[1:], rv.Kind())
	}
	return bindStruct(rv, tagName, lookup)
}

func bindStruct(rv reflect.Value, tagName string, lookup lookupFunc) error {
	rt := rv.Type()
	for i := range rt.NumField() {
		field := rt.Field(i)
		fv := rv.Field(i)

		// Embedded structs: recurse so a shared Pagination struct can be
		// embedded and still bind.
		if field.Anonymous && fv.Kind() == reflect.Struct && !field.IsExported() {
			continue
		}
		if field.Anonymous && fv.Kind() == reflect.Struct {
			if err := bindStruct(fv, tagName, lookup); err != nil {
				return err
			}
			continue
		}
		if !fv.CanSet() {
			continue
		}

		key, ok := field.Tag.Lookup(tagName)
		if !ok || key == "-" || key == "" {
			continue
		}

		vals, present := lookup(key)
		if !present {
			def, hasDef := field.Tag.Lookup("default")
			if !hasDef {
				continue
			}
			vals = []string{def}
		}

		if err := setFieldValue(fv, vals); err != nil {
			return &BindError{
				Field:  field.Name,
				Source: tagName,
				Key:    key,
				Value:  strings.Join(vals, ","),
				Err:    err,
			}
		}
	}
	return nil
}

// setFieldValue assigns vals to fv, converting to the field's type.
func setFieldValue(fv reflect.Value, vals []string) error {
	// Slices take every value ("?tag=a&tag=b").
	if fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() != reflect.Uint8 {
		out := reflect.MakeSlice(fv.Type(), len(vals), len(vals))
		for i, s := range vals {
			if err := setScalar(out.Index(i), s); err != nil {
				return err
			}
		}
		fv.Set(out)
		return nil
	}
	if len(vals) == 0 {
		return nil
	}
	return setScalar(fv, vals[0])
}

func setScalar(fv reflect.Value, s string) error {
	// Pointer fields: allocate then fill, so an absent value stays nil and a
	// present-but-empty one is distinguishable.
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		return setScalar(fv.Elem(), s)
	}

	// encoding.TextUnmarshaler wins over the built-in conversions so custom
	// types (uuid.UUID, netip.Addr, …) bind without special-casing here.
	if fv.CanAddr() {
		if tu, ok := fv.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return tu.UnmarshalText([]byte(s))
		}
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
		return nil

	case reflect.Bool:
		// Treat a bare flag ("?debug") as true, matching how query flags are
		// conventionally written.
		if s == "" {
			fv.SetBool(true)
			return nil
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		fv.SetBool(b)
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// time.Duration is an int64 but must parse as "1s", not 1000000000.
		if fv.Type() == reflect.TypeOf(time.Duration(0)) {
			d, err := time.ParseDuration(s)
			if err != nil {
				return err
			}
			fv.SetInt(int64(d))
			return nil
		}
		n, err := strconv.ParseInt(s, 10, fv.Type().Bits())
		if err != nil {
			return err
		}
		fv.SetInt(n)
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, fv.Type().Bits())
		if err != nil {
			return err
		}
		fv.SetUint(n)
		return nil

	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, fv.Type().Bits())
		if err != nil {
			return err
		}
		fv.SetFloat(f)
		return nil

	default:
		return fmt.Errorf("unsupported field type %s", fv.Type())
	}
}
