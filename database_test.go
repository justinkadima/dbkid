package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

type noopDriver struct{}

func (noopDriver) Open(string) (driver.Conn, error) { return noopConn{}, nil }

type noopConnector struct{}

func (noopConnector) Connect(context.Context) (driver.Conn, error) { return noopConn{}, nil }
func (noopConnector) Driver() driver.Driver                        { return noopDriver{} }

type noopConn struct{}

func (noopConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (noopConn) Close() error                        { return nil }
func (noopConn) Begin() (driver.Tx, error)           { return nil, errors.New("not implemented") }

type testBrowser struct {
	cookie *http.Cookie
	token  string
}

func bootstrapTestBrowser(t *testing.T, s *server) testBrowser {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil))
	var body struct {
		Token string `json:"csrf_token"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(rec.Result().Cookies()) != 1 {
		t.Fatal("cannot bootstrap browser session")
	}
	return testBrowser{cookie: rec.Result().Cookies()[0], token: body.Token}
}

func (b testBrowser) request(s *server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost:8080"+path, strings.NewReader(body))
	req.AddCookie(b.cookie)
	req.Header.Set("X-CSRF-Token", b.token)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodPost && path == "/api/query" {
		id, err := randomToken()
		if err != nil {
			panic(err)
		}
		req.Header.Set("X-DBKid-Request-ID", id)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestConnectSessionAndFailedReconnect(t *testing.T) {
	s := newServer()
	// An in-memory SQL driver ensures this test cannot contact any database.
	injector, ok := interface{}(s).(interface {
		setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error))
	})
	if !ok {
		t.Fatal("backend does not yet support a test-only database connector")
	}
	var opened *sql.DB
	injector.setOpenDatabase(func(_ context.Context, request connectRequest) (*sql.DB, string, error) {
		if request.Host == "127.0.0.2" {
			return nil, "", errors.New("test-secret-not-for-output")
		}
		opened = sql.OpenDB(noopConnector{})
		return opened, "8.mock", nil
	})
	a := bootstrapTestBrowser(t, s)
	b := bootstrapTestBrowser(t, s)
	valid := `{"host":"127.0.0.1","port":3306,"user":"alice","password":"test-secret-not-for-output","tls":"disable"}`
	connected := a.request(s, http.MethodPost, "/api/connect", valid)
	if connected.Code != http.StatusOK || !strings.Contains(connected.Body.String(), `"connected":true`) || strings.Contains(connected.Body.String(), "test-secret-not-for-output") {
		t.Fatalf("connect response invalid: %d", connected.Code)
	}
	other := b.request(s, http.MethodGet, "/api/session", "")
	if !strings.Contains(other.Body.String(), `"connected":false`) {
		t.Fatal("connection leaked into another browser session")
	}
	failed := a.request(s, http.MethodPost, "/api/connect", strings.Replace(valid, "127.0.0.1", "127.0.0.2", 1))
	if failed.Code != http.StatusBadGateway || strings.Contains(failed.Body.String(), "test-secret-not-for-output") {
		t.Fatalf("failed reconnect response invalid: %d", failed.Code)
	}
	current := a.request(s, http.MethodGet, "/api/session", "")
	if !strings.Contains(current.Body.String(), `"connected":true`) || !strings.Contains(current.Body.String(), `"host":"127.0.0.1"`) {
		t.Fatal("failed reconnect discarded the old connection")
	}
	if err := opened.Ping(); err != nil {
		t.Fatalf("old connection is not usable after failed reconnect: %v", err)
	}
	disconnected := a.request(s, http.MethodPost, "/api/disconnect", "{}")
	if disconnected.Code != http.StatusOK || !strings.Contains(disconnected.Body.String(), `"connected":false`) {
		t.Fatalf("disconnect response: %d %s", disconnected.Code, disconnected.Body.String())
	}
	if err := opened.Ping(); err == nil {
		t.Fatal("disconnect must close the connection")
	}
}

func TestDisconnectSupersedesInFlightConnect(t *testing.T) {
	s := newServer()
	started, release := make(chan struct{}), make(chan struct{})
	created := sql.OpenDB(noopConnector{})
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) {
		close(started)
		<-release
		return created, "8.mock", nil
	})
	b := bootstrapTestBrowser(t, s)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- b.request(s, http.MethodPost, "/api/connect", `{"host":"127.0.0.1","port":3306,"user":"test","tls":"disable"}`)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("connect did not start")
	}
	disconnect := b.request(s, http.MethodPost, "/api/disconnect", "{}")
	close(release)
	connect := <-done
	if disconnect.Code != http.StatusOK || connect.Code != http.StatusConflict {
		t.Fatalf("disconnect/connect status = %d/%d", disconnect.Code, connect.Code)
	}
	if created.Ping() == nil {
		t.Fatal("superseded connection was not closed")
	}
}

func TestMySQLConfigDefaultsToVerifiedTLSAndSingleStatement(t *testing.T) {
	request := connectRequest{Host: "db.example", Port: 3306, User: "test", Password: "a:weird@password"}
	cfg := mysqlConfig(request)
	if cfg.TLS == nil || cfg.TLS.InsecureSkipVerify || cfg.TLS.ServerName != "db.example" || cfg.AllowFallbackToPlaintext {
		t.Fatal("TLS must default to verified with no plaintext fallback")
	}
	if cfg.MultiStatements || cfg.InterpolateParams || cfg.User != request.User || cfg.Passwd != request.Password || cfg.Timeout <= 0 || cfg.ReadTimeout <= 0 || cfg.WriteTimeout <= 0 {
		t.Fatal("MySQL configuration must preserve credentials and disable unsafe query modes")
	}
	local := mysqlConfig(connectRequest{Host: "localhost", Port: 3306, TLS: "disable"})
	if local.TLS != nil || local.Addr != "127.0.0.1:3306" {
		t.Fatal("local plaintext must bind to a literal loopback address")
	}
	remotePlaintext := connectRequest{Host: "db.example", Port: 3306, User: "test", TLS: "disable", AllowInsecure: true}
	if validateConnect(&remotePlaintext) != "" || mysqlConfig(remotePlaintext).TLS != nil {
		t.Fatal("explicitly acknowledged remote plaintext should be permitted")
	}
}

func TestConnectorDoesNotParseCredentialsAsDriverParameters(t *testing.T) {
	module, err := os.ReadFile("go.mod")
	if err != nil || !strings.Contains(string(module), "github.com/go-sql-driver/mysql v1.8.1") {
		t.Fatal("the Go 1.19-compatible mysql v1.8.1 security pin is required")
	}
	username := "name@tcp(example.invalid)/?allowAllFiles=true&multiStatements=true"
	request := connectRequest{Host: "example.invalid", Port: 3306, User: username, Password: "x/y?allowAllFiles=true", TLS: "verify"}
	config := mysqlConfig(request)
	if config.User != username || config.DBName != "" || config.AllowAllFiles || config.MultiStatements {
		t.Fatal("user-controlled values changed driver policy")
	}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		t.Fatal("connector rejected literal user values")
	}
	db := sql.OpenDB(connector) // Does not dial a server.
	if err := db.Close(); err != nil {
		t.Fatal("connector cleanup failed")
	}
}

func TestLoopbackDatabaseHostVariantsAreSafe(t *testing.T) {
	for _, tt := range []struct{ host, address string }{
		{"localhost.", "127.0.0.1:3306"},
		{"[::1]", "[::1]:3306"},
	} {
		t.Run(tt.host, func(t *testing.T) {
			request := connectRequest{Host: tt.host, Port: 3306, User: "test", TLS: "disable"}
			if message := validateConnect(&request); message != "" {
				t.Fatalf("loopback host rejected: %s", message)
			}
			if config := mysqlConfig(request); config.Addr != tt.address || config.TLS != nil {
				t.Fatalf("wrong loopback transport address: %q", config.Addr)
			}
		})
	}
}

func TestExpiredSessionClosesItsDatabase(t *testing.T) {
	s := newServer()
	clock, ok := interface{}(s).(interface{ setClock(func() time.Time) })
	if !ok {
		t.Fatal("session expiry is not implemented")
	}
	now := time.Unix(100000, 0)
	clock.setClock(func() time.Time { return now })
	var opened *sql.DB
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) {
		opened = sql.OpenDB(noopConnector{})
		return opened, "8.mock", nil
	})
	b := bootstrapTestBrowser(t, s)
	if rec := b.request(s, http.MethodPost, "/api/connect", `{"host":"127.0.0.1","port":3306,"user":"test","tls":"disable"}`); rec.Code != 200 {
		t.Fatalf("connect: %d", rec.Code)
	}
	now = now.Add(25 * time.Hour)
	rec := b.request(s, http.MethodGet, "/api/session", "")
	var result struct {
		Token     string `json:"csrf_token"`
		Connected bool   `json:"connected"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Connected || result.Token == b.token || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("expired session was not replaced: %d", rec.Code)
	}
	if opened.Ping() == nil {
		t.Fatal("expired session left its database open")
	}
	if old := b.request(s, http.MethodGet, "/api/databases", ""); old.Code != http.StatusForbidden {
		t.Fatalf("expired session token still accepted: %d", old.Code)
	}
}

