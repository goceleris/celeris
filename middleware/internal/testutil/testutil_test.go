package testutil_test

import (
	"testing"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

func TestRunMiddleware(t *testing.T) {
	mw := func(c *celeris.Context) error {
		return c.String(200, "ok")
	}
	rec, err := testutil.RunMiddleware(t, mw)
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestRunMiddlewareWithMethod(t *testing.T) {
	mw := func(c *celeris.Context) error {
		return c.String(200, "%s", c.Method())
	}
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "POST", "/test")
	testutil.AssertNoError(t, err)
	testutil.AssertBodyContains(t, rec, "POST")
}

func TestRunChain(t *testing.T) {
	var order []string
	mw1 := func(c *celeris.Context) error {
		order = append(order, "mw1-before")
		err := c.Next()
		order = append(order, "mw1-after")
		return err
	}
	mw2 := func(c *celeris.Context) error {
		order = append(order, "mw2")
		return c.String(200, "done")
	}
	rec, err := testutil.RunChain(t, []celeris.HandlerFunc{mw1, mw2}, "GET", "/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if len(order) != 3 || order[0] != "mw1-before" || order[1] != "mw2" || order[2] != "mw1-after" {
		t.Fatalf("unexpected execution order: %v", order)
	}
}

func TestAssertHTTPError(t *testing.T) {
	err := celeris.NewHTTPError(404, "not found")
	testutil.AssertHTTPError(t, err, 404)
}
