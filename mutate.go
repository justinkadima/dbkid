package main

// Row CRUD handlers per docs/plans/v2-features.md section A, plus the
// connection plumbing shared with ddl.go and import.go. Column names are
// verified against INFORMATION_SCHEMA before any statement is built, values
// only ever travel as ? placeholders (UNHEX(?) for binary columns), and every
// update or delete targets exactly one row through the table's full primary
// key with a mandatory LIMIT 1.

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"
)

type rowValue struct {
	Null   bool   `json:"null"`
	Value  string `json:"value"`
	Binary bool   `json:"binary"`
}

type rowInsertRequest struct {
	Database  string              `json:"database"`
	Table     string              `json:"table"`
	Values    map[string]rowValue `json:"values"`
	Confirmed bool                `json:"confirmed"`
}

type rowUpdateRequest struct {
	Database  string              `json:"database"`
	Table     string              `json:"table"`
	Key       map[string]string   `json:"key"`
	Values    map[string]rowValue `json:"values"`
	Confirmed bool                `json:"confirmed"`
}

type rowDeleteRequest struct {
	Database  string            `json:"database"`
	Table     string            `json:"table"`
	Key       map[string]string `json:"key"`
	Confirmed bool              `json:"confirmed"`
}

type affectedRowsResponse struct {
	AffectedRows int64 `json:"affected_rows"`
}

func quoteIdentifierList(names []string) string {
	quoted := make([]string, len(names))
	for index, name := range names {
		quoted[index] = quoteIdentifier(name)
	}
	return strings.Join(quoted, ",")
}

// openMutationConn returns the connected database's next fresh request
// connection with the standard 20 second budget. wireLimit overrides the
// inbound byte cap for import connections; 0 keeps the default.
func openMutationConn(w http.ResponseWriter, r *http.Request, current *session, wireLimit int64) (context.Context, context.CancelFunc, *sql.Conn, *connectionBudget, bool) {
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return nil, nil, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	if wireLimit > 0 {
		ctx = context.WithValue(ctx, wireLimitKey{}, wireLimit)
	}
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		cancel()
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return nil, nil, nil, nil, false
	}
	return ctx, cancel, conn, budget, true
}

// runMutation executes one mutation statement on a fresh connection and
// writes {"affected_rows": N}; it returns false once a response was written.
func runMutation(w http.ResponseWriter, r *http.Request, current *session, statement string) bool {
	ctx, cancel, conn, budget, ok := openMutationConn(w, r, current, 0)
	if !ok {
		return false
	}
	defer cancel()
	defer closeRequestConn(conn)
	result, err := conn.ExecContext(ctx, statement)
	if err == nil {
		var affected int64
		affected, err = result.RowsAffected()
		if err == nil {
			writeJSON(w, http.StatusOK, affectedRowsResponse{affected})
			return true
		}
	}
	writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
	return false
}

// execRowMutation runs a built mutation with placeholders on an open
// connection and writes the affected-rows response.
func execRowMutation(w http.ResponseWriter, ctx context.Context, conn *sql.Conn, budget *connectionBudget, statement string, args []interface{}) bool {
	result, err := conn.ExecContext(ctx, statement, args...)
	if err == nil {
		var affected int64
		affected, err = result.RowsAffected()
		if err == nil {
			writeJSON(w, http.StatusOK, affectedRowsResponse{affected})
			return true
		}
	}
	writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
	return false
}

// columnMetaMap indexes a table's column metadata by name.
func columnMetaMap(meta []columnMeta) map[string]columnMeta {
	byName := make(map[string]columnMeta, len(meta))
	for _, column := range meta {
		byName[column.Name] = column
	}
	return byName
}

// cellSQL renders one row value for INSERT or SET clauses: NULL, a plain ?
// placeholder, or UNHEX(?) with the hex string as its argument for binary
// columns. message is "" when the value is valid.
func cellSQL(column columnMeta, value rowValue) (fragment string, arg interface{}, message string) {
	if value.Null {
		return "NULL", nil, ""
	}
	if value.Binary {
		if !isBinaryDataType(column.DataType) {
			return "", nil, "binary values require a binary column"
		}
		if !validHexValue(value.Value) {
			return "", nil, "invalid hex value"
		}
		return "UNHEX(?)", value.Value, ""
	}
	return "?", value.Value, ""
}

// keyCellSQL renders one primary-key comparison for UPDATE/DELETE WHERE
// clauses. Binary key parts arrive as display strings (0x-prefixed hex) or
// as bare hex; both are accepted and bound through UNHEX(?).
func keyCellSQL(column primaryKeyColumn, value string) (fragment string, arg interface{}, message string) {
	if !isBinaryDataType(column.DataType) {
		return "?", value, ""
	}
	hex := value
	if len(hex) >= 2 && (hex[:2] == "0x" || hex[:2] == "0X") {
		hex = hex[2:]
	}
	if !validHexValue(hex) {
		return "", nil, "invalid hex value"
	}
	return "UNHEX(?)", hex, ""
}

