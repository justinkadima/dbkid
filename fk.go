package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------- foreign keys

type foreignKeyInfo struct {
	Name               string   `json:"name"`
	Columns            []string `json:"columns"`
	ReferencedDatabase string   `json:"referenced_database"`
	ReferencedTable    string   `json:"referenced_table"`
	ReferencedColumns  []string `json:"referenced_columns"`
	OnDelete           string   `json:"on_delete"`
	OnUpdate           string   `json:"on_update"`
}

func handleForeignKeys(w http.ResponseWriter, r *http.Request, current *session) {
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
	foreignKeys, err := listForeignKeys(ctx, conn, database, table)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ForeignKeys []foreignKeyInfo `json:"foreign_keys"`
	}{foreignKeys})
}

// listForeignKeys reads the constraint columns from KEY_COLUMN_USAGE and the
// referential rules from REFERENTIAL_CONSTRAINTS, grouped per constraint.
func listForeignKeys(ctx context.Context, conn *sql.Conn, database, table string) (result []foreignKeyInfo, resultErr error) {
	rows, err := conn.QueryContext(ctx, `SELECT CONSTRAINT_NAME, COLUMN_NAME, REFERENCED_TABLE_SCHEMA,
		REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME, ORDINAL_POSITION
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION`, database, table)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && resultErr == nil {
			resultErr = closeErr
		}
	}()
	byName := make(map[string]*foreignKeyInfo)
	order := make([]string, 0, 4)
	for rows.Next() {
		var name, column, refDatabase, refTable, refColumn string
		var position int
		if err := rows.Scan(&name, &column, &refDatabase, &refTable, &refColumn, &position); err != nil {
			return nil, err
		}
		entry, exists := byName[name]
		if !exists {
			entry = &foreignKeyInfo{Name: name, ReferencedDatabase: refDatabase, ReferencedTable: refTable,
				OnDelete: "RESTRICT", OnUpdate: "RESTRICT"}
			byName[name] = entry
			order = append(order, name)
		}
		entry.Columns = append(entry.Columns, column)
		entry.ReferencedColumns = append(entry.ReferencedColumns, refColumn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ruleRows, err := conn.QueryContext(ctx, `SELECT CONSTRAINT_NAME, DELETE_RULE, UPDATE_RULE
		FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = ? AND TABLE_NAME = ?`, database, table)
	if err == nil {
		defer ruleRows.Close()
		for ruleRows.Next() {
			var name, deleteRule, updateRule string
			if scanErr := ruleRows.Scan(&name, &deleteRule, &updateRule); scanErr == nil {
				if entry, exists := byName[name]; exists {
					entry.OnDelete, entry.OnUpdate = deleteRule, updateRule
				}
			}
		}
		_ = ruleRows.Err()
	}
	result = make([]foreignKeyInfo, 0, len(order))
	for _, name := range order {
		result = append(result, *byName[name])
	}
	return result, nil
}

var foreignKeyActions = map[string]bool{
	"NO ACTION": true, "RESTRICT": true, "CASCADE": true, "SET NULL": true, "SET DEFAULT": true,
}

type foreignKeyAddRequest struct {
	Database           string   `json:"database"`
	Table              string   `json:"table"`
	Name               string   `json:"name"`
	Columns            []string `json:"columns"`
	ReferencedDatabase string   `json:"referenced_database"`
	ReferencedTable    string   `json:"referenced_table"`
	ReferencedColumns  []string `json:"referenced_columns"`
	OnDelete           string   `json:"on_delete"`
	OnUpdate           string   `json:"on_update"`
	Confirmed          bool     `json:"confirmed"`
}

func (request foreignKeyAddRequest) constraintName() string {
	if request.Name != "" {
		return request.Name
	}
	generated := "fk_" + request.Table + "_" + request.Columns[0]
	if len(generated) > 64 {
		generated = generated[:64]
	}
	return generated
}

func buildForeignKeyStatement(request foreignKeyAddRequest) (string, string) {
	if !validIdentifier(request.Database) || !validIdentifier(request.Table) {
		return "", "database and table names are required"
	}
	if request.Name != "" && (len(request.Name) > 64 || strings.ContainsRune(request.Name, 0)) {
		return "", "a valid foreign key name is required"
	}
	if len(request.Columns) < 1 || len(request.Columns) > 3 {
		return "", "a foreign key needs 1 to 3 columns"
	}
	if len(request.Columns) != len(request.ReferencedColumns) {
		return "", "a foreign key needs the same number of local and referenced columns"
	}
	for _, column := range append(append([]string{}, request.Columns...), request.ReferencedColumns...) {
		if !validIdentifier(column) {
			return "", "a valid column name is required"
		}
	}
	if !validIdentifier(request.ReferencedDatabase) || !validIdentifier(request.ReferencedTable) {
		return "", "database and table names are required"
	}
	for _, action := range []struct{ value, label string }{{request.OnDelete, "DELETE"}, {request.OnUpdate, "UPDATE"}} {
		if action.value == "" {
			continue
		}
		if !foreignKeyActions[action.value] {
			return "", fmt.Sprintf("unsupported ON %s action", action.label)
		}
	}
	var statement strings.Builder
	statement.WriteString("ALTER TABLE ")
	statement.WriteString(quoteIdentifier(request.Database))
	statement.WriteString(".")
	statement.WriteString(quoteIdentifier(request.Table))
	statement.WriteString(" ADD CONSTRAINT ")
	statement.WriteString(quoteIdentifier(request.constraintName()))
	statement.WriteString(" FOREIGN KEY (")
	statement.WriteString(quoteIdentifierList(request.Columns))
	statement.WriteString(") REFERENCES ")
	statement.WriteString(quoteIdentifier(request.ReferencedDatabase))
	statement.WriteString(".")
	statement.WriteString(quoteIdentifier(request.ReferencedTable))
	statement.WriteString(" (")
	statement.WriteString(quoteIdentifierList(request.ReferencedColumns))
	statement.WriteString(")")
	if request.OnDelete != "" {
		statement.WriteString(" ON DELETE ")
		statement.WriteString(request.OnDelete)
	}
	if request.OnUpdate != "" {
		statement.WriteString(" ON UPDATE ")
		statement.WriteString(request.OnUpdate)
	}
	return statement.String(), ""
}

func handleForeignKeyAdd(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request foreignKeyAddRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	statement, message := buildForeignKeyStatement(request)
	if message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	runMutation(w, r, current, statement)
}

type foreignKeyDropRequest struct {
	Database  string `json:"database"`
	Table     string `json:"table"`
	Name      string `json:"name"`
	Confirmed bool   `json:"confirmed"`
}

func handleForeignKeyDrop(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request foreignKeyDropRequest
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
	if !validIdentifier(request.Name) || len(request.Name) > 64 {
		writeError(w, http.StatusBadRequest, "a valid foreign key name is required")
		return
	}
	runMutation(w, r, current, "ALTER TABLE "+quoteIdentifier(request.Database)+"."+quoteIdentifier(request.Table)+
		" DROP FOREIGN KEY "+quoteIdentifier(request.Name))
}
