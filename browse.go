package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// v2 read handlers. Implemented per docs/plans/v2-features.md section B.

var errInvalidBrowse = errors.New("invalid sort or filter")

const maxBrowseFilters = 3

var browseFilterOps = map[string]string{
	"=":           "=",
	"!=":          "!=",
	"<":           "<",
	"<=":          "<=",
	">":           ">",
	">=":          ">=",
	"like":        "LIKE",
	"not like":    "NOT LIKE",
	"is null":     "IS NULL",
	"is not null": "IS NOT NULL",
}

type browseFilterSpec struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Value  string `json:"value"`
}

// parseBrowseFilters decodes the raw `filter` query parameter. It must be a
// JSON array of 1..3 {column, op, value} objects; anything else is invalid.
func parseBrowseFilters(filterJSON string) ([]browseFilterSpec, error) {
	if filterJSON == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(filterJSON))
	decoder.DisallowUnknownFields()
	var filters []browseFilterSpec
	if err := decoder.Decode(&filters); err != nil {
		return nil, errInvalidBrowse
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errInvalidBrowse
	}
	if len(filters) == 0 || len(filters) > maxBrowseFilters {
		return nil, errInvalidBrowse
	}
	for _, filter := range filters {
		if filter.Op == "" {
			return nil, errInvalidBrowse
		}
	}
	return filters, nil
}

// columnMeta is INFORMATION_SCHEMA.COLUMNS metadata for one column.
type columnMeta struct {
	Name     string
	DataType string // DATA_TYPE, lowercase
	Type     string // COLUMN_TYPE
	Nullable bool
	Key      string // COLUMN_KEY (PRI/UNI/MUL/empty)
	Default  *string
	Extra    string
}

func isBinaryDataType(dataType string) bool {
	switch dataType {
	case "binary", "varbinary", "blob", "tinyblob", "mediumblob", "longblob":
		return true
	}
	return false
}

func isStringDataType(dataType string) bool {
	switch dataType {
	case "char", "varchar", "text", "tinytext", "mediumtext", "longtext", "enum", "set", "json":
		return true
	}
	return false
}

// tableColumnMeta reads one table's column metadata on the given connection.
// Cells stay under the same display caps as listColumns.
func tableColumnMeta(ctx context.Context, conn *sql.Conn, database, table string) (result []columnMeta, resultErr error) {
	rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, COLUMN_KEY, COLUMN_DEFAULT, EXTRA
		FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, database, table)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			return
		}
		if err := rows.Close(); resultErr == nil && err != nil {
			result, resultErr = nil, err
		}
	}()
	result = make([]columnMeta, 0)
	for rows.Next() {
		var column columnMeta
		var nullable string
		var defaultValue sql.NullString
		if err := rows.Scan(&column.Name, &column.DataType, &column.Type, &nullable, &column.Key, &defaultValue, &column.Extra); err != nil {
			return nil, err
		}
		if len(column.Name) > maxCellDisplayBytes || len(column.DataType) > maxCellDisplayBytes || len(column.Type) > maxCellDisplayBytes ||
			len(nullable) > maxCellDisplayBytes || len(column.Key) > maxCellDisplayBytes ||
			len(defaultValue.String) > maxCellDisplayBytes || len(column.Extra) > maxCellDisplayBytes {
			return nil, errResultTooLarge
		}
		column.Nullable = strings.EqualFold(nullable, "YES")
		if defaultValue.Valid {
			column.Default = &defaultValue.String
		}
		result = append(result, column)
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, err
	}
	return result, nil
}

func columnMetaSet(meta []columnMeta) map[string]string {
	set := make(map[string]string, len(meta))
	for _, column := range meta {
		set[column.Name] = column.DataType
	}
	return set
}

type primaryKeyColumn struct {
	Name     string
	DataType string
}

func tablePrimaryKeys(meta []columnMeta) []primaryKeyColumn {
	keys := make([]primaryKeyColumn, 0, 2)
	for _, column := range meta {
		if column.Key == "PRI" {
			keys = append(keys, primaryKeyColumn{Name: column.Name, DataType: column.DataType})
		}
	}
	return keys
}

