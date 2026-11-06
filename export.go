package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Export downloads through single-use, short-lived tokens.
// Implemented per docs/plans/v2-features.md section B.

const (
	exportRowCap        = 50000
	exportByteCap       = 32 << 20
	exportInsertBatch   = 100
	exportMaxTables     = 500
	exportFlushBytes    = 64 << 10
	exportWireLimit     = 64 << 20
	exportCSVTruncation = "-- TRUNCATED\n"
	exportSQLTruncation = "-- TRUNCATED BY DBKID\n"
)

type exportTokenRequest struct {
	Database  string `json:"database"`
	Table     string `json:"table"`
	Format    string `json:"format"`
	What      string `json:"what"`
	Sort      string `json:"sort"`
	Dir       string `json:"dir"`
	Filter    string `json:"filter"`
	Confirmed bool   `json:"confirmed,omitempty"` // tolerated, ignored: read-only
}

func handleExportToken(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request exportTokenRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !validIdentifier(request.Database) || (request.Table != "" && !validIdentifier(request.Table)) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	if request.Format != "csv" && request.Format != "sql" {
		writeError(w, http.StatusBadRequest, "unsupported export format")
		return
	}
	if request.What != "data" && request.What != "structure" && request.What != "both" {
		writeError(w, http.StatusBadRequest, "unsupported export selection")
		return
	}
	if request.Format != "csv" || request.Table == "" {
		// Sorting and filtering only apply to a CSV table export.
		if request.Sort != "" || request.Dir != "" || request.Filter != "" {
			writeError(w, http.StatusBadRequest, "invalid sort or filter")
			return
		}
	}
	if request.Format == "csv" && request.Table == "" {
		writeError(w, http.StatusBadRequest, "a table is required for CSV export")
		return
	}
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	if request.Format == "csv" && (request.Sort != "" || request.Dir != "" || request.Filter != "") {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		conn, budget, err := requestConn(ctx, db)
		if err != nil {
			cancel()
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		meta, err := tableColumnMeta(ctx, conn, request.Database, request.Table)
		cancel()
		closeRequestConn(conn)
		if err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
			return
		}
		filters, err := parseBrowseFilters(request.Filter)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid sort or filter")
			return
		}
		if _, _, _, err := buildBrowseExtras(filters, columnMetaSet(meta), request.Sort, request.Dir); err != nil {
			writeError(w, http.StatusBadRequest, "invalid sort or filter")
			return
		}
	}
	token, err := current.issueExportToken(exportRequest{
		Database: request.Database, Table: request.Table, Format: request.Format, What: request.What,
		Sort: request.Sort, Dir: request.Dir, Filter: request.Filter,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unable to create download token")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		URL string `json:"url"`
	}{"/api/export?token=" + token})
}