// sortedValueNames returns a value map's column names in stable order so
// generated SQL is deterministic.
func sortedValueNames(values map[string]rowValue) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func handleRowInsert(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request rowInsertRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) || !validIdentifier(request.Table) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	if len(request.Values) == 0 {
		writeError(w, http.StatusBadRequest, "values are required")
		return
	}
	ctx, cancel, conn, budget, ok := openMutationConn(w, r, current, 0)
	if !ok {
		return
	}
	defer cancel()
	defer closeRequestConn(conn)
	meta, err := tableColumnMeta(ctx, conn, request.Database, request.Table)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	columns := columnMetaMap(meta)
	names := sortedValueNames(request.Values)
	fragments := make([]string, len(names))
	args := make([]interface{}, 0, len(names))
	for index, name := range names {
		column, known := columns[name]
		if !known {
			writeError(w, http.StatusBadRequest, "unknown column")
			return
		}
		fragment, arg, message := cellSQL(column, request.Values[name])
		if message != "" {
			writeError(w, http.StatusBadRequest, message)
			return
		}
		fragments[index] = fragment
		if fragment != "NULL" {
			args = append(args, arg)
		}
	}
	statement := "INSERT INTO " + quoteIdentifier(request.Database) + "." + quoteIdentifier(request.Table) +
		" (" + quoteIdentifierList(names) + ") VALUES (" + strings.Join(fragments, ", ") + ")"
	execRowMutation(w, ctx, conn, budget, statement, args)
}

func handleRowUpdate(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request rowUpdateRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) || !validIdentifier(request.Table) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	if len(request.Values) == 0 {
		writeError(w, http.StatusBadRequest, "values are required")
		return
	}
	ctx, cancel, conn, budget, ok := openMutationConn(w, r, current, 0)
	if !ok {
		return
	}
	defer cancel()
	defer closeRequestConn(conn)
	meta, err := tableColumnMeta(ctx, conn, request.Database, request.Table)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	primary := tablePrimaryKeys(meta)
	if len(primary) == 0 || len(request.Key) != len(primary) {
		writeError(w, http.StatusBadRequest, "a primary key is required")
		return
	}
	columns := columnMetaMap(meta)
	valueNames := sortedValueNames(request.Values)
	setFragments := make([]string, len(valueNames))
	args := make([]interface{}, 0, len(valueNames)+len(primary))
	for index, name := range valueNames {
		column, known := columns[name]
		if !known {
			writeError(w, http.StatusBadRequest, "unknown column")
			return
		}
		fragment, arg, message := cellSQL(column, request.Values[name])
		if message != "" {
			writeError(w, http.StatusBadRequest, message)
			return
		}
		setFragments[index] = quoteIdentifier(name) + "=" + fragment
		if fragment != "NULL" {
			args = append(args, arg)
		}
	}
	whereFragments := make([]string, 0, len(primary))
	for _, column := range primary {
		value, present := request.Key[column.Name]
		if !present {
			writeError(w, http.StatusBadRequest, "a primary key is required")
			return
		}
		fragment, arg, message := keyCellSQL(column, value)
		if message != "" {
			writeError(w, http.StatusBadRequest, message)
			return
		}
		whereFragments = append(whereFragments, quoteIdentifier(column.Name)+"="+fragment)
		args = append(args, arg)
	}
	statement := "UPDATE " + quoteIdentifier(request.Database) + "." + quoteIdentifier(request.Table) +
		" SET " + strings.Join(setFragments, ", ") +
		" WHERE " + strings.Join(whereFragments, " AND ") + " LIMIT 1"
	execRowMutation(w, ctx, conn, budget, statement, args)
}

func handleRowDelete(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request rowDeleteRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) || !validIdentifier(request.Table) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	ctx, cancel, conn, budget, ok := openMutationConn(w, r, current, 0)
	if !ok {
		return
	}
	defer cancel()
	defer closeRequestConn(conn)
	meta, err := tableColumnMeta(ctx, conn, request.Database, request.Table)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	primary := tablePrimaryKeys(meta)
	if len(primary) == 0 || len(request.Key) != len(primary) {
		writeError(w, http.StatusBadRequest, "a primary key is required")
		return
	}
	whereFragments := make([]string, 0, len(primary))
	args := make([]interface{}, 0, len(primary))
	for _, column := range primary {
		value, present := request.Key[column.Name]
		if !present {
			writeError(w, http.StatusBadRequest, "a primary key is required")
			return
		}
		fragment, arg, message := keyCellSQL(column, value)
		if message != "" {
			writeError(w, http.StatusBadRequest, message)
			return
		}
		whereFragments = append(whereFragments, quoteIdentifier(column.Name)+"="+fragment)
		args = append(args, arg)
	}
	statement := "DELETE FROM " + quoteIdentifier(request.Database) + "." + quoteIdentifier(request.Table) +
		" WHERE " + strings.Join(whereFragments, " AND ") + " LIMIT 1"
	execRowMutation(w, ctx, conn, budget, statement, args)
}