// matchPrimaryKey validates a [[column, value], ...] key against the table's
// full primary key and renders the WHERE fragment plus bound arguments.
// Binary key parts use the same hex/UNHEX convention as row mutations.
func matchPrimaryKey(pk []primaryKeyColumn, key [][2]string) (string, []interface{}, bool, string) {
	if len(pk) == 0 || len(key) != len(pk) {
		return "", nil, false, "a primary key is required"
	}
	byName := make(map[string]string, len(key))
	for _, pair := range key {
		if _, exists := byName[pair[0]]; exists {
			return "", nil, false, "a primary key is required"
		}
		byName[pair[0]] = pair[1]
	}
	fragments := make([]string, 0, len(pk))
	args := make([]interface{}, 0, len(pk))
	for _, column := range pk {
		value, ok := byName[column.Name]
		if !ok {
			return "", nil, false, "a primary key is required"
		}
		if isBinaryDataType(column.DataType) {
			if !validHexValue(value) {
				return "", nil, false, "invalid hex value"
			}
			fragments = append(fragments, quoteIdentifier(column.Name)+" = UNHEX(?)")
			args = append(args, value)
			continue
		}
		fragments = append(fragments, quoteIdentifier(column.Name)+" = ?")
		args = append(args, value)
	}
	return "(" + strings.Join(fragments, " AND ") + ")", args, true, ""
}

func validHexValue(value string) bool {
	if len(value) == 0 || len(value)%2 != 0 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func parseKeyParam(raw string) ([][2]string, bool) {
	if raw == "" {
		return nil, false
	}
	var key [][2]string
	if err := json.Unmarshal([]byte(raw), &key); err != nil {
		return nil, false
	}
	if len(key) == 0 {
		return nil, false
	}
	for _, pair := range key {
		if pair[0] == "" {
			return nil, false
		}
	}
	return key, true
}

// buildBrowseExtras validates sort and filter parameters against the table's
// real columns and renders the WHERE and ORDER BY fragments plus arguments.
func buildBrowseExtras(filters []browseFilterSpec, columns map[string]string, sortColumn, sortDir string) (string, string, []interface{}, error) {
	if (sortColumn == "") != (sortDir == "") {
		return "", "", nil, errInvalidBrowse
	}
	orderSQL := ""
	if sortColumn != "" {
		if sortDir != "asc" && sortDir != "desc" {
			return "", "", nil, errInvalidBrowse
		}
		if _, ok := columns[sortColumn]; !ok {
			return "", "", nil, errInvalidBrowse
		}
		orderSQL = " ORDER BY " + quoteIdentifier(sortColumn) + " " + strings.ToUpper(sortDir)
	}
	var args []interface{}
	whereSQL := ""
	if filters != nil {
		fragments := make([]string, 0, len(filters))
		for _, filter := range filters {
			if _, ok := columns[filter.Column]; !ok {
				return "", "", nil, errInvalidBrowse
			}
			sqlOp, ok := browseFilterOps[filter.Op]
			if !ok {
				return "", "", nil, errInvalidBrowse
			}
			switch sqlOp {
			case "IS NULL", "IS NOT NULL":
				fragments = append(fragments, quoteIdentifier(filter.Column)+" "+sqlOp)
			case "LIKE", "NOT LIKE":
				fragments = append(fragments, quoteIdentifier(filter.Column)+" "+sqlOp+" ?")
				args = append(args, "%"+filter.Value+"%")
			default:
				fragments = append(fragments, quoteIdentifier(filter.Column)+" "+sqlOp+" ?")
				args = append(args, filter.Value)
			}
		}
		whereSQL = " WHERE (" + strings.Join(fragments, " AND ") + ")"
	}
	return whereSQL, orderSQL, args, nil
}

// browseRowsV2 extends browseRows with optional sorting and filtering.
// sortColumn/sortDir/filterJSON are raw query-string values; validate them
// against the table's real columns before building SQL. Return errInvalidBrowse
// for anything invalid; the caller maps it to HTTP 400.
func browseRowsV2(ctx context.Context, db *sql.DB, database, table string, page, size, offset int64,
	sortColumn, sortDir, filterJSON string) (result rowsResult, resultErr error) {
	filters, err := parseBrowseFilters(filterJSON)
	if err != nil {
		return rowsResult{}, errInvalidBrowse
	}
	if sortColumn == "" && sortDir == "" && filters == nil {
		return browseRows(ctx, db, database, table, page, size, offset)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		return rowsResult{}, err
	}
	defer closeRequestConn(conn)
	defer func() { resultErr = budget.resultError(resultErr) }()
	meta, err := tableColumnMeta(ctx, conn, database, table)
	if err != nil {
		return rowsResult{}, err
	}
	whereSQL, orderSQL, filterArgs, err := buildBrowseExtras(filters, columnMetaSet(meta), sortColumn, sortDir)
	if err != nil {
		return rowsResult{}, err
	}
	query := "SELECT * FROM " + quoteIdentifier(database) + "." + quoteIdentifier(table) + whereSQL + orderSQL + " LIMIT ? OFFSET ?"
	args := append(make([]interface{}, 0, len(filterArgs)+2), filterArgs...)
	args = append(args, size+1, offset)
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return rowsResult{}, err
	}
	columns, values, more, cellsTruncated, err := collectRows(rows, int(size))
	if err != nil {
		budget.abort()
		cancel()
		_ = rows.Close()
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
			_ = rows.Close()
			return rowsResult{}, err
		}
		if err := rows.Close(); err != nil {
			return rowsResult{}, err
		}
	}
	return rowsResult{Columns: columns, Rows: values, Page: page, PageSize: size, HasNext: more, CellsTruncated: cellsTruncated}, nil
}

