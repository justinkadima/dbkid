package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

const boundedMySQLNetwork = "dbkid-bounded-tcp"

// Count all inbound TCP bytes per physical connection, including handshake and
// TLS ciphertext. TLS is negotiated by the MySQL driver over this wrapper;
// mysql v1.8.1's MaxAllowedPacket does not limit incoming packets.
// The driver can preallocate a full advertised 24-bit packet before reading it;
// this wire cap is not a bound on allocations from malicious protocol metadata.
const maxIncomingBytes int64 = 8 << 20

var errResultTooLarge = errors.New("database result exceeds the permitted size")
var errMultipleResultSets = errors.New("multiple database result sets are not supported")

type boundedConn struct {
	net.Conn
	readMu    sync.Mutex
	remaining int64
	exceeded  uint32 // atomic: driver errors otherwise hide the Read error
}

// Read serializes concurrent readers so they cannot collectively exceed the
// budget. The read after the exact boundary fails and closes the socket.
func (c *boundedConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.remaining <= 0 {
		atomic.StoreUint32(&c.exceeded, 1)
		_ = c.Conn.Close()
		return 0, errResultTooLarge
	}
	if int64(len(p)) > c.remaining {
		p = p[:int(c.remaining)]
	}
	n, err := c.Conn.Read(p)
	c.remaining -= int64(n)
	return n, err
}

type connectionBudgetKey struct{}

// wireLimitKey overrides the per-connection inbound byte cap for streaming
// handlers (import/export) that legitimately read more than maxIncomingBytes.
type wireLimitKey struct{}

type connectionBudget struct {
	mu   sync.Mutex
	conn *boundedConn
}

func (b *connectionBudget) attach(conn *boundedConn) {
	b.mu.Lock()
	b.conn = conn
	b.mu.Unlock()
}

func (b *connectionBudget) connection() *boundedConn {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn
}

func (b *connectionBudget) resultError(err error) error {
	if conn := b.connection(); err != nil && conn != nil && atomic.LoadUint32(&conn.exceeded) != 0 {
		return errResultTooLarge
	}
	return err
}

func (b *connectionBudget) abort() {
	if conn := b.connection(); conn != nil {
		_ = conn.Close()
	}
}

// Embedding the full set of mysql v1.8.1 optional interfaces preserves the
// driver's query, prepared-statement, argument, cancellation and ping paths.
type mysqlDriverConnection interface {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.QueryerContext
	driver.ExecerContext
	driver.NamedValueChecker
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type trackedMySQLConn struct {
	mysqlDriverConnection
	budget *connectionBudget
}

type trackedMySQLConnector struct{ driver.Connector }

func (c trackedMySQLConnector) Connect(ctx context.Context) (driver.Conn, error) {
	budget := &connectionBudget{}
	raw, err := c.Connector.Connect(context.WithValue(ctx, connectionBudgetKey{}, budget))
	if err != nil {
		return nil, budget.resultError(err)
	}
	if budget.connection() == nil {
		_ = raw.Close()
		return nil, errors.New("bounded MySQL dialer was not used")
	}
	conn, ok := raw.(mysqlDriverConnection)
	if !ok {
		_ = raw.Close()
		return nil, errors.New("MySQL driver lacks required connection interfaces")
	}
	return &trackedMySQLConn{mysqlDriverConnection: conn, budget: budget}, nil
}

func dialBoundedMySQL(ctx context.Context, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	// The driver cannot see through the wrapper to set TCP keepalive itself.
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
	}
	limit := maxIncomingBytes
	if override, ok := ctx.Value(wireLimitKey{}).(int64); ok && override > 0 && override <= 256<<20 {
		limit = override
	}
	limited := &boundedConn{Conn: conn, remaining: limit}
	if budget, ok := ctx.Value(connectionBudgetKey{}).(*connectionBudget); ok {
		budget.attach(limited)
	}
	return limited, nil
}

func init() {
	// Driver diagnostics can contain server-controlled data; never log them.
	_ = mysql.SetLogger(log.New(io.Discard, "", 0))
	mysql.RegisterDialContext(boundedMySQLNetwork, dialBoundedMySQL)
}

