package main

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// Server status, SQL history and recent connections.
// Implemented per docs/plans/v2-features.md section B.

const statusRowCap = 500

func handleStatus(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
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
	status, err := collectShowRows(ctx, conn, budget, "SHOW GLOBAL STATUS")
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	variables, err := collectShowRows(ctx, conn, budget, "SHOW GLOBAL VARIABLES")
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	processes, err := collectShowRows(ctx, conn, budget, "SHOW PROCESSLIST")
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status    [][]interface{} `json:"status"`
		Variables [][]interface{} `json:"variables"`
		Processes [][]interface{} `json:"processes"`
	}{status, variables, processes})
}

// collectShowRows runs one bounded SHOW statement on an existing connection.
func collectShowRows(ctx context.Context, conn *sql.Conn, budget *connectionBudget, statement string) ([][]interface{}, error) {
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		return nil, budget.resultError(err)
	}
	_, values, _, _, err := collectRows(rows, statusRowCap)
	if err != nil {
		budget.abort()
		_ = rows.Close()
		return nil, budget.resultError(err)
	}
	if err := requireSingleResultSet(rows); err != nil {
		return nil, budget.resultError(err)
	}
	if err := rows.Close(); err != nil {
		return nil, budget.resultError(err)
	}
	return values, nil
}

// handleHistory serves GET /api/history (list, newest first) and, once the
// dispatcher routes the path, POST /api/history/clear.
func handleHistory(w http.ResponseWriter, r *http.Request, current *session) {
	if r.URL.Path == "/api/history/clear" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !decodeJSON(w, r, &struct{}{}, true) {
			return
		}
		current.clearHistory()
		writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	snapshot := current.historySnapshot()
	history := make([]historyEntry, 0, len(snapshot))
	for i := len(snapshot) - 1; i >= 0; i-- {
		history = append(history, snapshot[i])
	}
	writeJSON(w, http.StatusOK, struct {
		History []historyEntry `json:"history"`
	}{history})
}

func handleRecent(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Recent []recentConnection `json:"recent"`
	}{current.srv.recentSnapshot()})
}