func sessionDatabase(current *session) (*sql.DB, bool) {
	current.mu.RLock()
	db := current.db
	current.mu.RUnlock()
	return db, db != nil
}

func handleRowFetch(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	values := r.URL.Query()
	database, table, keyJSON := values.Get("database"), values.Get("table"), values.Get("key")
	if !validIdentifier(database) || !validIdentifier(table) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	key, ok := parseKeyParam(keyJSON)
	if !ok {
		writeError(w, http.StatusBadRequest, "a primary key is required")
		return
	}
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	meta, err := tableColumnMeta(ctx, conn, database, table)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	whereSQL, args, ok, message := matchPrimaryKey(tablePrimaryKeys(meta), key)
	if !ok {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	query := "SELECT * FROM " + quoteIdentifier(database) + "." + quoteIdentifier(table) + " WHERE " + whereSQL + " LIMIT 1"
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		budget.abort()
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			budget.abort()
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		budget.abort() // a full primary key cannot repeat; never drain past it
		writeError(w, http.StatusNotFound, "row not found")
		return
	}
	raw := make([]interface{}, len(names))
	dest := make([]interface{}, len(names))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		budget.abort()
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	budget.abort() // never read past the single fetched row
	displayed := make([]interface{}, len(raw))
	truncated := make([]bool, len(raw))
	for i, value := range raw {
		displayed[i], truncated[i] = displayValue(value)
	}
	metaByName := make(map[string]columnMeta, len(meta))
	for _, column := range meta {
		metaByName[column.Name] = column
	}
	infos := make([]columnInfo, 0, len(names))
	binary := make([]bool, 0, len(names))
	for _, name := range names {
		column := metaByName[name]
		infos = append(infos, columnInfo{Name: name, Type: column.Type, Nullable: column.Nullable, Key: column.Key, Default: column.Default, Extra: column.Extra})
		binary = append(binary, isBinaryDataType(column.DataType))
	}
	writeJSON(w, http.StatusOK, struct {
		Columns   []columnInfo  `json:"columns"`
		Values    []interface{} `json:"values"`
		Binary    []bool        `json:"binary"`
		Truncated []bool        `json:"truncated"`
	}{infos, displayed, binary, truncated})
}

type indexInfo struct {
	Name    string   `json:"name"`
	Unique  bool     `json:"unique"`
	Columns []string `json:"columns"`
	Type    string   `json:"type"`
}

