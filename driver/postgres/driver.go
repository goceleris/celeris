package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
)

// DriverName is the name under which the driver is registered with
// database/sql. Use it in sql.Open: sql.Open(postgres.DriverName, dsn).
const DriverName = "celeris-postgres"

// Driver implements database/sql/driver.Driver for PostgreSQL on top of the
// celeris event loop.
type Driver struct{}

var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.DriverContext = (*Driver)(nil)
)

// Open parses name as a DSN and returns a single driver.Conn. Callers
// generally prefer sql.Open + database/sql's built-in pooling over calling
// this directly.
func (d *Driver) Open(name string) (driver.Conn, error) {
	cn, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return cn.Connect(context.Background())
}

// OpenConnector parses name and returns a Connector. database/sql calls this
// on sql.Open; the returned Connector is used for every Conn the pool dials.
func (d *Driver) OpenConnector(name string) (driver.Connector, error) {
	return newConnector(name)
}

func init() {
	sql.Register(DriverName, &Driver{})
}
