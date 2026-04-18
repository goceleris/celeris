package postgres

import (
	"errors"
	"testing"
	"time"
)

func TestParseDSN_URL(t *testing.T) {
	d, err := ParseDSN("postgres://alice:secret@db.example.com:6543/shop?sslmode=disable&application_name=orders&search_path=public")
	if err != nil {
		t.Fatal(err)
	}
	if d.User != "alice" || d.Password != "secret" {
		t.Errorf("user/password: got %q/%q", d.User, d.Password)
	}
	if d.Host != "db.example.com" || d.Port != "6543" {
		t.Errorf("host/port: got %q/%q", d.Host, d.Port)
	}
	if d.Database != "shop" {
		t.Errorf("db: got %q", d.Database)
	}
	if d.Options.Application != "orders" {
		t.Errorf("app: got %q", d.Options.Application)
	}
	if d.Params["search_path"] != "public" {
		t.Errorf("search_path: got %q", d.Params["search_path"])
	}
	if d.Params["application_name"] != "orders" {
		t.Errorf("application_name startup param missing: %v", d.Params)
	}
}

func TestParseDSN_URLDefaults(t *testing.T) {
	d, err := ParseDSN("postgres://u@/")
	if err != nil {
		t.Fatal(err)
	}
	if d.Host != defaultHost || d.Port != defaultPort {
		t.Errorf("defaults: got %q:%q", d.Host, d.Port)
	}
	if d.Options.SSLMode != "disable" {
		t.Errorf("sslmode default: got %q", d.Options.SSLMode)
	}
}

func TestParseDSN_KV(t *testing.T) {
	d, err := ParseDSN("host=127.0.0.1 port=5432 user=bob password='p a s s' dbname=catalog connect_timeout=3 statement_cache_size=128")
	if err != nil {
		t.Fatal(err)
	}
	if d.Host != "127.0.0.1" || d.Port != "5432" {
		t.Errorf("host/port: %q %q", d.Host, d.Port)
	}
	if d.User != "bob" || d.Password != "p a s s" {
		t.Errorf("user/password: %q / %q", d.User, d.Password)
	}
	if d.Database != "catalog" {
		t.Errorf("db: %q", d.Database)
	}
	if d.Options.ConnectTimeout != 3*time.Second {
		t.Errorf("timeout: %v", d.Options.ConnectTimeout)
	}
	if d.Options.StatementCacheSize != 128 {
		t.Errorf("cache: %d", d.Options.StatementCacheSize)
	}
}

func TestParseDSN_Empty(t *testing.T) {
	if _, err := ParseDSN("   "); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseDSN_SSLReject(t *testing.T) {
	for _, m := range []string{"require", "verify-ca", "verify-full"} {
		d, err := ParseDSN("postgres://u@h/?sslmode=" + m)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.CheckSSL(); !errors.Is(err, ErrSSLNotSupported) {
			t.Errorf("sslmode=%s: got %v, want ErrSSLNotSupported", m, err)
		}
	}
}

func TestParseDSN_SSLAccept(t *testing.T) {
	for _, m := range []string{"disable", "prefer", "allow", ""} {
		d := DSN{Options: Options{SSLMode: m}}
		if err := d.CheckSSL(); err != nil {
			t.Errorf("sslmode=%q: %v", m, err)
		}
	}
}

func TestParseDSN_KVQuoted(t *testing.T) {
	d, err := ParseDSN(`host=localhost password="quo\"te"`)
	if err != nil {
		t.Fatal(err)
	}
	if d.Password != `quo"te` {
		t.Errorf("quoted: %q", d.Password)
	}
}