func handleIndexes(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	values := r.URL.Query()
	database, table := values.Get("database"), values.Get("table")
	if !validIdentifier(database) || !validIdentifier(table) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	indexes, err := listIndexes(ctx, conn, database, table)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Indexes []indexInfo `json:"indexes"`
	}{indexes})
}

func listIndexes(ctx context.Context, conn *sql.Conn, database, table string) (result []indexInfo, resultErr error) {
	rows, err := conn.QueryContext(ctx, `SELECT INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME, INDEX_TYPE
		FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY INDEX_NAME, SEQ_IN_INDEX`, database, table)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			return
		}
		if err := rows.Close(); resultErr == nil && err != nil {
			result, resultErr = nil, err
		}
	}()
	used := resultEnvelopeBytes
	result = make([]indexInfo, 0)
	byName := make(map[string]int)
	for rows.Next() {
		var name string
		var nonUnique int64
		var seq int64
		var column, indexType string
		if err := rows.Scan(&name, &nonUnique, &seq, &column, &indexType); err != nil {
			return nil, err
		}
		if len(name) > maxCellDisplayBytes || len(column) > maxCellDisplayBytes || len(indexType) > maxCellDisplayBytes {
			return nil, errResultTooLarge
		}
		position, exists := byName[name]
		if !exists {
			index := indexInfo{Name: name, Unique: nonUnique == 0, Columns: []string{column}, Type: indexType}
			if err := accountJSONValue(&used, index); err != nil {
				return nil, err
			}
			byName[name] = len(result)
			result = append(result, index)
			continue
		}
		result[position].Columns = append(result[position].Columns, column)
		if err := accountJSONValue(&used, column); err != nil {
			return nil, err
		}
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, err
	}
	return result, nil
}

type routineInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Created     string `json:"created"`
	LastAltered string `json:"last_altered"`
}

type triggerInfo struct {
	Name   string `json:"name"`
	Event  string `json:"event"`
	Table  string `json:"table"`
	Timing string `json:"timing"`
}

type eventInfo struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	Type          string `json:"type"`
	IntervalValue string `json:"interval_value"`
	IntervalField string `json:"interval_field"`
}

func handleRoutines(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	database := r.URL.Query().Get("database")
	if !validIdentifier(database) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	rowsData, err := listRowsAsStrings(ctx, conn,
		`SELECT ROUTINE_NAME, ROUTINE_TYPE, CREATED, LAST_ALTERED FROM INFORMATION_SCHEMA.ROUTINES
		WHERE ROUTINE_SCHEMA = ? ORDER BY ROUTINE_NAME`, database)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	used := resultEnvelopeBytes
	response := make([]routineInfo, 0, len(rowsData))
	for _, row := range rowsData {
		entry := routineInfo{Name: row[0]}
		if len(row) > 1 {
			entry.Type = row[1]
		}
		if len(row) > 2 {
			entry.Created = row[2]
		}
		if len(row) > 3 {
			entry.LastAltered = row[3]
		}
		if err := accountJSONValue(&used, entry); err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		response = append(response, entry)
	}
	writeJSON(w, http.StatusOK, struct {
		Routines []routineInfo `json:"routines"`
	}{response})
}

func handleTriggers(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	database := r.URL.Query().Get("database")
	if !validIdentifier(database) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	rowsData, err := listRowsAsStrings(ctx, conn,
		`SELECT TRIGGER_NAME, EVENT_MANIPULATION, EVENT_OBJECT_TABLE, ACTION_TIMING FROM INFORMATION_SCHEMA.TRIGGERS
		WHERE TRIGGER_SCHEMA = ? ORDER BY TRIGGER_NAME`, database)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	used := resultEnvelopeBytes
	response := make([]triggerInfo, 0, len(rowsData))
	for _, row := range rowsData {
		entry := triggerInfo{Name: row[0]}
		if len(row) > 1 {
			entry.Event = row[1]
		}
		if len(row) > 2 {
			entry.Table = row[2]
		}
		if len(row) > 3 {
			entry.Timing = row[3]
		}
		if err := accountJSONValue(&used, entry); err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		response = append(response, entry)
	}
	writeJSON(w, http.StatusOK, struct {
		Triggers []triggerInfo `json:"triggers"`
	}{response})
}