type connectRequest struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password"`
	TLS           string `json:"tls"`
	AllowInsecure bool   `json:"allow_insecure"`
}

func validateConnect(request *connectRequest) string {
	host, isLocal, valid := normalizeDatabaseHost(request.Host)
	if !valid || host == "" {
		return "invalid database host"
	}
	if request.Port < 1 || request.Port > 65535 {
		return "invalid database port"
	}
	if request.User == "" || len(request.User) > 256 || strings.ContainsRune(request.User, 0) {
		return "invalid database user"
	}
	if request.TLS == "" {
		request.TLS = "verify"
	}
	if request.TLS != "verify" && request.TLS != "disable" {
		return "TLS must be verify or disable"
	}
	if !isLocal && request.TLS == "disable" && !request.AllowInsecure {
		return "remote plaintext requires explicit allow_insecure"
	}
	return ""
}

func normalizeDatabaseHost(host string) (string, bool, bool) {
	if host == "" || len(host) > 253 || host != strings.TrimSpace(host) || strings.ContainsAny(host, "\\/@?#% 	\r\n\x00") {
		return "", false, false
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
		if ip := net.ParseIP(host); ip == nil || !strings.Contains(host, ":") {
			return "", false, false
		}
	} else if strings.ContainsAny(host, "[]") {
		return "", false, false
	}
	if strings.EqualFold(host, "localhost") || strings.EqualFold(host, "localhost.") {
		return "127.0.0.1", true, true
	}
	ip := net.ParseIP(host)
	if strings.Contains(host, ":") && ip == nil {
		return "", false, false
	}
	return host, ip != nil && ip.IsLoopback(), true
}

func mysqlConfig(request connectRequest) *mysql.Config {
	config := mysql.NewConfig()
	config.User = request.User
	config.Passwd = request.Password
	config.Net = boundedMySQLNetwork
	host, _, _ := normalizeDatabaseHost(request.Host)
	config.Addr = net.JoinHostPort(host, strconv.Itoa(request.Port))
	config.AllowAllFiles = false
	config.MultiStatements = false
	config.InterpolateParams = false
	config.AllowFallbackToPlaintext = false
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 20 * time.Second
	config.WriteTimeout = 10 * time.Second
	if request.TLS != "disable" {
		serverName := strings.TrimSuffix(strings.Trim(request.Host, "[]"), ".")
		config.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	}
	return config
}

func requestConn(ctx context.Context, db *sql.DB) (*sql.Conn, *connectionBudget, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	var budget *connectionBudget
	if err := conn.Raw(func(raw interface{}) error {
		if tracked, ok := raw.(*trackedMySQLConn); ok {
			budget = tracked.budget
		}
		return nil
	}); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, budget, nil
}

// Even with zero idle connections, database/sql can hand a just-released
// connection directly to a waiting request. Discard it explicitly instead.
func closeRequestConn(conn *sql.Conn) {
	_ = conn.Raw(func(interface{}) error { return driver.ErrBadConn })
	_ = conn.Close()
}

func openDatabase(ctx context.Context, request connectRequest) (*sql.DB, string, error) {
	connector, err := mysql.NewConnector(mysqlConfig(request))
	if err != nil {
		return nil, "", err
	}
	db := sql.OpenDB(trackedMySQLConnector{Connector: connector})
	// Every operation uses a new physical connection. USE, transactions and
	// other session state must never be visible in a later HTTP request.
	db.SetMaxIdleConns(0)
	db.SetMaxOpenConns(4)
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, "", err
	}
	defer closeRequestConn(conn)
	var version string
	if err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		_ = db.Close()
		return nil, "", budget.resultError(err)
	}
	if len(version) > maxCellDisplayBytes {
		_ = db.Close()
		return nil, "", errResultTooLarge
	}
	return db, version, nil
}

