package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

// metadata contains every SHOW CREATE column, normalized to lowercase. A NULL,
// duplicate, unknown encoding or missing required field makes editing unsafe.
type objectSnapshot struct {
	Database   string            `json:"database"`
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	Generation uint64            `json:"generation"`
	Definition string            `json:"definition"`
	Metadata   map[string]string `json:"metadata"`
	Truncated  bool              `json:"truncated"`
}

type objectReplaceRequest struct {
	Original  objectSnapshot `json:"original"`
	SQL       string         `json:"sql"`
	Confirmed bool           `json:"confirmed"`
}

func objectColumn(kind string) (string, string) {
	switch kind {
	case "procedure":
		return "PROCEDURE", "create procedure"
	case "trigger":
		return "TRIGGER", "sql original statement"
	case "event":
		return "EVENT", "create event"
	}
	return "", ""
}

func snapshotMetadataValid(s objectSnapshot) bool {
	_, definitionColumn := objectColumn(s.Kind)
	if definitionColumn == "" || s.Definition == "" || !utf8.ValidString(s.Definition) || s.Metadata[definitionColumn] != s.Definition || s.Metadata[s.Kind] != s.Name {
		return false
	}
	// Unknown SHOW CREATE columns may represent context we cannot reproduce.
	for key := range s.Metadata {
		switch key {
		case s.Kind, definitionColumn, "sql_mode", "character_set_client", "collation_connection", "database collation":
		case "time_zone":
			if s.Kind != "event" {
				return false
			}
		case "created":
			if s.Kind != "trigger" {
				return false
			}
		default:
			return false
		}
	}
	for _, k := range []string{"sql_mode", "character_set_client", "collation_connection", "database collation"} {
		value, ok := s.Metadata[k]
		if !ok || (k != "sql_mode" && value == "") {
			return false
		}
	}
	if s.Kind == "event" && s.Metadata["time_zone"] == "" {
		return false
	}
	return true
}

// readObjectSnapshot consumes exactly one bounded SHOW CREATE result; abort the
// transport before closing rows on any abnormal result to avoid draining it.
func readObjectSnapshot(ctx context.Context, conn *sql.Conn, budget *connectionBudget, database, name, kind string, generation uint64) (out objectSnapshot, err error) {
	keyword, definitionColumn := objectColumn(kind)
	out = objectSnapshot{Database: database, Name: name, Kind: kind, Generation: generation, Metadata: map[string]string{}}
	rows, err := conn.QueryContext(ctx, "SHOW CREATE "+keyword+" "+quoteIdentifier(database)+"."+quoteIdentifier(name))
	if err != nil {
		return out, budget.resultError(err)
	}
	defer func() {
		if err != nil {
			budget.abort()
		}
		_ = rows.Close()
	}()
	columns, err := rows.Columns()
	if err != nil {
		return out, err
	}
	if len(columns) == 0 || len(columns) > 64 {
		return out, errors.New("unsupported object metadata")
	}
	keys := make([]string, len(columns))
	vals := make([]sql.NullString, len(columns))
	dest := make([]interface{}, len(columns))
	seen := map[string]bool{}
	for i, c := range columns {
		k := strings.ToLower(c)
		if k == "" || seen[k] {
			return out, errors.New("ambiguous object definition result")
		}
		seen[k] = true
		keys[i] = k
		dest[i] = &vals[i]
	}
	if !seen[definitionColumn] {
		return out, errors.New("object definition is unavailable")
	}
	if !rows.Next() {
		if e := rows.Err(); e != nil {
			return out, e
		}
		return out, sql.ErrNoRows
	}
	if err = rows.Scan(dest...); err != nil {
		return out, err
	}
	for i, v := range vals {
		if !v.Valid || !utf8.ValidString(v.String) {
			return out, errors.New("object metadata is unavailable or not valid UTF-8")
		}
		out.Metadata[keys[i]] = v.String
	}
	out.Definition = out.Metadata[definitionColumn]
	if out.Definition == "" {
		return out, errors.New("object definition is unavailable")
	}
	encoded, e := json.Marshal(out)
	if e != nil || len(encoded)+1 > maxResultJSONBytes {
		return out, errResultTooLarge
	}
	if !snapshotMetadataValid(out) {
		return out, errors.New("object creation metadata is unavailable or unsupported")
	}
	if rows.Next() {
		return out, errors.New("ambiguous object definition result")
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if err = requireSingleResultSet(rows); err != nil {
		return out, err
	}
	return out, nil
}

func objectSessionMatches(ctx context.Context, conn *sql.Conn, budget *connectionBudget, s objectSnapshot) (ok bool, err error) {
	if _, err = conn.ExecContext(ctx, "USE "+quoteIdentifier(s.Database)); err != nil {
		return false, err
	}
	rows, err := conn.QueryContext(ctx, "SELECT @@SESSION.sql_mode, @@SESSION.character_set_client, @@SESSION.collation_connection, @@SESSION.time_zone, @@collation_database")
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			budget.abort()
		}
		_ = rows.Close()
	}()
	if !rows.Next() {
		if e := rows.Err(); e != nil {
			return false, e
		}
		return false, errors.New("session context unavailable")
	}
	var mode, charset, collation, zone, dbcollation sql.NullString
	if err = rows.Scan(&mode, &charset, &collation, &zone, &dbcollation); err != nil {
		return false, err
	}
	if rows.Next() {
		return false, errors.New("ambiguous session context")
	}
	if err = rows.Err(); err != nil {
		return false, err
	}
	if err = requireSingleResultSet(rows); err != nil {
		return false, err
	}
	if !mode.Valid || !charset.Valid || !collation.Valid || !zone.Valid || !dbcollation.Valid {
		return false, errors.New("session context unavailable")
	}
	m := s.Metadata
	return mode.String == m["sql_mode"] && charset.String == m["character_set_client"] && collation.String == m["collation_connection"] && dbcollation.String == m["database collation"] && (s.Kind != "event" || zone.String == m["time_zone"]), nil
}

type replaceResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *replaceResponseWriter) Header() http.Header { return w.header }
func (w *replaceResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *replaceResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	w.body = append(w.body, b...)
	return len(b), nil
}

// Every error carries a stage, including decodeJSON and the shared replay gate.
func handleObjectReplace(w http.ResponseWriter, r *http.Request, current *session) {
	tmp := &replaceResponseWriter{header: make(http.Header)}
	doObjectReplace(tmp, r, current)
	var result map[string]interface{}
	if json.Unmarshal(tmp.body, &result) != nil {
		result = map[string]interface{}{"error": "operation failed"}
	}
	if _, ok := result["stage"]; !ok {
		result["stage"] = "not_started"
	}
	if tmp.status == 0 {
		tmp.status = http.StatusInternalServerError
	}
	writeJSON(w, tmp.status, result)
}

func doObjectReplace(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	// Reserve the ID before decoding so even malformed replays cannot be
	// reported as not started. Confirmation and target validation still occur
	// before any database operation.
	db, ok := writeGateCapture(w, r, current, true, nil)
	if !ok {
		return
	}
	var in objectReplaceRequest
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if !in.Confirmed {
		writeError(w, 400, "explicit SQL confirmation is required")
		return
	}
	s := in.Original
	// Fail closed: without a complete SQL-mode-aware body lexer we cannot
	// distinguish executable comments from the same text in string literals.
	// Consequently even a harmless quoted /*! or /*M! is rejected here.
	if !validIdentifier(s.Database) || !validIdentifier(s.Name) || strings.ContainsRune(s.Database, '\\') || strings.ContainsRune(s.Name, '\\') || s.Truncated || !snapshotMetadataValid(s) || strings.ContainsRune(s.Definition, '\r') || strings.ContainsRune(in.SQL, '\r') || !objectCreateMatches(in.SQL, s.Database, s.Name, s.Kind) || strings.Contains(strings.ToUpper(in.SQL), "/*!") || strings.Contains(strings.ToUpper(in.SQL), "/*M!") {
		writeError(w, 400, "invalid original or unsupported CREATE target")
		return
	}
	current.mu.RLock()
	validCapture := !current.retired && current.db == db && current.generation == s.Generation
	current.mu.RUnlock()
	if !validCapture {
		writeError(w, http.StatusConflict, "connection generation changed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, 502, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	matches, err := objectSessionMatches(ctx, conn, budget, s)
	if err != nil {
		writeError(w, 502, databaseErrorMessage(budget.resultError(err)))
		return
	}
	if !matches {
		writeError(w, 409, "session or database creation context changed")
		return
	}
	actual, err := readObjectSnapshot(ctx, conn, budget, s.Database, s.Name, s.Kind, s.Generation)
	if err != nil {
		writeError(w, 502, databaseErrorMessage(budget.resultError(err)))
		return
	}
	if !reflect.DeepEqual(actual, s) {
		writeError(w, 409, "object definition or metadata changed")
		return
	}
	keyword, _ := objectColumn(s.Kind)
	if _, err = conn.ExecContext(ctx, "DROP "+keyword+" "+quoteIdentifier(s.Database)+"."+quoteIdentifier(s.Name)); err != nil {
		writeJSON(w, 502, map[string]string{"stage": "drop_unknown", "error": databaseErrorMessage(budget.resultError(err))})
		return
	}
	if _, err = conn.ExecContext(ctx, in.SQL); err != nil {
		stage := "create_unknown"
		var dbErr *mysql.MySQLError
		// A syntax rejection precedes execution; interruption and other server
		// errors may leave effects and are therefore classified as uncertain.
		if errors.As(err, &dbErr) && dbErr.Number == 1064 {
			stage = "dropped_create_failed"
		}
		writeJSON(w, 502, map[string]string{"stage": stage, "error": databaseErrorMessage(budget.resultError(err))})
		return
	}
	// CREATE can succeed even when an event immediately expires. Verify on the
	// same pinned connection, within the existing request deadline and budget.
	created, err := readObjectSnapshot(ctx, conn, budget, s.Database, s.Name, s.Kind, s.Generation)
	if err != nil || !objectCreateMatches(created.Definition, s.Database, s.Name, s.Kind) || created.Definition != in.SQL {
		writeJSON(w, http.StatusBadGateway, map[string]string{"stage": "created_unverified", "error": "CREATE completed, but the resulting object could not be verified. Inspect the server before retrying; no automatic restore or retry was attempted."})
		return
	}
	writeJSON(w, 200, map[string]string{"stage": "replaced", "warning": "Not atomic; concurrent DDL may race. Grants, trigger order, and event schedules are not guaranteed to be preserved."})
}

func handleObjectDefinition(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	values := r.URL.Query()
	database, name, kind := values.Get("database"), values.Get("name"), values.Get("kind")
	keyword, _ := objectColumn(kind)
	if !validIdentifier(database) || !validIdentifier(name) || strings.ContainsRune(database, '\\') || strings.ContainsRune(name, '\\') {
		writeError(w, 400, "valid database and object names are required")
		return
	}
	if keyword == "" {
		writeError(w, 400, "invalid object kind")
		return
	}
	current.mu.RLock()
	db, generation := current.db, current.generation
	retired := current.retired
	current.mu.RUnlock()
	if db == nil || retired {
		writeError(w, 409, "not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, 502, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	result, err := readObjectSnapshot(ctx, conn, budget, database, name, kind, generation)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "object definition not found")
			return
		}
		if errors.Is(err, errResultTooLarge) {
			writeError(w, 502, databaseErrorMessage(err))
			return
		}
		if strings.HasPrefix(err.Error(), "object ") || strings.HasPrefix(err.Error(), "ambiguous ") || strings.HasPrefix(err.Error(), "unsupported ") {
			writeError(w, 502, err.Error())
			return
		}
		writeError(w, 502, databaseErrorMessage(budget.resultError(err)))
		return
	}
	writeJSON(w, 200, result)
}