func handleEvents(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	database := r.URL.Query().Get("database")
	if !validIdentifier(database) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	rowsData, err := listRowsAsStrings(ctx, conn,
		`SELECT EVENT_NAME, STATUS, EVENT_TYPE, INTERVAL_VALUE, INTERVAL_FIELD FROM INFORMATION_SCHEMA.EVENTS
		WHERE EVENT_SCHEMA = ? ORDER BY EVENT_NAME`, database)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	used := resultEnvelopeBytes
	response := make([]eventInfo, 0, len(rowsData))
	for _, row := range rowsData {
		entry := eventInfo{Name: row[0]}
		if len(row) > 1 {
			entry.Status = row[1]
		}
		if len(row) > 2 {
			entry.Type = row[2]
		}
		if len(row) > 3 {
			entry.IntervalValue = row[3]
		}
		if len(row) > 4 {
			entry.IntervalField = row[4]
		}
		if err := accountJSONValue(&used, entry); err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		response = append(response, entry)
	}
	writeJSON(w, http.StatusOK, struct {
		Events []eventInfo `json:"events"`
	}{response})
}

func handleRoutineShow(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	values := r.URL.Query()
	database, name, kind := values.Get("database"), values.Get("name"), values.Get("kind")
	if !validIdentifier(database) || !validIdentifier(name) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	if kind != "procedure" && kind != "function" {
		writeError(w, http.StatusBadRequest, "invalid routine kind")
		return
	}
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	statement := "SHOW CREATE " + strings.ToUpper(kind[:1]) + kind[1:] + " " + quoteIdentifier(database) + "." + quoteIdentifier(name)
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		budget.abort()
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	if len(names) < 3 {
		budget.abort()
		writeError(w, http.StatusBadGateway, databaseErrorMessage(errors.New("unexpected routine definition result")))
		return
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			budget.abort()
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		budget.abort()
		writeError(w, http.StatusNotFound, "routine not found")
		return
	}
	raw := make([]interface{}, len(names))
	dest := make([]interface{}, len(names))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		budget.abort()
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	budget.abort()
	definition := ""
	if raw[2] != nil {
		text, _, err := stringValue(raw[2])
		if err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		definition = text
	}
	truncated := false
	if len(definition) > maxCellDisplayBytes {
		end := maxCellDisplayBytes
		for end > 0 && !utf8.RuneStart(definition[end]) {
			end--
		}
		definition = definition[:end] + truncationMarker
		truncated = true
	}
	writeJSON(w, http.StatusOK, struct {
		Name       string `json:"name"`
		Definition string `json:"definition"`
		Truncated  bool   `json:"truncated,omitempty"`
	}{name, definition, truncated})
}

// listRowsAsStrings runs a bounded metadata query and returns raw cell strings.
// Every cell stays under the display cap; callers apply their JSON budget.
func listRowsAsStrings(ctx context.Context, conn *sql.Conn, query string, args ...interface{}) ([][]string, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("unexpected empty metadata result")
	}
	result := make([][]string, 0)
	for rows.Next() {
		raw := make([]interface{}, len(names))
		dest := make([]interface{}, len(names))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make([]string, len(raw))
		for i, value := range raw {
			if value == nil {
				continue
			}
			text, _, err := stringValue(value)
			if err != nil {
				return nil, err
			}
			if len(text) > maxCellDisplayBytes {
				return nil, errResultTooLarge
			}
			row[i] = text
		}
		result = append(result, row)
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, err
	}
	return result, nil
}

// stringValue renders a raw database cell as display text. Binary bytes keep
// the 0x/hex representation used everywhere in DBKid.
func stringValue(value interface{}) (string, bool, error) {
	switch value := value.(type) {
	case nil:
		return "", false, nil
	case string:
		return value, false, nil
	case []byte:
		if utf8.Valid(value) {
			return string(value), false, nil
		}
		return "0x" + hex.EncodeToString(value), true, nil
	default:
		return stringValue([]byte(fmt.Sprint(value)))
	}
}