func databaseErrorMessage(err error) string {
	if errors.Is(err, errResultTooLarge) {
		return "database result exceeded the size limit"
	}
	if errors.Is(err, errMultipleResultSets) {
		return "multiple database result sets are not supported"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "database operation timed out"
	}
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) {
		return fmt.Sprintf("database error %d", mysqlError.Number)
	}
	return "database operation failed"
}

func requireSingleResultSet(rows *sql.Rows) error {
	if rows.NextResultSet() {
		return errMultipleResultSets
	}
	return rows.Err()
}

// Account for JSON escaping, not merely the UTF-8 bytes in the values.
func accountJSONValue(used *int, value interface{}) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded)+1 > maxResultJSONBytes-*used {
		return errResultTooLarge
	}
	*used += len(encoded) + 1 // allow for a comma
	return nil
}

func listDatabases(ctx context.Context, db *sql.DB) (result []string, resultErr error) {
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		return nil, err
	}
	defer closeRequestConn(conn)
	defer func() { resultErr = budget.resultError(resultErr) }()
	rows, err := conn.QueryContext(ctx, "SELECT SCHEMA_NAME FROM INFORMATION_SCHEMA.SCHEMATA ORDER BY SCHEMA_NAME")
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			budget.abort()
		}
		if err := rows.Close(); resultErr == nil && err != nil {
			result, resultErr = nil, err
		}
	}()
	used := resultEnvelopeBytes
	result = make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if len(name) > maxCellDisplayBytes {
			return nil, errResultTooLarge
		}
		if err := accountJSONValue(&used, name); err != nil {
			return nil, err
		}
		result = append(result, name)
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, err
	}
	return result, nil
}

func validIdentifier(name string) bool {
	return name != "" && len(name) <= 1024 && !strings.ContainsRune(name, 0) && utf8.ValidString(name)
}

type tableInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func listTables(ctx context.Context, db *sql.DB, database string) (result []tableInfo, resultErr error) {
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		return nil, err
	}
	defer closeRequestConn(conn)
	defer func() { resultErr = budget.resultError(resultErr) }()
	rows, err := conn.QueryContext(ctx, `SELECT TABLE_NAME, TABLE_TYPE FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE IN ('BASE TABLE', 'VIEW') ORDER BY TABLE_NAME`, database)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			budget.abort()
		}
		if err := rows.Close(); resultErr == nil && err != nil {
			result, resultErr = nil, err
		}
	}()
	used := resultEnvelopeBytes
	result = make([]tableInfo, 0)
	for rows.Next() {
		var table tableInfo
		if err := rows.Scan(&table.Name, &table.Type); err != nil {
			return nil, err
		}
		if len(table.Name) > maxCellDisplayBytes || len(table.Type) > maxCellDisplayBytes {
			return nil, errResultTooLarge
		}
		if err := accountJSONValue(&used, table); err != nil {
			return nil, err
		}
		result = append(result, table)
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, err
	}
	return result, nil
}

type columnInfo struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable bool    `json:"nullable"`
	Key      string  `json:"key"`
	Default  *string `json:"default"`
	Extra    string  `json:"extra"`
}

func listColumns(ctx context.Context, db *sql.DB, database, table string) (result []columnInfo, resultErr error) {
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		return nil, err
	}
	defer closeRequestConn(conn)
	defer func() { resultErr = budget.resultError(resultErr) }()
	rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_KEY, COLUMN_DEFAULT, EXTRA
		FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, database, table)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			budget.abort()
		}
		if err := rows.Close(); resultErr == nil && err != nil {
			result, resultErr = nil, err
		}
	}()
	used := resultEnvelopeBytes
	result = make([]columnInfo, 0)
	for rows.Next() {
		var column columnInfo
		var nullable string
		var defaultValue sql.NullString
		if err := rows.Scan(&column.Name, &column.Type, &nullable, &column.Key, &defaultValue, &column.Extra); err != nil {
			return nil, err
		}
		if len(column.Name) > maxCellDisplayBytes || len(column.Type) > maxCellDisplayBytes ||
			len(nullable) > maxCellDisplayBytes || len(column.Key) > maxCellDisplayBytes ||
			len(defaultValue.String) > maxCellDisplayBytes || len(column.Extra) > maxCellDisplayBytes {
			return nil, errResultTooLarge
		}
		column.Nullable = strings.EqualFold(nullable, "YES")
		if defaultValue.Valid {
			column.Default = &defaultValue.String
		}
		if err := accountJSONValue(&used, column); err != nil {
			return nil, err
		}
		result = append(result, column)
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, err
	}
	return result, nil
}

