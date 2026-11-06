package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"
)

type fakeRecord struct {
	conn  int
	query string
	args  []driver.NamedValue
	exec  bool
}

type fakeEngine struct {
	mu      sync.Mutex
	next    int
	records []fakeRecord
	closed  []int
	query   func(string, []driver.NamedValue) ([]string, [][]driver.Value, error)
	exec    func(string, []driver.NamedValue) (driver.Result, error)
}

type fakeConnector struct{ engine *fakeEngine }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.engine.mu.Lock()
	defer c.engine.mu.Unlock()
	c.engine.next++
	return &fakeConn{engine: c.engine, id: c.engine.next}, nil
}
func (c fakeConnector) Driver() driver.Driver { return fakeDriver{c.engine} }

type fakeDriver struct{ engine *fakeEngine }

func (d fakeDriver) Open(string) (driver.Conn, error) {
	return fakeConnector{d.engine}.Connect(context.Background())
}

type fakeConn struct {
	engine *fakeEngine
	id     int
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared statement")
}
func (c *fakeConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected Begin") }
func (c *fakeConn) Close() error {
	c.engine.mu.Lock()
	defer c.engine.mu.Unlock()
	c.engine.closed = append(c.engine.closed, c.id)
	return nil
}
func (c *fakeConn) record(query string, args []driver.NamedValue, exec bool) {
	c.engine.mu.Lock()
	defer c.engine.mu.Unlock()
	c.engine.records = append(c.engine.records, fakeRecord{c.id, query, append([]driver.NamedValue(nil), args...), exec})
}
func (c *fakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.record(query, args, false)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.engine.query == nil {
		return nil, errors.New("unexpected query")
	}
	columns, values, err := c.engine.query(query, args)
	if err != nil {
		return nil, err
	}
	return &fakeRows{columns: columns, values: values}, nil
}
func (c *fakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.record(query, args, true)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.engine.exec != nil {
		return c.engine.exec(query, args)
	}
	return driver.RowsAffected(0), nil
}

type fakeRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func connectFakeDatabase(t *testing.T, s *server, engine *fakeEngine) testBrowser {
	t.Helper()
	s.setOpenDatabase(func(_ context.Context, _ connectRequest) (*sql.DB, string, error) {
		db := sql.OpenDB(fakeConnector{engine})
		db.SetMaxIdleConns(0)
		return db, "8.mock", nil
	})
	b := bootstrapTestBrowser(t, s)
	rec := b.request(s, "POST", "/api/connect", `{"host":"127.0.0.1","port":3306,"user":"test","tls":"disable"}`)
	if rec.Code != 200 {
		t.Fatalf("fake connect failed: %d", rec.Code)
	}
	return b
}
