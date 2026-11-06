package main

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Full-database search. Implemented per docs/plans/v2-features.md section B.

const (
	searchMaxQueryChars = 256
	searchMaxTables     = 200
	searchStopAfterHits = 50
	searchPerTableRows  = 20
	searchTotalRowCap   = 1000
)

type searchTableResult struct {
	Table              string          `json:"table"`
	Columns            []string        `json:"columns"`
	Rows               [][]interface{} `json:"rows"`
	More               bool            `json:"more"`
	MatchedColumnCount int             `json:"matched_column_count"`
}

func handleSearch(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	values := r.URL.Query()
	database, term, mode := values.Get("database"), values.Get("q"), values.Get("mode")
	if !validIdentifier(database) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	if term == "" || len(term) > searchMaxQueryChars {
		writeError(w, http.StatusBadRequest, "a search term is required")
		return
	}
	if mode == "" {
		mode = "like"
	}
	if mode != "like" && mode != "exact" {
		writeError(w, http.StatusBadRequest, "invalid search mode")
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
	results, err := searchDatabase(ctx, conn, database, term, mode)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Results []searchTableResult `json:"results"`
	}{results})
}

// searchDatabase scans a database's tables on one connection. LIKE mode wraps
// the term with wildcards; exact mode compares equality. Bounded by the
// collectRows display rules, 20 rows per table, 50 tables with matches, 200
// tables scanned, 1000 total rows and a 2 MiB JSON budget.
func searchDatabase(ctx context.Context, conn *sql.Conn, database, term, mode string) (result []searchTableResult, resultErr error) {
	tables, err := listTableNames(ctx, conn, database)
	if err != nil {
		return nil, err
	}
	result = make([]searchTableResult, 0)
	used := resultEnvelopeBytes
	totalRows, hits, scanned := 0, 0, 0
	for _, table := range tables {
		if scanned >= searchMaxTables || hits >= searchStopAfterHits || totalRows >= searchTotalRowCap {
			break
		}
		columns, err := stringColumns(ctx, conn, database, table)
		if err != nil {
			return nil, err
		}
		if len(columns) == 0 {
			continue
		}
		scanned++
		fragments := make([]string, 0, len(columns))
		args := make([]interface{}, 0, len(columns))
		for _, column := range columns {
			if mode == "exact" {
				fragments = append(fragments, quoteIdentifier(column)+" = ?")
				args = append(args, term)
			} else {
				fragments = append(fragments, quoteIdentifier(column)+" LIKE ?")
				args = append(args, "%"+term+"%")
			}
		}
		query := "SELECT * FROM " + quoteIdentifier(database) + "." + quoteIdentifier(table) +
			" WHERE (" + strings.Join(fragments, " OR ") + ") LIMIT " + strconv.Itoa(searchPerTableRows+1)
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		names, values, more, _, err := collectRows(rows, searchPerTableRows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		// The LIMIT bounds the lookahead to one extra row, so closing drains
		// at most that single row and the connection stays usable.
		if err := requireSingleResultSet(rows); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if len(values) == 0 {
			continue
		}
		hits++
		totalRows += len(values)
		entry := searchTableResult{
			Table:              table,
			Columns:            names,
			Rows:               values,
			More:               more,
			MatchedColumnCount: len(columns),
		}
		if err := accountJSONValue(&used, entry); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, nil
}

// listTableNames mirrors listTables' query on an existing connection and
// applies the search table cap.
func listTableNames(ctx context.Context, conn *sql.Conn, database string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT TABLE_NAME, TABLE_TYPE FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE IN ('BASE TABLE', 'VIEW') ORDER BY TABLE_NAME`, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make([]string, 0)
	for rows.Next() {
		var name, tableType string
		if err := rows.Scan(&name, &tableType); err != nil {
			return nil, err
		}
		if len(name) > maxCellDisplayBytes || len(tableType) > maxCellDisplayBytes {
			return nil, errResultTooLarge
		}
		tables = append(tables, name)
		if len(tables) == searchMaxTables {
			break
		}
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, err
	}
	return tables, nil
}

// stringColumns returns a table's string-family column names in ordinal order.
func stringColumns(ctx context.Context, conn *sql.Conn, database, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, database, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make([]string, 0)
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, err
		}
		if len(name) > maxCellDisplayBytes || len(dataType) > maxCellDisplayBytes {
			return nil, errResultTooLarge
		}
		if isStringDataType(dataType) {
			columns = append(columns, name)
		}
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, err
	}
	return columns, nil
}