func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

const maxCellDisplayBytes = 4 << 10
const truncationMarker = "[truncated]"

// This is a JSON-encoded response budget, not just a count of raw cell bytes.
// Reserve space for the enclosing object's keys, page metadata and duration.
const maxResultJSONBytes = 2 << 20
const maxResultColumns = 512
const resultEnvelopeBytes = 512

// A displayed prefix is at most 4 KiB, excluding the explicit marker.
// Binary values retain the existing 0x/hex representation.
func displayValue(value interface{}) (interface{}, bool) {
	switch value := value.(type) {
	case nil:
		return nil, false
	case []byte:
		if !utf8.Valid(value) {
			return displayBinary(value)
		}
		if len(value) <= maxCellDisplayBytes {
			return string(value), false
		}
		end := maxCellDisplayBytes
		for !utf8.RuneStart(value[end]) {
			end--
		}
		return string(value[:end]) + truncationMarker, true
	case string:
		if !utf8.ValidString(value) {
			// Only convert the portion that can actually be displayed.
			end := (maxCellDisplayBytes - 2) / 2
			if len(value) <= end {
				return "0x" + hex.EncodeToString([]byte(value)), false
			}
			return "0x" + hex.EncodeToString([]byte(value[:end])) + truncationMarker, true
		}
		if len(value) <= maxCellDisplayBytes {
			return value, false
		}
		end := maxCellDisplayBytes
		for !utf8.RuneStart(value[end]) {
			end--
		}
		return value[:end] + truncationMarker, true
	case time.Time:
		return value.Format(time.RFC3339Nano), false
	default:
		return displayValue(fmt.Sprint(value))
	}
}

func displayBinary(value []byte) (string, bool) {
	end := (maxCellDisplayBytes - 2) / 2
	if len(value) <= end {
		return "0x" + hex.EncodeToString(value), false
	}
	return "0x" + hex.EncodeToString(value[:end]) + truncationMarker, true
}

func collectRows(rows *sql.Rows, limit int) ([]string, [][]interface{}, bool, bool, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, false, false, err
	}
	if columns == nil {
		columns = make([]string, 0)
	}
	if len(columns) > maxResultColumns {
		return nil, nil, false, false, errResultTooLarge
	}
	// Account for the escaped JSON for each name and cell, array delimiters,
	// commas, and a conservative allowance for the enclosing object.
	used := resultEnvelopeBytes
	for _, column := range columns {
		if len(column) > maxResultJSONBytes-used {
			return nil, nil, false, false, errResultTooLarge
		}
		encoded, err := json.Marshal(column)
		if err != nil || len(encoded)+1 > maxResultJSONBytes-used {
			return nil, nil, false, false, errResultTooLarge
		}
		used += len(encoded) + 1
	}
	result := make([][]interface{}, 0)
	more, cellsTruncated := false, false
	for rows.Next() {
		if len(result) == limit {
			more = true
			break
		}
		raw := make([]interface{}, len(columns))
		dest := make([]interface{}, len(columns))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, false, false, err
		}
		if 3 > maxResultJSONBytes-used {
			return nil, nil, false, false, errResultTooLarge
		}
		displayed := make([]interface{}, len(raw))
		used += 3 // row brackets and a possible comma
		for i, value := range raw {
			var truncated bool
			displayed[i], truncated = displayValue(value)
			cellsTruncated = cellsTruncated || truncated
			encoded, err := json.Marshal(displayed[i])
			if err != nil || len(encoded)+1 > maxResultJSONBytes-used {
				return nil, nil, false, false, errResultTooLarge
			}
			used += len(encoded) + 1
		}
		result = append(result, displayed)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, false, err
	}
	return columns, result, more, cellsTruncated, nil
}

