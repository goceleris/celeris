package postgres

import (
	"testing"
)

// Pool.Open / NewConnector flip AutoCacheStatements to true unless the DSN
// explicitly opts out. ParseDSN itself leaves the field at its zero value so
// internal dialConn callers (the fakePG-backed test fixtures) keep the
// simple-query default.

func TestParseDSN_AutoCacheStatements_DefaultUnsetByParser(t *testing.T) {
	d, err := ParseDSN("postgres://u@h/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if d.Options.AutoCacheStatements {
		t.Fatalf("ParseDSN should leave AutoCacheStatements unset (false); got true")
	}
	if d.Options.autoCacheStatementsExplicit {
		t.Fatalf("autoCacheStatementsExplicit should be false when not in DSN")
	}
}

func TestParseDSN_AutoCacheStatements_ExplicitTrue(t *testing.T) {
	d, err := ParseDSN("postgres://u@h/db?sslmode=disable&auto_cache_statements=true")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Options.AutoCacheStatements {
		t.Fatalf("explicit true: got false")
	}
	if !d.Options.autoCacheStatementsExplicit {
		t.Fatalf("autoCacheStatementsExplicit should be true after explicit DSN value")
	}
}

func TestParseDSN_AutoCacheStatements_ExplicitFalse(t *testing.T) {
	d, err := ParseDSN("postgres://u@h/db?sslmode=disable&auto_cache_statements=false")
	if err != nil {
		t.Fatal(err)
	}
	if d.Options.AutoCacheStatements {
		t.Fatalf("explicit false: got true")
	}
	if !d.Options.autoCacheStatementsExplicit {
		t.Fatalf("autoCacheStatementsExplicit should be true even when DSN says false")
	}
}

func TestNewConnector_DefaultsAutoCacheTrue(t *testing.T) {
	c, err := NewConnector("postgres://u@127.0.0.1:5432/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if !c.dsn.Options.AutoCacheStatements {
		t.Fatalf("NewConnector should default AutoCacheStatements=true")
	}
}

func TestNewConnector_RespectsExplicitOptOut(t *testing.T) {
	c, err := NewConnector("postgres://u@127.0.0.1:5432/db?sslmode=disable&auto_cache_statements=false")
	if err != nil {
		t.Fatal(err)
	}
	if c.dsn.Options.AutoCacheStatements {
		t.Fatalf("explicit auto_cache_statements=false must override the Connector default")
	}
}
