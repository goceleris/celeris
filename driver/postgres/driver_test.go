package postgres

import (
	"database/sql"
	"testing"
)

func TestDriverRegistered(t *testing.T) {
	// sql.Drivers() should contain our name after init().
	found := false
	for _, name := range sql.Drivers() {
		if name == DriverName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("driver %q not registered; have %v", DriverName, sql.Drivers())
	}
}

func TestSQLOpen(t *testing.T) {
	db, err := sql.Open(DriverName, "postgres://u@localhost/dbname?sslmode=disable")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if db == nil {
		t.Fatal("sql.Open returned nil *DB")
	}
	db.Close()
}

func TestSQLOpen_BadSSL(t *testing.T) {
	db, err := sql.Open(DriverName, "postgres://u@localhost/dbname?sslmode=require")
	// ssl rejection happens at DSN parse in newConnector, which is called
	// from OpenConnector; sql.Open propagates that error.
	if err == nil {
		db.Close()
		t.Fatal("expected ErrSSLNotSupported from sql.Open")
	}
}