type rowsResult struct {
	Columns        []string        `json:"columns"`
	Rows           [][]interface{} `json:"rows"`
	Page           int64           `json:"page"`
	PageSize       int64           `json:"page_size"`
	HasNext        bool            `json:"has_next"`
	CellsTruncated bool            `json:"cells_truncated,omitempty"`
}

func browseRows(ctx context.Context, db *sql.DB, database, table string, page, size, offset int64) (result rowsResult, resultErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		return rowsResult{}, err
	}
	defer closeRequestConn(conn)
	defer func() { resultErr = budget.resultError(resultErr) }()
	query := "SELECT * FROM " + quoteIdentifier(database) + "." + quoteIdentifier(table) + " LIMIT ? OFFSET ?"
	rows, err := conn.QueryContext(ctx, query, size+1, offset)
	if err != nil {
		return rowsResult{}, err
	}
	defer rows.Close()
	columns, values, more, cellsTruncated, err := collectRows(rows, int(size))
	if err != nil {
		budget.abort()
		cancel()
		return rowsResult{}, err
	}
	if more {
		// The lookahead proves there is another page. Never drain it on Close.
		budget.abort()
		cancel()
		_ = rows.Close()
	} else {
		if err := requireSingleResultSet(rows); err != nil {
			budget.abort()
			cancel()
			return rowsResult{}, err
		}
		if err := rows.Close(); err != nil {
			return rowsResult{}, err
		}
	}
	return rowsResult{Columns: columns, Rows: values, Page: page, PageSize: size, HasNext: more, CellsTruncated: cellsTruncated}, nil
}

type queryResult struct {
	Columns        []string        `json:"columns"`
	Rows           [][]interface{} `json:"rows"`
	AffectedRows   *int64          `json:"affected_rows"`
	Truncated      bool            `json:"truncated"`
	CellsTruncated bool            `json:"cells_truncated,omitempty"`
	DurationMS     int64           `json:"duration_ms"`
}

func executeQuery(ctx context.Context, db *sql.DB, database, statement string) (result queryResult, resultErr error) {
	start := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		return queryResult{}, err
	}
	defer closeRequestConn(conn)
	defer func() { resultErr = budget.resultError(resultErr) }()
	if database != "" {
		if _, err := conn.ExecContext(ctx, "USE "+quoteIdentifier(database)); err != nil {
			return queryResult{}, err
		}
	}
	// All statements take exactly the same path. Result metadata, not a SQL
	// keyword heuristic, determines how the already-confirmed result displays.
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		return queryResult{}, err
	}
	columns, err := rows.Columns()
	if err != nil {
		budget.abort()
		cancel()
		_ = rows.Close()
		return queryResult{}, err
	}
	result = queryResult{Columns: make([]string, 0), Rows: make([][]interface{}, 0)}
	if len(columns) == 0 {
		if err := rows.Close(); err != nil {
			return queryResult{}, err
		}
		var affected int64
		if err := conn.QueryRowContext(ctx, "SELECT ROW_COUNT()").Scan(&affected); err != nil {
			return queryResult{}, err
		}
		result.AffectedRows = &affected
	} else {
		var more bool
		result.Columns, result.Rows, more, result.CellsTruncated, err = collectRows(rows, 500)
		if err != nil {
			budget.abort()
			cancel()
			_ = rows.Close()
			return queryResult{}, err
		}
		result.Truncated = more
		if more {
			// Close the physical socket first: cancellation's driver watcher
			// can otherwise race Rows.Close and let it drain the stream.
			budget.abort()
			cancel()
			_ = rows.Close()
		} else {
			if err := requireSingleResultSet(rows); err != nil {
				budget.abort()
				cancel()
				_ = rows.Close()
				return queryResult{}, err
			}
			if err := rows.Close(); err != nil {
				return queryResult{}, err
			}
		}
	}
	result.DurationMS = time.Since(start).Milliseconds()
	return result, nil
}