func handleExport(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	token := r.URL.Query().Get("token")
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	request, ok := current.consumeExportToken(token)
	if !ok {
		writeError(w, http.StatusForbidden, "invalid or expired download token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ctx = exportContext(ctx)
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	var out bytes.Buffer
	var truncated bool
	if request.Format == "csv" {
		truncated, err = exportCSV(ctx, conn, budget, cancel, request, &out)
	} else {
		truncated, err = exportSQL(ctx, conn, budget, cancel, request, &out)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	filename, contentType := exportResponseNames(request)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(filename)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(out.Len()))
	if truncated {
		w.Header().Set("X-DBKid-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for start := 0; start < out.Len(); start += exportFlushBytes {
		end := start + exportFlushBytes
		if end > out.Len() {
			end = out.Len()
		}
		_, _ = w.Write(out.Bytes()[start:end])
		if flusher != nil {
			flusher.Flush()
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
}

func exportResponseNames(request exportRequest) (string, string) {
	if request.Format == "csv" {
		return request.Table + ".csv", "text/csv; charset=utf-8"
	}
	name := request.Table
	if name == "" {
		name = request.Database
	}
	return name + ".sql", "application/sql; charset=utf-8"
}

// exportContext applies the streaming export wire budget (64 MiB) so
// dialBoundedMySQL can allow more inbound bytes than the default cap.
func exportContext(parent context.Context) context.Context {
	return context.WithValue(parent, wireLimitKey{}, int64(exportWireLimit))
}

// abortableRows closes a rows stream that must not be drained: the physical
// socket is aborted and the context cancelled so Close cannot pull the rest.
func abortableRows(budget *connectionBudget, cancel context.CancelFunc, rows *sql.Rows) {
	budget.abort()
	cancel()
	_ = rows.Close()
}

func exportCSV(ctx context.Context, conn *sql.Conn, budget *connectionBudget, cancel context.CancelFunc,
	request exportRequest, out *bytes.Buffer) (truncated bool, resultErr error) {
	defer func() {
		if truncated {
			out.WriteString(exportCSVTruncation)
		}
	}()
	meta, err := tableColumnMeta(ctx, conn, request.Database, request.Table)
	if err != nil {
		return false, err
	}
	filters, err := parseBrowseFilters(request.Filter)
	if err != nil {
		return false, errInvalidBrowse
	}
	whereSQL, orderSQL, filterArgs, err := buildBrowseExtras(filters, columnMetaSet(meta), request.Sort, request.Dir)
	if err != nil {
		return false, err
	}
	query := "SELECT * FROM " + quoteIdentifier(request.Database) + "." + quoteIdentifier(request.Table) + whereSQL + orderSQL
	rows, err := conn.QueryContext(ctx, query, filterArgs...)
	if err != nil {
		return false, err
	}
	defer func() {
		if resultErr != nil || truncated {
			abortableRows(budget, cancel, rows)
		}
	}()
	names, err := rows.Columns()
	if err != nil {
		return false, err
	}
	dataTypes := make([]string, len(names))
	metaByName := columnMetaSet(meta)
	for i, name := range names {
		dataTypes[i] = metaByName[name]
	}
	header := make([]string, len(names))
	for i, name := range names {
		header[i] = csvQuoteField(name)
	}
	if err := appendBounded(out, strings.Join(header, ",")+"\n"); err != nil {
		return false, err
	}
	raw := make([]interface{}, len(names))
	dest := make([]interface{}, len(names))
	for i := range raw {
		dest[i] = &raw[i]
	}
	rowCount := 0
	for rows.Next() {
		if rowCount >= exportRowCap || out.Len() >= exportByteCap {
			truncated = true
			return true, nil
		}
		if err := rows.Scan(dest...); err != nil {
			return false, err
		}
		fields := make([]string, len(raw))
		for i, value := range raw {
			fields[i] = csvCell(dataTypes[i], value)
		}
		line := strings.Join(fields, ",") + "\n"
		if out.Len()+len(line) > exportByteCap {
			truncated = true
			return true, nil
		}
		if _, err := out.WriteString(line); err != nil {
			return false, err
		}
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if err := requireSingleResultSet(rows); err != nil {
		return false, err
	}
	return false, rows.Close()
}

func exportSQL(ctx context.Context, conn *sql.Conn, budget *connectionBudget, cancel context.CancelFunc,
	request exportRequest, out *bytes.Buffer) (truncated bool, resultErr error) {
	out.WriteString("-- DBKid export " + request.Database + " " + time.Now().UTC().Format(time.RFC3339) + "\n")
	out.WriteString("SET NAMES utf8mb4;\n")
	tables := []string{request.Table}
	if request.Table == "" {
		tables, resultErr = listExportTables(ctx, conn, request.Database)
		if resultErr != nil {
			return false, resultErr
		}
	}
	defer func() {
		if truncated {
			out.WriteString(exportSQLTruncation)
		}
	}()
	for _, table := range tables {
		if truncated || out.Len() >= exportByteCap {
			truncated = true
			break
		}
		tableTruncated, err := exportSQLTable(ctx, conn, budget, cancel, request, table, out)
		if err != nil {
			return truncated, err
		}
		truncated = tableTruncated
	}
	return truncated, nil
}

func exportSQLTable(ctx context.Context, conn *sql.Conn, budget *connectionBudget, cancel context.CancelFunc,
	request exportRequest, table string, out *bytes.Buffer) (truncated bool, resultErr error) {
	meta, err := tableColumnMeta(ctx, conn, request.Database, table)
	if err != nil {
		return false, err
	}
	if request.What == "both" {
		// Never an active DROP: the marker is a comment for the importer.
		out.WriteString("-- DROP TABLE IF EXISTS " + quoteIdentifier(table) + ";\n")
	}
	if request.What == "structure" || request.What == "both" {
		if err := exportSQLStructure(ctx, conn, budget, cancel, request.Database, table, out); err != nil {
			return false, err
		}
	}
	if request.What == "data" || request.What == "both" {
		return exportSQLData(ctx, conn, budget, cancel, request, table, meta, out)
	}
	return false, nil
}

func exportSQLStructure(ctx context.Context, conn *sql.Conn, budget *connectionBudget, cancel context.CancelFunc,
	database, table string, out *bytes.Buffer) error {
	statement := "SHOW CREATE TABLE " + quoteIdentifier(database) + "." + quoteIdentifier(table)
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		return err
	}
	names, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return err
	}
	// SHOW CREATE TABLE yields exactly one row and one result set, so closing
	// normally drains nothing; the connection must stay usable for the data
	// phase of a "both" export.
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		return sql.ErrNoRows
	}
	raw := make([]interface{}, len(names))
	dest := make([]interface{}, len(names))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		_ = rows.Close()
		return err
	}
	if err := requireSingleResultSet(rows); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	definition, _, err := stringValue(raw[len(raw)-1])
	if err != nil {
		return err
	}
	out.WriteString(definition)
	out.WriteString(";\n")
	return nil
}

func exportSQLData(ctx context.Context, conn *sql.Conn, budget *connectionBudget, cancel context.CancelFunc,
	request exportRequest, table string, meta []columnMeta, out *bytes.Buffer) (truncated bool, resultErr error) {
	query := "SELECT * FROM " + quoteIdentifier(request.Database) + "." + quoteIdentifier(table)
	pk := tablePrimaryKeys(meta)
	if len(pk) > 0 {
		orderBy := make([]string, 0, len(pk))
		for _, column := range pk {
			orderBy = append(orderBy, quoteIdentifier(column.Name))
		}
		query += " ORDER BY " + strings.Join(orderBy, ",")
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return false, err
	}
	defer func() {
		if resultErr != nil || truncated {
			abortableRows(budget, cancel, rows)
		}
	}()
	names, err := rows.Columns()
	if err != nil {
		return false, err
	}
	metaByName := columnMetaSet(meta)
	dataTypes := make([]string, len(names))
	for i, name := range names {
		dataTypes[i] = metaByName[name]
	}
	raw := make([]interface{}, len(names))
	dest := make([]interface{}, len(names))
	for i := range raw {
		dest[i] = &raw[i]
	}
	batch := make([]string, 0, exportInsertBatch)
	pending := 0
	rowCount := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		out.WriteString("INSERT INTO " + quoteIdentifier(table) + " VALUES " + strings.Join(batch, ",") + ";\n")
		batch = batch[:0]
		pending = 0
	}
	for rows.Next() {
		if rowCount >= exportRowCap || out.Len() >= exportByteCap {
			flush()
			truncated = true
			return true, nil
		}
		if err := rows.Scan(dest...); err != nil {
			return false, err
		}
		tuple := make([]string, len(raw))
		for i, value := range raw {
			tuple[i] = sqlValueLiteral(dataTypes[i], value)
		}
		row := "(" + strings.Join(tuple, ",") + ")"
		if out.Len()+pending+len(row) > exportByteCap {
			flush()
			if out.Len()+len(row) > exportByteCap {
				truncated = true
				return true, nil
			}
		}
		batch = append(batch, row)
		pending += len(row) + 2
		rowCount++
		if len(batch) == exportInsertBatch {
			flush()
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if err := requireSingleResultSet(rows); err != nil {
		return false, err
	}
	flush()
	return false, rows.Close()
}

func listExportTables(ctx context.Context, conn *sql.Conn, database string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME`, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if len(name) > maxCellDisplayBytes {
			return nil, errResultTooLarge
		}
		tables = append(tables, name)
		if len(tables) == exportMaxTables {
			break
		}
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, err
	}
	return tables, nil
}

func appendBounded(out *bytes.Buffer, text string) error {
	_, err := out.WriteString(text)
	return err
}

// csvCell renders one CSV field. NULL is an unquoted empty field, binary
// values are 0x-prefixed hex, every other value is RFC-4180 quoted.
func csvCell(dataType string, value interface{}) string {
	if value == nil {
		return ""
	}
	if isBinaryDataType(dataType) {
		return "0x" + hex.EncodeToString(asBytes(value))
	}
	return csvQuoteField(rawExportString(value))
}

// csvQuoteField quotes a CSV field and doubles embedded quotes.
func csvQuoteField(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(value); i++ {
		if value[i] == '"' {
			b.WriteString(`""`)
			continue
		}
		b.WriteByte(value[i])
	}
	b.WriteByte('"')
	return b.String()
}

// sqlValueLiteral renders one SQL dump literal. NULL stays NULL, numeric
// columns stay raw when the value is a valid number, binary values are
// unquoted 0x hex, datetime-family and string values are quoted with
// backslash and single-quote escaping.
func sqlValueLiteral(dataType string, value interface{}) string {
	if value == nil {
		return "NULL"
	}
	if isBinaryDataType(dataType) {
		return "0x" + hex.EncodeToString(asBytes(value))
	}
	text := rawExportString(value)
	if isNumericDataType(dataType) && validNumberLiteral(text) {
		return text
	}
	return sqlQuoteString(text)
}

func sqlQuoteString(value string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		default:
			b.WriteByte(value[i])
		}
	}
	b.WriteByte('\'')
	return b.String()
}

func isNumericDataType(dataType string) bool {
	switch dataType {
	case "tinyint", "smallint", "mediumint", "int", "bigint", "decimal", "float", "double":
		return true
	}
	return false
}

// validNumberLiteral reports whether the text is a plain SQL-safe number.
func validNumberLiteral(text string) bool {
	i := 0
	if i < len(text) && (text[i] == '+' || text[i] == '-') {
		i++
	}
	digits, dot := 0, false
	for ; i < len(text); i++ {
		c := text[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot:
			dot = true
		case c == 'e' || c == 'E':
			if digits == 0 {
				return false
			}
			rest := text[i+1:]
			if rest == "" {
				return false
			}
			if rest[0] == '+' || rest[0] == '-' {
				rest = rest[1:]
			}
			if rest == "" {
				return false
			}
			for j := 0; j < len(rest); j++ {
				if rest[j] < '0' || rest[j] > '9' {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	return digits > 0
}

func asBytes(value interface{}) []byte {
	switch value := value.(type) {
	case []byte:
		return value
	case string:
		return []byte(value)
	default:
		return []byte(fmt.Sprint(value))
	}
}

func rawExportString(value interface{}) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	case []byte:
		return string(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case uint64:
		return strconv.FormatUint(value, 10)
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64)
	case time.Time:
		return value.Format("2006-01-02 15:04:05.999999")
	default:
		return fmt.Sprint(value)
	}
}
