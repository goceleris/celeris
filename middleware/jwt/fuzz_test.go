package jwt

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/extract"
	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
)

func FuzzTokenParse(f *testing.F) {
	f.Add("eyJhbGciOiJIUzI1NiJ9.e30.ZRrHA1JJJW8opB1Qfp7QDl2Ruf1Fm9golNmBh")
	f.Add("")
	f.Add("not.a.token")
	f.Add("a]b.c.d")
	f.Add("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U")
	f.Add("Bearer token")
	f.Add(".......")
	f.Add("{}")

	f.Fuzz(func(_ *testing.T, tokenStr string) {
		// Ensure the parser never panics on arbitrary input.
		p := jwtparse.NewParser()
		_, _ = p.Parse(tokenStr, func(_ *jwtparse.Token) (any, error) {
			return []byte("secret"), nil
		})
	})
}

func FuzzExtractorParse(f *testing.F) {
	f.Add("Bearer eyJhbGciOiJIUzI1NiJ9.e30.test")
	f.Add("Bearer ")
	f.Add("")
	f.Add("Basic dGVzdA==")
	f.Add("bearer token")
	f.Add("BeArEr token")

	f.Fuzz(func(t *testing.T, header string) {
		// Ensure header extractor never panics on arbitrary input.
		ex := extract.Parse("header:authorization:Bearer ")
		ctx, _ := celeristest.NewContextT(t, "GET", "/",
			celeristest.WithHeader("authorization", header),
		)
		_ = ex(ctx)
	})
}
