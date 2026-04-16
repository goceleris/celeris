package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// ExampleOpen shows the database/sql entry point: import the driver for its
// side-effect (the init registers "celeris-postgres") and sql.Open a DSN.
// database/sql owns the pool and the driver runs against a package-level
// standalone event loop.
func ExampleOpen() {
	// The blank import at package scope is what registers the driver.
	db, err := sql.Open("celeris-postgres",
		"postgres://app:secret@localhost:5432/mydb?sslmode=disable&application_name=svc")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	var greeting string
	if err := db.QueryRowContext(ctx, "SELECT 'hello, world'").Scan(&greeting); err != nil {
		log.Fatal(err)
	}
	fmt.Println(greeting)
}

// ExampleOpen_withEngine shows the direct Pool path. Opening with
// [postgres.WithEngine] binds the pool to a running celeris.Server's event
// loop so driver FDs are serviced by the same epoll/io_uring instance as
// the HTTP workers, and per-worker idle conns match the HTTP handler's
// CPU affinity.
func ExampleOpen_withEngine() {
	// In real code, pass postgres.WithEngine(srv) where srv is a
	// *celeris.Server that has already been Start()-ed. Doing so binds
	// the pool to the HTTP event loop so driver FDs and HTTP workers
	// share the same epoll/io_uring instance.
	pool, err := postgres.Open(
		"postgres://app:secret@localhost:5432/mydb?sslmode=disable",
		postgres.WithMaxOpen(32),
		postgres.WithStatementCacheSize(512),
		postgres.WithApplication("api"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	ctx := context.Background()
	rows, err := pool.QueryContext(ctx,
		"SELECT id, name FROM users WHERE tenant = $1", 42)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("id=%d name=%s\n", id, name)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
}

// ExamplePool_BeginTx wraps a pair of updates in a serializable transaction
// and rolls back on any error.
func ExamplePool_BeginTx() {
	pool, err := postgres.Open("postgres://app:secret@localhost/mydb?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		log.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE accounts SET balance = balance - $1 WHERE id = $2", 100, "a"); err != nil {
		_ = tx.Rollback()
		log.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE accounts SET balance = balance + $1 WHERE id = $2", 100, "b"); err != nil {
		_ = tx.Rollback()
		log.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}
}

// ExampleTx_Savepoint demonstrates partial rollback via savepoints inside a
// larger transaction.
func ExampleTx_Savepoint() {
	pool, err := postgres.Open("postgres://app:secret@localhost/mydb?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "INSERT INTO jobs(name) VALUES ('always')"); err != nil {
		log.Fatal(err)
	}

	if err := tx.Savepoint(ctx, "maybe"); err != nil {
		log.Fatal(err)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO jobs(name) VALUES ('might-fail')")
	if err != nil {
		// Undo just the 'might-fail' insert; the 'always' row survives.
		if rbErr := tx.RollbackToSavepoint(ctx, "maybe"); rbErr != nil {
			log.Fatal(rbErr)
		}
	} else {
		if err := tx.ReleaseSavepoint(ctx, "maybe"); err != nil {
			log.Fatal(err)
		}
	}

	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}
}

// ExamplePool_CopyFrom bulk-loads a []row fixture into a table via PG's
// COPY FROM STDIN protocol. For large, live-generated streams, implement
// [postgres.CopyFromSource] directly instead of materialising to a slice.
func ExamplePool_CopyFrom() {
	pool, err := postgres.Open("postgres://app:secret@localhost/mydb?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	ctx := context.Background()
	rows := [][]any{
		{1, "alice", true},
		{2, "bob", false},
		{3, "carol", true},
	}
	n, err := pool.CopyFrom(ctx, "users", []string{"id", "name", "active"},
		postgres.CopyFromSlice(rows))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("inserted %d rows\n", n)
}