func TestDatabasesReturnsStringsAndEmptyArray(t *testing.T) {
	engine := &fakeEngine{}
	names := [][]driver.Value{{"alpha"}, {"space name"}}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if !strings.Contains(strings.ToUpper(query), "INFORMATION_SCHEMA.SCHEMATA") || len(args) != 0 {
			return nil, nil, errors.New("unexpected database listing")
		}
		return []string{"SCHEMA_NAME"}, names, nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	for _, want := range []string{`["alpha","space name"]`, `[]`} {
		rec := b.request(s, http.MethodGet, "/api/databases", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"databases":`+want) {
			t.Fatalf("database list = %d %s, want %s", rec.Code, rec.Body.String(), want)
		}
		names = nil
	}
}

func TestTablesAndStructureUseParameters(t *testing.T) {
	database, table := "db' OR '1'='1", "strange`table"
	empty := false
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.Contains(query, database) || strings.Contains(query, table) || len(args) < 1 || args[0].Value != database {
			return nil, nil, errors.New("metadata values were not parameterized")
		}
		if strings.Contains(strings.ToUpper(query), "INFORMATION_SCHEMA.TABLES") {
			if len(args) != 1 {
				return nil, nil, errors.New("unexpected table arguments")
			}
			if empty {
				return []string{"TABLE_NAME", "TABLE_TYPE"}, nil, nil
			}
			return []string{"TABLE_NAME", "TABLE_TYPE"}, [][]driver.Value{{table, "BASE TABLE"}, {"v", "VIEW"}}, nil
		}
		if strings.Contains(strings.ToUpper(query), "INFORMATION_SCHEMA.COLUMNS") {
			if len(args) != 2 || args[1].Value != table {
				return nil, nil, errors.New("table value was not parameterized")
			}
			if empty {
				return []string{"COLUMN_NAME", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, nil, nil
			}
			return []string{"COLUMN_NAME", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, [][]driver.Value{
				{"id", "int(11)", "NO", "PRI", nil, "auto_increment"},
				{"label", "varchar(30)", "YES", "", "", ""},
			}, nil
		}
		return nil, nil, errors.New("unexpected metadata query")
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	tablesURL := "/api/tables?database=" + url.QueryEscape(database)
	columnsURL := "/api/structure?database=" + url.QueryEscape(database) + "&table=" + url.QueryEscape(table)
	tables := b.request(s, http.MethodGet, tablesURL, "")
	if tables.Code != 200 || !strings.Contains(tables.Body.String(), `"type":"BASE TABLE"`) || !strings.Contains(tables.Body.String(), `"type":"VIEW"`) {
		t.Fatalf("table list: %d %s", tables.Code, tables.Body.String())
	}
	structure := b.request(s, http.MethodGet, columnsURL, "")
	var result struct {
		Columns []struct {
			Name     string  `json:"name"`
			Nullable bool    `json:"nullable"`
			Default  *string `json:"default"`
		} `json:"columns"`
	}
	if structure.Code != 200 || json.Unmarshal(structure.Body.Bytes(), &result) != nil || len(result.Columns) != 2 || result.Columns[0].Nullable || result.Columns[0].Default != nil || !result.Columns[1].Nullable || result.Columns[1].Default == nil || *result.Columns[1].Default != "" {
		t.Fatalf("structure: %d %s", structure.Code, structure.Body.String())
	}
	empty = true
	if rec := b.request(s, http.MethodGet, tablesURL, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tables":[]`) {
		t.Fatalf("empty tables: %s", rec.Body.String())
	}
	if rec := b.request(s, http.MethodGet, columnsURL, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"columns":[]`) {
		t.Fatalf("empty structure: %s", rec.Body.String())
	}
}

func TestRowsQuoteIdentifiersAndBoundPagination(t *testing.T) {
	database, table := "odd`db", "items` OR 1=1 --"
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if query != "SELECT * FROM `odd``db`.`items`` OR 1=1 --` LIMIT ? OFFSET ?" || len(args) != 2 || args[0].Value != int64(201) || args[1].Value != int64(200) {
			return nil, nil, errors.New("identifiers or pagination were not safely encoded")
		}
		values := make([][]driver.Value, 201)
		for i := range values {
			values[i] = []driver.Value{nil, []byte(""), []byte{0xff, 0x00}}
		}
		return []string{"null_value", "empty", "binary"}, values, nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	path := "/api/rows?database=" + url.QueryEscape(database) + "&table=" + url.QueryEscape(table) + "&page=2&page_size=999"
	rec := b.request(s, http.MethodGet, path, "")
	var result struct {
		Columns  []string        `json:"columns"`
		Rows     [][]interface{} `json:"rows"`
		Page     int             `json:"page"`
		PageSize int             `json:"page_size"`
		HasNext  bool            `json:"has_next"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Columns) != 3 || len(result.Rows) != 200 || result.Page != 2 || result.PageSize != 200 || !result.HasNext {
		t.Fatalf("rows response: status %d, row count %d, body %s", rec.Code, len(result.Rows), rec.Body.String())
	}
	if result.Rows[0][0] != nil || result.Rows[0][1] != "" || result.Rows[0][2] != "0xff00" {
		t.Fatalf("NULL, empty and binary values were not preserved: %v", result.Rows[0])
	}
	overflow := b.request(s, http.MethodGet, "/api/rows?database=a&table=b&page=9223372036854775807&page_size=200", "")
	if overflow.Code != http.StatusBadRequest {
		t.Fatalf("overflowing page accepted: %d", overflow.Code)
	}
}

func TestQueryAlwaysRequiresExplicitConfirmation(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return nil, nil, errors.New("an unconfirmed query reached the database")
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	for _, body := range []string{
		`{"database":"example","sql":"SELECT 1"}`,
		`{"database":"example","sql":"DELETE FROM t","confirmed":false}`,
		`{"database":"example","sql":"   ","confirmed":true}`,
	} {
		rec := b.request(s, http.MethodPost, "/api/query", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("query must be rejected before execution: %d %s", rec.Code, rec.Body.String())
		}
	}
	if len(engine.records) != 0 {
		t.Fatal("an unconfirmed or empty statement reached the database")
	}
}

func TestQueryResultsAffectedRowsAndConnectionIsolation(t *testing.T) {
	const resultSQL = "WITH c AS (SELECT 'a;b') SELECT * FROM c"
	const updateSQL = "/*comment*/ UPDATE items SET x=1"
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if len(args) != 0 {
			return nil, nil, errors.New("unexpected SQL parameters")
		}
		switch query {
		case resultSQL:
			values := make([][]driver.Value, 501)
			for i := range values {
				values[i] = []driver.Value{[]byte("value")}
			}
			return []string{"answer"}, values, nil
		case updateSQL:
			return []string{}, nil, nil
		case "SELECT ROW_COUNT()":
			return []string{"ROW_COUNT()"}, [][]driver.Value{{int64(7)}}, nil
		case "SHOW TABLES":
			return []string{"name"}, nil, nil
		}
		return nil, nil, errors.New("unexpected SQL")
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	submit := func(database, statement string) (*httptest.ResponseRecorder, struct {
		Columns      []string        `json:"columns"`
		Rows         [][]interface{} `json:"rows"`
		AffectedRows *int64          `json:"affected_rows"`
		Truncated    bool            `json:"truncated"`
		Duration     int64           `json:"duration_ms"`
	}) {
		t.Helper()
		payload, _ := json.Marshal(queryRequest{Database: database, SQL: statement, Confirmed: true})
		rec := b.request(s, http.MethodPost, "/api/query", string(payload))
		var result struct {
			Columns      []string        `json:"columns"`
			Rows         [][]interface{} `json:"rows"`
			AffectedRows *int64          `json:"affected_rows"`
			Truncated    bool            `json:"truncated"`
			Duration     int64           `json:"duration_ms"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		return rec, result
	}
	rec, first := submit("odd`db", resultSQL)
	if rec.Code != 200 || len(first.Columns) != 1 || len(first.Rows) != 500 || !first.Truncated || first.AffectedRows != nil || first.Duration < 0 || first.Rows[0][0] != "value" {
		t.Fatalf("result-producing query: status %d, rows %d, body %s", rec.Code, len(first.Rows), rec.Body.String())
	}
	rec, changed := submit("odd`db", updateSQL)
	if rec.Code != 200 || len(changed.Columns) != 0 || len(changed.Rows) != 0 || changed.AffectedRows == nil || *changed.AffectedRows != 7 || changed.Truncated {
		t.Fatalf("affected-row query: %d %s", rec.Code, rec.Body.String())
	}
	rec, empty := submit("", "SHOW TABLES")
	if rec.Code != 200 || len(empty.Columns) != 1 || empty.Rows == nil || len(empty.Rows) != 0 || empty.AffectedRows != nil || empty.Truncated {
		t.Fatalf("empty result query: %d %s", rec.Code, rec.Body.String())
	}
	if len(engine.records) != 6 {
		t.Fatalf("unexpected query count: %d", len(engine.records))
	}
	r := engine.records
	if r[0].query != "USE `odd``db`" || !r[0].exec || r[0].conn != r[1].conn || r[2].query != "USE `odd``db`" || r[2].conn != r[3].conn || r[3].conn != r[4].conn || r[1].conn == r[3].conn || r[3].conn == r[5].conn {
		t.Fatal("database selection and statements must share one fresh connection per request")
	}
}

type closeWaitConnector struct{ conn *trackedMySQLConn }

func (c closeWaitConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c closeWaitConnector) Driver() driver.Driver                        { return noopDriver{} }

type closeWaitDriverConn struct {
	mysqlDriverConnection
	rows *closeWaitRows
}

func (c *closeWaitDriverConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return c.rows, nil
}
func (c *closeWaitDriverConn) CheckNamedValue(*driver.NamedValue) error { return nil }
func (c *closeWaitDriverConn) Close() error                             { return nil }

type closeWaitRows struct {
	peer    net.Conn
	columns []string
	values  []driver.Value
	count   int
	total   int
}

func (r *closeWaitRows) Columns() []string { return r.columns }
func (r *closeWaitRows) Next(dest []driver.Value) error {
	if r.count == r.total {
		return io.EOF
	}
	r.count++
	copy(dest, r.values)
	return nil
}
func (r *closeWaitRows) Close() error {
	// Simulate a driver draining an unread stream until the socket is closed.
	var b [1]byte
	_, _ = r.peer.Read(b[:])
	return nil
}

func TestEarlyResultTerminationClosesUnreadStream(t *testing.T) {
	for _, tt := range []struct {
		name    string
		columns []string
		values  []driver.Value
		count   int
		run     func(context.Context, *sql.DB) error
	}{
		{"query-row-limit", []string{"v"}, []driver.Value{"ok"}, 501, func(ctx context.Context, db *sql.DB) error {
			result, err := executeQuery(ctx, db, "", "SELECT many")
			if err == nil && (!result.Truncated || len(result.Rows) != 500) {
				return errors.New("row-limit response is incomplete")
			}
			return err
		}},
		{"browse-byte-limit", []string{"a", "b", "c"}, []driver.Value{strings.Repeat("x", 4096), strings.Repeat("x", 4096), strings.Repeat("x", 4096)}, 200, func(ctx context.Context, db *sql.DB) error {
			_, err := browseRows(ctx, db, "db", "table", 1, 200, 0)
			if !errors.Is(err, errResultTooLarge) {
				return errors.New("browse did not return its byte-limit error")
			}
			return nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			limited := &boundedConn{Conn: client, remaining: 1024}
			defer limited.Close()
			budget := &connectionBudget{conn: limited}
			driverConn := &closeWaitDriverConn{rows: &closeWaitRows{peer: peer, columns: tt.columns, values: tt.values, total: tt.count}}
			db := sql.OpenDB(closeWaitConnector{&trackedMySQLConn{mysqlDriverConnection: driverConn, budget: budget}})
			db.SetMaxIdleConns(0)
			defer db.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tt.run(ctx, db) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("early termination: %v", err)
				}
			case <-time.After(500 * time.Millisecond):
				_ = limited.Close() // release the simulated driver before failing
				<-done
				t.Fatal("Rows.Close attempted to drain an unread stream")
			}
		})
	}
}

func TestMySQLPreparedMetadataStillUsesParameters(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const database = "db' OR 1=1 --"
	serverDone := make(chan error, 1)
	go func() {
		for i := 0; i < 2; i++ {
			peer, err := listener.Accept()
			if err != nil {
				serverDone <- err
				return
			}
			_ = peer.SetDeadline(time.Now().Add(10 * time.Second))
			if i == 0 {
				err = serveSyntheticMySQL(peer, 0)
			} else {
				err = serveSyntheticPreparedQuery(peer, database)
			}
			_ = peer.Close()
			if err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := connectRequest{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "test", TLS: "disable"}
	db, _, err := openDatabase(ctx, request)
	if err != nil {
		t.Fatalf("synthetic connection: %v", err)
	}
	defer db.Close()
	tables, err := listTables(ctx, db, database)
	if err != nil || tables == nil || len(tables) != 0 {
		t.Fatalf("prepared metadata query: tables=%v, err=%v", tables, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("synthetic prepared peer: %v", err)
	}
}

func serveSyntheticPreparedQuery(conn net.Conn, database string) error {
	if err := syntheticMySQLWritePacket(conn, 0, syntheticMySQLHandshake()); err != nil {
		return err
	}
	if _, err := syntheticMySQLReadPacket(conn); err != nil {
		return err
	}
	if err := syntheticMySQLWritePacket(conn, 2, []byte{0, 0, 0, 2, 0, 0, 0}); err != nil {
		return err
	}
	prepared, err := syntheticMySQLReadPacket(conn)
	if err != nil || len(prepared) < 2 || prepared[0] != 0x16 || !bytes.Contains(prepared, []byte("?")) || bytes.Contains(prepared, []byte(database)) {
		return errors.New("synthetic peer expected a parameterized COM_STMT_PREPARE")
	}
	// One parameter, no prepare-time columns. A later execute supplies metadata.
	if err := syntheticMySQLWritePacket(conn, 1, []byte{0, 1, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0}); err != nil {
		return err
	}
	if err := syntheticMySQLWritePacket(conn, 2, []byte{1}); err != nil {
		return err
	}
	eof := []byte{0xfe, 0, 0, 2, 0}
	if err := syntheticMySQLWritePacket(conn, 3, eof); err != nil {
		return err
	}
	executed, err := syntheticMySQLReadPacket(conn)
	if err != nil || len(executed) == 0 || executed[0] != 0x17 || !bytes.Contains(executed, []byte(database)) {
		return errors.New("synthetic peer did not receive a bound COM_STMT_EXECUTE value")
	}
	if err := syntheticMySQLWritePacket(conn, 1, []byte{2}); err != nil {
		return err
	}
	column := []byte{3, 'd', 'e', 'f', 0, 0, 0, 1, 'v', 0, 12, 33, 0, 0xff, 0xff, 0, 0, 0xfd, 0, 0, 0, 0, 0}
	for sequence := byte(2); sequence < 4; sequence++ {
		if err := syntheticMySQLWritePacket(conn, sequence, column); err != nil {
			return err
		}
	}
	if err := syntheticMySQLWritePacket(conn, 4, eof); err != nil {
		return err
	}
	return syntheticMySQLWritePacket(conn, 5, eof)
}

func TestConnectRejectsOversizedServerVersion(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer peer.Close()
		_ = peer.SetDeadline(time.Now().Add(10 * time.Second))
		serverDone <- serveSyntheticMySQL(peer, 2<<20)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := connectRequest{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "test", TLS: "disable"}
	db, _, err := openDatabase(ctx, request)
	if db != nil {
		_ = db.Close()
	}
	if !errors.Is(err, errResultTooLarge) {
		t.Fatalf("oversized server version was not rejected with a result limit: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("synthetic peer: %v", err)
	}
}

func TestTwoMiBMySQLCellIsMarkedAndNextQueryWorks(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		for _, valueLength := range []int{0, 2 << 20, 0} {
			peer, err := listener.Accept()
			if err != nil {
				serverDone <- err
				return
			}
			_ = peer.SetDeadline(time.Now().Add(10 * time.Second))
			err = serveSyntheticMySQL(peer, valueLength)
			_ = peer.Close()
			if err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := connectRequest{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "test", TLS: "disable"}
	db, _, err := openDatabase(ctx, request)
	if err != nil {
		t.Fatalf("synthetic connection: %v", err)
	}
	defer db.Close()
	result, err := executeQuery(ctx, db, "", "SELECT REPEAT('x', 2097152)")
	if err != nil || len(result.Rows) != 1 || len(result.Rows[0]) != 1 || !result.CellsTruncated || result.Truncated {
		t.Fatalf("2 MiB result: rows=%d, cells_truncated=%v, truncated=%v, err=%v", len(result.Rows), result.CellsTruncated, result.Truncated, err)
	}
	value := result.Rows[0][0].(string)
	if len(value) > 1<<20 || !strings.HasSuffix(value, truncationMarker) {
		t.Fatalf("2 MiB result was not visibly bounded: length=%d", len(value))
	}
	if next, err := executeQuery(ctx, db, "", "SELECT 1"); err != nil || len(next.Rows) != 1 {
		t.Fatalf("next request failed after truncation: rows=%d, err=%v", len(next.Rows), err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("synthetic peer: %v", err)
	}
}

func TestIncomingLimitProducesExplicitResultError(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func(context.Context, *sql.DB) error
	}{
		{"query", func(ctx context.Context, db *sql.DB) error {
			_, err := executeQuery(ctx, db, "", "SELECT big")
			return err
		}},
		{"metadata", func(ctx context.Context, db *sql.DB) error { _, err := listDatabases(ctx, db); return err }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// A synthetic wire peer, not a MySQL server or a database fixture.
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			serverDone := make(chan error, 1)
			go func() {
				for i := 0; i < 2; i++ {
					peer, err := listener.Accept()
					if err != nil {
						serverDone <- err
						return
					}
					_ = peer.SetDeadline(time.Now().Add(10 * time.Second))
					valueLength := 0
					if i == 1 {
						valueLength = int(maxIncomingBytes) + (1 << 20)
					}
					err = serveSyntheticMySQL(peer, valueLength)
					_ = peer.Close()
					if err != nil {
						serverDone <- err
						return
					}
				}
				serverDone <- nil
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			request := connectRequest{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "test", TLS: "disable"}
			db, version, err := openDatabase(ctx, request)
			if err != nil {
				t.Fatalf("synthetic MySQL handshake: %v", err)
			}
			defer db.Close()
			if version != "8.fake" {
				t.Fatalf("synthetic version: %q", version)
			}
			err = tt.run(ctx, db)
			if err == nil {
				t.Fatal("wire result beyond the physical connection budget succeeded")
			}
			if message := databaseErrorMessage(err); !strings.Contains(message, "result") || !strings.Contains(message, "limit") {
				t.Fatalf("wire limit did not produce an explicit bounded-result error: %q (underlying %v)", message, err)
			}
			if err := <-serverDone; err != nil {
				t.Fatalf("synthetic peer: %v", err)
			}
		})
	}
}

func syntheticMySQLWritePacket(conn net.Conn, sequence byte, payload []byte) error {
	n := len(payload)
	header := []byte{byte(n), byte(n >> 8), byte(n >> 16), sequence}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

func syntheticMySQLReadPacket(conn net.Conn) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	n := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if n > 1<<20 {
		return nil, errors.New("unexpectedly large synthetic client packet")
	}
	payload := make([]byte, n)
	_, err := io.ReadFull(conn, payload)
	return payload, err
}

func syntheticMySQLHandshake() []byte {
	handshake := append([]byte{10}, []byte("8.fake\x00")...)
	handshake = append(handshake, 1, 0, 0, 0)
	handshake = append(handshake, []byte("12345678")...)
	handshake = append(handshake, 0, 0xff, 0xff, 33, 2, 0, 0xff, 0xff, 21)
	handshake = append(handshake, make([]byte, 10)...)
	return append(handshake, []byte("abcdefghijkl\x00mysql_native_password\x00")...)
}

func serveSyntheticMySQL(conn net.Conn, valueLength int) error {
	if err := syntheticMySQLWritePacket(conn, 0, syntheticMySQLHandshake()); err != nil {
		return err
	}
	if _, err := syntheticMySQLReadPacket(conn); err != nil {
		return err
	}
	if err := syntheticMySQLWritePacket(conn, 2, []byte{0, 0, 0, 2, 0, 0, 0}); err != nil {
		return err
	}
	query, err := syntheticMySQLReadPacket(conn)
	if err != nil {
		return err
	}
	if len(query) == 0 || query[0] != 3 {
		return errors.New("synthetic peer expected COM_QUERY")
	}
	return syntheticMySQLResult(conn, valueLength)
}

func syntheticMySQLResult(conn net.Conn, valueLength int) error {
	if err := syntheticMySQLWritePacket(conn, 1, []byte{1}); err != nil {
		return err
	}
	column := []byte{3, 'd', 'e', 'f', 0, 0, 0, 1, 'v', 0, 12, 33, 0, 0xff, 0xff, 0, 0, 0xfd, 0, 0, 0, 0, 0}
	if err := syntheticMySQLWritePacket(conn, 2, column); err != nil {
		return err
	}
	eof := []byte{0xfe, 0, 0, 2, 0}
	if err := syntheticMySQLWritePacket(conn, 3, eof); err != nil {
		return err
	}
	if valueLength == 0 {
		if err := syntheticMySQLWritePacket(conn, 4, append([]byte{6}, []byte("8.fake")...)); err != nil {
			return err
		}
		return syntheticMySQLWritePacket(conn, 5, eof)
	}
	packetLength := valueLength + 4 // 0xfd and three-byte length prefix
	header := []byte{byte(packetLength), byte(packetLength >> 8), byte(packetLength >> 16), 4,
		0xfd, byte(valueLength), byte(valueLength >> 8), byte(valueLength >> 16)}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	chunk := bytes.Repeat([]byte{'x'}, 32<<10)
	for remaining := valueLength; remaining > 0; {
		part := chunk
		if remaining < len(part) {
			part = part[:remaining]
		}
		n, err := conn.Write(part)
		remaining -= n
		if err != nil {
			return nil // the bounded client is expected to close the stream
		}
	}
	if int64(valueLength) > maxIncomingBytes {
		return nil
	}
	return syntheticMySQLWritePacket(conn, 5, eof)
}

func TestRegisteredMySQLDialerLimitsVerifiedTLS(t *testing.T) {
	certificate, roots := syntheticTLSCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer peer.Close()
		_ = peer.SetDeadline(time.Now().Add(10 * time.Second))
		if err := syntheticMySQLWritePacket(peer, 0, syntheticMySQLHandshake()); err != nil {
			serverDone <- err
			return
		}
		request, err := syntheticMySQLReadPacket(peer)
		if err != nil || len(request) != 32 || request[1]&0x08 == 0 {
			serverDone <- errors.New("client failed to request TLS before authentication")
			return
		}
		secure := tls.Server(peer, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
		if err := secure.Handshake(); err != nil {
			serverDone <- err
			return
		}
		if _, err := syntheticMySQLReadPacket(secure); err != nil {
			serverDone <- err
			return
		}
		if err := syntheticMySQLWritePacket(secure, 3, []byte{0, 0, 0, 2, 0, 0, 0}); err != nil {
			serverDone <- err
			return
		}
		query, err := syntheticMySQLReadPacket(secure)
		if err != nil || len(query) == 0 || query[0] != 3 {
			serverDone <- errors.New("synthetic TLS peer expected COM_QUERY")
			return
		}
		serverDone <- syntheticMySQLResult(secure, int(maxIncomingBytes)+(1<<20))
	}()
	request := connectRequest{Host: "db.test", Port: listener.Addr().(*net.TCPAddr).Port, User: "test", TLS: "verify"}
	cfg := mysqlConfig(request)
	cfg.Addr = listener.Addr().String()
	cfg.TLS.RootCAs = roots
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(trackedMySQLConnector{Connector: connector})
	db.SetMaxIdleConns(0)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = executeQuery(ctx, db, "", "SELECT big")
	if !errors.Is(err, errResultTooLarge) {
		t.Fatalf("verified TLS query did not enforce the inbound limit: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("synthetic TLS peer: %v", err)
	}
}

type multiResultConnector struct{}

func (multiResultConnector) Connect(context.Context) (driver.Conn, error) {
	return multiResultConn{}, nil
}
func (multiResultConnector) Driver() driver.Driver { return noopDriver{} }

type multiResultConn struct{ noopConn }

func (multiResultConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &multiResultRows{}, nil
}

type multiResultRows struct {
	set  int
	read bool
}

func (r *multiResultRows) Columns() []string { return []string{"v"} }
func (r *multiResultRows) Close() error      { return nil }
func (r *multiResultRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = "value"
	return nil
}
func (r *multiResultRows) HasNextResultSet() bool { return r.set == 0 }
func (r *multiResultRows) NextResultSet() error {
	if r.set != 0 {
		return io.EOF
	}
	r.set++
	r.read = false
	return nil
}

func TestMultipleResultSetsAreNotSilentlyDiscarded(t *testing.T) {
	db := sql.OpenDB(multiResultConnector{})
	db.SetMaxIdleConns(0)
	defer db.Close()
	result, err := executeQuery(context.Background(), db, "", "CALL example()")
	if err == nil {
		t.Fatalf("multiple result sets were silently presented as complete: %d rows, truncated=%v", len(result.Rows), result.Truncated)
	}
	if message := databaseErrorMessage(err); !strings.Contains(message, "result") || !strings.Contains(message, "multiple") {
		t.Fatalf("multiple-result error is not informative: %q", message)
	}
}

func TestMetadataResponsesRejectAggregateOverBudget(t *testing.T) {
	big := strings.Repeat("x", 4096)
	for _, tt := range []struct {
		name    string
		columns []string
		value   []driver.Value
		run     func(*sql.DB) error
	}{
		{"databases", []string{"name"}, []driver.Value{big}, func(db *sql.DB) error { _, err := listDatabases(context.Background(), db); return err }},
		{"tables", []string{"name", "type"}, []driver.Value{big, "BASE TABLE"}, func(db *sql.DB) error { _, err := listTables(context.Background(), db, "db"); return err }},
		{"columns", []string{"name", "type", "nullable", "key", "default", "extra"}, []driver.Value{"n", "text", "YES", "", big, ""}, func(db *sql.DB) error {
			_, err := listColumns(context.Background(), db, "db", "table")
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			engine := &fakeEngine{query: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
				values := make([][]driver.Value, 600)
				for i := range values {
					values[i] = tt.value
				}
				return tt.columns, values, nil
			}}
			db := sql.OpenDB(fakeConnector{engine})
			db.SetMaxIdleConns(0)
			defer db.Close()
			if err := tt.run(db); !errors.Is(err, errResultTooLarge) {
				t.Fatalf("oversized metadata should fail with a result limit: %v", err)
			}
		})
	}
}

func TestConcurrentRequestsUseFreshPhysicalConnections(t *testing.T) {
	firstStarted, releaseFirst := make(chan struct{}), make(chan struct{})
	engine := &fakeEngine{}
	engine.query = func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if query == "first" {
			close(firstStarted)
			<-releaseFirst
		}
		return []string{"v"}, [][]driver.Value{{"ok"}}, nil
	}
	db := sql.OpenDB(fakeConnector{engine})
	db.SetMaxIdleConns(0)
	db.SetMaxOpenConns(1)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 2)
	go func() { _, err := executeQuery(ctx, db, "", "first"); done <- err }()
	<-firstStarted
	go func() { _, err := executeQuery(ctx, db, "", "second"); done <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for db.Stats().WaitCount == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waiting := db.Stats().WaitCount > 0
	close(releaseFirst)
	if !waiting {
		t.Fatal("second request did not wait for the busy pool")
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("request failed: %v", err)
		}
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.records) != 2 || engine.records[0].conn == engine.records[1].conn {
		t.Fatalf("pool handed one physical connection to two requests: %+v", engine.records)
	}
}

func TestRequestsDoNotRetrySQLOnBadConnection(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func(*sql.DB) error
	}{
		{"databases", func(db *sql.DB) error { _, err := listDatabases(context.Background(), db); return err }},
		{"tables", func(db *sql.DB) error { _, err := listTables(context.Background(), db, "db"); return err }},
		{"columns", func(db *sql.DB) error { _, err := listColumns(context.Background(), db, "db", "table"); return err }},
		{"browse", func(db *sql.DB) error {
			_, err := browseRows(context.Background(), db, "db", "table", 1, 10, 0)
			return err
		}},
		{"query", func(db *sql.DB) error { _, err := executeQuery(context.Background(), db, "", "SELECT 1"); return err }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			engine := &fakeEngine{query: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
				return nil, nil, driver.ErrBadConn
			}}
			db := sql.OpenDB(fakeConnector{engine})
			db.SetMaxIdleConns(0)
			defer db.Close()
			if err := tt.run(db); err == nil {
				t.Fatal("bad connection did not fail")
			}
			if len(engine.records) != 1 {
				t.Fatalf("SQL was automatically attempted %d times", len(engine.records))
			}
		})
	}
}

func TestQueryReportsCellTruncationSeparatelyFromRowLimit(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return []string{"value"}, [][]driver.Value{{strings.Repeat("a", 4097)}}, nil
	}
	db := sql.OpenDB(fakeConnector{engine})
	db.SetMaxIdleConns(0)
	defer db.Close()
	result, err := executeQuery(context.Background(), db, "", "SELECT value")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var flags struct {
		CellsTruncated bool `json:"cells_truncated"`
		Truncated      bool `json:"truncated"`
	}
	if err := json.Unmarshal(encoded, &flags); err != nil || !flags.CellsTruncated || flags.Truncated {
		t.Fatalf("cell truncation was not distinguished from the row limit: %+v", flags)
	}
	if len(result.Rows) != 1 || !strings.HasSuffix(result.Rows[0][0].(string), "[truncated]") {
		t.Fatal("large cell is not visibly marked")
	}
}

func TestBrowseTruncatedCellsPreservePageRows(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(_ string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if len(args) != 2 || args[0].Value != int64(3) {
			return nil, nil, errors.New("unexpected browse arguments")
		}
		switch args[1].Value {
		case int64(0):
			return []string{"id", "value"}, [][]driver.Value{{"1", strings.Repeat("x", 6000)}, {"2", "ordinary"}, {"3", "lookahead"}}, nil
		case int64(2):
			return []string{"id", "value"}, [][]driver.Value{{"3", "lookahead"}}, nil
		default:
			return nil, nil, errors.New("unexpected offset")
		}
	}
	db := sql.OpenDB(fakeConnector{engine})
	db.SetMaxIdleConns(0)
	defer db.Close()
	first, err := browseRows(context.Background(), db, "db", "table", 1, 2, 0)
	if err != nil || len(first.Rows) != 2 || !first.HasNext || !first.CellsTruncated || first.Rows[1][0] != "2" {
		t.Fatalf("first page lost a row or its truncation flag: rows=%v, has_next=%v, cells_truncated=%v, err=%v", len(first.Rows), first.HasNext, first.CellsTruncated, err)
	}
	if !strings.HasSuffix(first.Rows[0][1].(string), truncationMarker) {
		t.Fatal("large browse cell is not visibly marked")
	}
	second, err := browseRows(context.Background(), db, "db", "table", 2, 2, 2)
	if err != nil || len(second.Rows) != 1 || second.Rows[0][0] != "3" || second.HasNext || second.CellsTruncated {
		t.Fatalf("lookahead row was skipped or misreported: %+v, err=%v", second, err)
	}
}

func TestBrowseBudgetCountsEscapedJSON(t *testing.T) {
	for _, tt := range []struct {
		rows    int
		wantErr bool
	}{
		{80, false},
		{100, true},
	} {
		engine := &fakeEngine{query: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
			values := make([][]driver.Value, tt.rows)
			for i := range values {
				values[i] = []driver.Value{strings.Repeat("\x00", maxCellDisplayBytes)}
			}
			return []string{"value"}, values, nil
		}}
		db := sql.OpenDB(fakeConnector{engine})
		db.SetMaxIdleConns(0)
		result, err := browseRows(context.Background(), db, "db", "table", 1, 200, 0)
		_ = db.Close()
		if tt.wantErr {
			if !errors.Is(err, errResultTooLarge) {
				t.Fatalf("JSON expansion for %d rows was not rejected: %v", tt.rows, err)
			}
			continue
		}
		encoded, marshalErr := json.Marshal(result)
		if err != nil || marshalErr != nil || len(encoded) > maxResultJSONBytes || len(result.Rows) != tt.rows {
			t.Fatalf("bounded escaped JSON: rows=%d, bytes=%d, err=%v, marshal=%v", len(result.Rows), len(encoded), err, marshalErr)
		}
	}
}

func TestBrowseRejectsAggregateOverBudgetWithoutSkippingRows(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
		values := make([][]driver.Value, 200)
		cell := strings.Repeat("x", 4096)
		for i := range values {
			values[i] = []driver.Value{cell, cell, cell}
		}
		return []string{"a", "b", "c"}, values, nil
	}
	db := sql.OpenDB(fakeConnector{engine})
	db.SetMaxIdleConns(0)
	defer db.Close()
	result, err := browseRows(context.Background(), db, "d", "t", 1, 200, 0)
	if err == nil {
		t.Fatalf("oversized page was returned instead of an explicit limit error: %d rows, has_next=%v", len(result.Rows), result.HasNext)
	}
	if message := databaseErrorMessage(err); !strings.Contains(message, "result") || !strings.Contains(message, "limit") {
		t.Fatalf("oversized page did not report a useful bounded-result error: %q", message)
	}
}

func TestMySQLConfigUsesBoundedDialer(t *testing.T) {
	config := mysqlConfig(connectRequest{Host: "127.0.0.1", Port: 3306, User: "test", TLS: "disable"})
	if config.Net == "tcp" || config.Net == "" {
		t.Fatalf("MySQL uses the unrestricted network %q rather than a bounded dialer", config.Net)
	}
	if _, err := mysql.NewConnector(config); err != nil {
		t.Fatalf("bounded MySQL configuration must work with NewConnector: %v", err)
	}
}

func TestDisplayValueLimitsOversizedUTF8Cell(t *testing.T) {
	value := strings.Repeat("é", 100000)
	displayed, truncated := displayValue(value)
	got, ok := displayed.(string)
	if !ok || !truncated || len(got) > 4096+len("[truncated]") || !strings.HasSuffix(got, "[truncated]") {
		t.Fatalf("large cell was not bounded and marked: type %T, length %d", displayed, len(got))
	}
}

func TestDisplayValueUTF8AndBinaryBoundaries(t *testing.T) {
	for _, input := range []interface{}{
		strings.Repeat("a", maxCellDisplayBytes-1) + "€!",
		[]byte(strings.Repeat("a", maxCellDisplayBytes-1) + "€!"),
	} {
		displayed, truncated := displayValue(input)
		got := displayed.(string)
		if !truncated || !utf8.ValidString(got) || got != strings.Repeat("a", maxCellDisplayBytes-1)+truncationMarker {
			t.Fatalf("UTF-8 split at a rune boundary incorrectly: len=%d, truncated=%v", len(got), truncated)
		}
	}
	binaryBoundary := append([]byte{0xff}, bytes.Repeat([]byte{'a'}, (maxCellDisplayBytes-2)/2-1)...)
	got, truncated := displayValue(binaryBoundary)
	if truncated || len(got.(string)) != maxCellDisplayBytes || !strings.HasPrefix(got.(string), "0xff") {
		t.Fatalf("exact-boundary binary was changed: len=%d, truncated=%v", len(got.(string)), truncated)
	}
	got, truncated = displayValue(append(binaryBoundary, 'a'))
	if !truncated || len(got.(string)) != maxCellDisplayBytes+len(truncationMarker) || !strings.HasSuffix(got.(string), truncationMarker) {
		t.Fatalf("oversized binary was not bounded and marked: len=%d, truncated=%v", len(got.(string)), truncated)
	}
	got, truncated = displayValue("literal " + truncationMarker)
	if truncated || got != "literal "+truncationMarker {
		t.Fatal("an ordinary value ending in the marker was misreported as truncated")
	}
}

func TestBoundedConnExactBoundaryAndClose(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	limited := &boundedConn{Conn: client, remaining: 8}
	type writeResult struct {
		n   int
		err error
	}
	written := make(chan writeResult, 1)
	go func() {
		n, err := peer.Write([]byte("123456789"))
		written <- writeResult{n, err}
	}()
	var got [8]byte
	if n, err := io.ReadFull(limited, got[:]); n != 8 || err != nil || string(got[:]) != "12345678" {
		t.Fatalf("exact budget read = %d, %v, %q", n, err, got)
	}
	if n, err := limited.Read(nil); n != 0 || err != nil {
		t.Fatalf("empty read at boundary = %d, %v", n, err)
	}
	if n, err := limited.Read(make([]byte, 1)); n != 0 || !errors.Is(err, errResultTooLarge) {
		t.Fatalf("read past budget = %d, %v", n, err)
	}
	outcome := <-written
	if outcome.n != 8 || outcome.err == nil {
		t.Fatalf("peer sent beyond budget or was not closed: %+v", outcome)
	}
}

func TestBoundedConnConcurrentReaders(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	const budget = 8192
	limited := &boundedConn{Conn: client, remaining: budget}
	type outcome struct {
		n   int
		err error
	}
	writer := make(chan outcome, 1)
	go func() {
		n, err := peer.Write(bytes.Repeat([]byte{'x'}, budget+1024))
		writer <- outcome{n, err}
	}()
	results := make(chan outcome, 8)
	var wg sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var total int
			buf := make([]byte, 97)
			for {
				n, err := limited.Read(buf)
				total += n
				if err != nil {
					results <- outcome{total, err}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(results)
	total := 0
	for result := range results {
		total += result.n
		if !errors.Is(result.err, errResultTooLarge) {
			t.Fatalf("concurrent read returned %v", result.err)
		}
	}
	if sent := <-writer; total != budget || sent.n != budget || sent.err == nil {
		t.Fatalf("concurrent budget: read=%d, written=%d, err=%v", total, sent.n, sent.err)
	}
}

func TestBoundedConnCloseUnblocksReader(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	limited := &boundedConn{Conn: client, remaining: 10}
	result := make(chan error, 1)
	go func() {
		_, err := limited.Read(make([]byte, 1))
		result <- err
	}()
	_ = limited.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closing a blocked read did not fail it")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("closing the connection did not unblock Read")
	}
}

func syntheticTLSCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"db.test"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

func TestBoundedConnLimitsVerifiedTLSCiphertext(t *testing.T) {
	certificate, roots := syntheticTLSCertificate(t)
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	const budget = 32 << 10
	limited := &boundedConn{Conn: client, remaining: budget}
	tlsClient := tls.Client(limited, &tls.Config{RootCAs: roots, ServerName: "db.test", MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
	tlsServer := tls.Server(peer, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
	serverDone := make(chan error, 1)
	go func() {
		if err := tlsServer.Handshake(); err != nil {
			serverDone <- err
			return
		}
		_, err := tlsServer.Write(bytes.Repeat([]byte("z"), 2*budget))
		serverDone <- err
	}()
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("verified TLS handshake failed: %v", err)
	}
	plaintext, err := io.ReadAll(tlsClient)
	if err == nil || len(plaintext) == 0 || len(plaintext) >= budget || limited.remaining != 0 {
		t.Fatalf("TLS ciphertext limit: plaintext=%d, remaining=%d, err=%v", len(plaintext), limited.remaining, err)
	}
	if err := <-serverDone; err == nil {
		t.Fatal("TLS peer was not interrupted at the byte limit")
	}
}
