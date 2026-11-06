package main

// DDL endpoints per docs/plans/v2-features.md section A: databases, tables,
// columns, indexes, routines and process kills. Every handler decodes its own
// JSON body, passes it through writeGate, validates identifiers via
// validIdentifier and renders exactly one statement through quoteIdentifier.
// User values never appear in generated SQL text.

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const maxTableColumns = 64

// Types are rendered uppercased exactly as given in the whitelist.
var columnTypes = map[string]string{
	"int":        "INT",
	"bigint":     "BIGINT",
	"smallint":   "SMALLINT",
	"tinyint":    "TINYINT",
	"mediumint":  "MEDIUMINT",
	"varchar":    "VARCHAR",
	"char":       "CHAR",
	"text":       "TEXT",
	"tinytext":   "TINYTEXT",
	"mediumtext": "MEDIUMTEXT",
	"longtext":   "LONGTEXT",
	"date":       "DATE",
	"datetime":   "DATETIME",
	"timestamp":  "TIMESTAMP",
	"time":       "TIME",
	"year":       "YEAR",
	"decimal":    "DECIMAL",
	"float":      "FLOAT",
	"double":     "DOUBLE",
	"boolean":    "BOOLEAN",
	"bool":       "BOOL",
	"json":       "JSON",
	"blob":       "BLOB",
	"tinyblob":   "TINYBLOB",
	"mediumblob": "MEDIUMBLOB",
	"longblob":   "LONGBLOB",
	"binary":     "BINARY",
	"varbinary":  "VARBINARY",
}

// Engines are rendered in their canonical spelling.
var storageEngines = map[string]string{
	"innodb":  "InnoDB",
	"myisam":  "MyISAM",
	"memory":  "MEMORY",
	"csv":     "CSV",
	"archive": "ARCHIVE",
	"aria":    "ARIA",
}

var columnLengthPattern = regexp.MustCompile(`^[0-9]{1,3}$`)
var defaultNumberPattern = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
var processIDPattern = regexp.MustCompile(`^[0-9]+$`)

type columnDef struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	Length        string `json:"length"`
	Decimals      string `json:"decimals"`
	Nullable      bool   `json:"nullable"`
	AutoIncrement bool   `json:"auto_increment"`
	Primary       bool   `json:"primary"`
	Unique        bool   `json:"unique"`
	Default       string `json:"default"`
	DefaultNull   bool   `json:"default_null"`
}

// definition renders the column definition used by table create, column add
// and column modify. message is "" on success, otherwise a HTTP 400 message.
func (def columnDef) definition() (text, message string) {
	if !validIdentifier(def.Name) {
		return "", "a valid column name is required"
	}
	rendered, ok := columnTypes[def.Type]
	if !ok {
		return "", "unsupported column type"
	}
	if def.Length != "" && !columnLengthPattern.MatchString(def.Length) {
		return "", "invalid column length"
	}
	if def.Decimals != "" && !columnLengthPattern.MatchString(def.Decimals) {
		return "", "invalid column decimals"
	}
	// Decimals only apply to exact and approximate numeric types and require
	// a length; other types ignore them.
	withDecimals := def.Decimals != "" &&
		(def.Type == "decimal" || def.Type == "float" || def.Type == "double")
	if withDecimals && def.Length == "" {
		return "", "a length is required with decimals"
	}
	switch {
	case withDecimals:
		rendered += "(" + def.Length + "," + def.Decimals + ")"
	case def.Length != "":
		rendered += "(" + def.Length + ")"
	}
	if def.Nullable {
		rendered += " NULL"
	} else {
		rendered += " NOT NULL"
	}
	if def.DefaultNull {
		if !def.Nullable {
			return "", "unsupported default value"
		}
		rendered += " DEFAULT NULL"
	} else if def.Default != "" {
		switch {
		case def.Default == "CURRENT_TIMESTAMP":
			if def.Type != "timestamp" && def.Type != "datetime" {
				return "", "unsupported default value"
			}
			rendered += " DEFAULT CURRENT_TIMESTAMP"
		case defaultNumberPattern.MatchString(def.Default):
			rendered += " DEFAULT " + def.Default
		case !strings.ContainsAny(def.Default, "'\\"):
			rendered += " DEFAULT '" + def.Default + "'"
		default:
			return "", "unsupported default value"
		}
	}
	if def.AutoIncrement {
		if !def.Primary && !def.Unique {
			return "", "auto-increment requires a key"
		}
		rendered += " AUTO_INCREMENT"
	}
	return quoteIdentifier(def.Name) + " " + rendered, ""
}

type databaseCreateRequest struct {
	Name      string `json:"name"`
	Confirmed bool   `json:"confirmed"`
}

type databaseDropRequest struct {
	Database  string `json:"database"`
	Confirmed bool   `json:"confirmed"`
}

type tableCreateRequest struct {
	Database  string      `json:"database"`
	Name      string      `json:"name"`
	Engine    string      `json:"engine"`
	Columns   []columnDef `json:"columns"`
	Confirmed bool        `json:"confirmed"`
}

type tableRenameRequest struct {
	Database  string `json:"database"`
	Table     string `json:"table"`
	NewName   string `json:"new_name"`
	Confirmed bool   `json:"confirmed"`
}

type tableDropRequest struct {
	Database  string `json:"database"`
	Table     string `json:"table"`
	Confirmed bool   `json:"confirmed"`
}

type columnAddRequest struct {
	Database    string    `json:"database"`
	Table       string    `json:"table"`
	Column      columnDef `json:"column"`
	Position    string    `json:"position"`
	AfterColumn string    `json:"after_column"`
	Confirmed   bool      `json:"confirmed"`
}

type columnModifyRequest struct {
	Database  string    `json:"database"`
	Table     string    `json:"table"`
	OldName   string    `json:"old_name"`
	Column    columnDef `json:"column"`
	Confirmed bool      `json:"confirmed"`
}

type columnDropRequest struct {
	Database  string `json:"database"`
	Table     string `json:"table"`
	Column    string `json:"column"`
	Confirmed bool   `json:"confirmed"`
}

type indexCreateRequest struct {
	Database  string   `json:"database"`
	Table     string   `json:"table"`
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	Unique    bool     `json:"unique"`
	Confirmed bool     `json:"confirmed"`
}

type indexDropRequest struct {
	Database  string `json:"database"`
	Table     string `json:"table"`
	Name      string `json:"name"`
	Confirmed bool   `json:"confirmed"`
}

type routineDropRequest struct {
	Database  string `json:"database"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Confirmed bool   `json:"confirmed"`
}

type processKillRequest struct {
	ID        json.Number `json:"id"`
	Confirmed bool        `json:"confirmed"`
}

func handleDatabaseCreate(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request databaseCreateRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Name) {
		writeError(w, http.StatusBadRequest, "a valid database name is required")
		return
	}
	runMutation(w, r, current, "CREATE DATABASE "+quoteIdentifier(request.Name))
}

func handleDatabaseDrop(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request databaseDropRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) {
		writeError(w, http.StatusBadRequest, "a valid database name is required")
		return
	}
	runMutation(w, r, current, "DROP DATABASE "+quoteIdentifier(request.Database))
}

func handleTableCreate(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request tableCreateRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) || !validIdentifier(request.Name) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	if len(request.Columns) == 0 {
		writeError(w, http.StatusBadRequest, "a column definition is required")
		return
	}
	if len(request.Columns) > maxTableColumns {
		writeError(w, http.StatusBadRequest, "too many columns")
		return
	}
	engine := ""
	if request.Engine != "" {
		var ok bool
		if engine, ok = storageEngines[strings.ToLower(request.Engine)]; !ok {
			writeError(w, http.StatusBadRequest, "unsupported storage engine")
			return
		}
	}
	parts := make([]string, 0, len(request.Columns)+1)
	primary := make([]string, 0, 1)
	for _, column := range request.Columns {
		definition, message := column.definition()
		if message != "" {
			writeError(w, http.StatusBadRequest, message)
			return
		}
		parts = append(parts, definition)
		if column.Primary {
			primary = append(primary, column.Name)
		}
	}
	if len(primary) > 0 {
		parts = append(parts, "PRIMARY KEY ("+quoteIdentifierList(primary)+")")
	}
	for _, column := range request.Columns {
		if column.Unique {
			parts = append(parts, "UNIQUE KEY "+quoteIdentifier("u_"+column.Name)+" ("+quoteIdentifier(column.Name)+")")
		}
	}
	statement := "CREATE TABLE " + quoteIdentifier(request.Database) + "." + quoteIdentifier(request.Name) +
		" (" + strings.Join(parts, ", ") + ")"
	if engine != "" {
		statement += " ENGINE=" + engine
	}
	runMutation(w, r, current, statement)
}

func handleTableRename(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request tableRenameRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) || !validIdentifier(request.Table) || !validIdentifier(request.NewName) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	runMutation(w, r, current, "RENAME TABLE "+quoteIdentifier(request.Database)+"."+quoteIdentifier(request.Table)+
		" TO "+quoteIdentifier(request.Database)+"."+quoteIdentifier(request.NewName))
}

func handleTableDrop(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request tableDropRequest
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
	runMutation(w, r, current, "DROP TABLE "+quoteIdentifier(request.Database)+"."+quoteIdentifier(request.Table))
}

func handleColumnAdd(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request columnAddRequest
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
	definition, message := request.Column.definition()
	if message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	suffix := ""
	switch request.Position {
	case "":
	case "first":
		suffix = " FIRST"
	case "after":
		if !validIdentifier(request.AfterColumn) {
			writeError(w, http.StatusBadRequest, "a valid column name is required")
			return
		}
		suffix = " AFTER " + quoteIdentifier(request.AfterColumn)
	default:
		writeError(w, http.StatusBadRequest, "invalid column position")
		return
	}
	runMutation(w, r, current, "ALTER TABLE "+quoteIdentifier(request.Database)+"."+quoteIdentifier(request.Table)+
		" ADD COLUMN "+definition+suffix)
}

func handleColumnModify(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request columnModifyRequest
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
	if !validIdentifier(request.OldName) {
		writeError(w, http.StatusBadRequest, "a valid column name is required")
		return
	}
	definition, message := request.Column.definition()
	if message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	runMutation(w, r, current, "ALTER TABLE "+quoteIdentifier(request.Database)+"."+quoteIdentifier(request.Table)+
		" CHANGE COLUMN "+quoteIdentifier(request.OldName)+" "+definition)
}

func handleColumnDrop(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request columnDropRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) || !validIdentifier(request.Table) || !validIdentifier(request.Column) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	runMutation(w, r, current, "ALTER TABLE "+quoteIdentifier(request.Database)+"."+quoteIdentifier(request.Table)+
		" DROP COLUMN "+quoteIdentifier(request.Column))
}

func handleIndexCreate(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request indexCreateRequest
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
	if !validIdentifier(request.Name) {
		writeError(w, http.StatusBadRequest, "a valid index name is required")
		return
	}
	if len(request.Columns) < 1 || len(request.Columns) > 16 {
		writeError(w, http.StatusBadRequest, "an index requires 1 to 16 columns")
		return
	}
	for _, column := range request.Columns {
		if !validIdentifier(column) {
			writeError(w, http.StatusBadRequest, "a valid column name is required")
			return
		}
	}
	keyword := "CREATE INDEX "
	if request.Unique {
		keyword = "CREATE UNIQUE INDEX "
	}
	runMutation(w, r, current, keyword+quoteIdentifier(request.Name)+" ON "+
		quoteIdentifier(request.Database)+"."+quoteIdentifier(request.Table)+
		" ("+quoteIdentifierList(request.Columns)+")")
}

func handleIndexDrop(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request indexDropRequest
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
	if !validIdentifier(request.Name) {
		writeError(w, http.StatusBadRequest, "a valid index name is required")
		return
	}
	runMutation(w, r, current, "ALTER TABLE "+quoteIdentifier(request.Database)+"."+quoteIdentifier(request.Table)+
		" DROP INDEX "+quoteIdentifier(request.Name))
}

func handleRoutineDrop(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request routineDropRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) || !validIdentifier(request.Name) {
		writeError(w, http.StatusBadRequest, "a valid database and routine name are required")
		return
	}
	var verb string
	switch request.Kind {
	case "procedure":
		verb = "DROP PROCEDURE "
	case "function":
		verb = "DROP FUNCTION "
	case "trigger":
		verb = "DROP TRIGGER "
	case "event":
		verb = "DROP EVENT "
	default:
		writeError(w, http.StatusBadRequest, "unsupported routine kind")
		return
	}
	runMutation(w, r, current, verb+quoteIdentifier(request.Database)+"."+quoteIdentifier(request.Name))
}

func handleProcessKill(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request processKillRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	id := request.ID.String()
	if !processIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	number, err := strconv.ParseUint(id, 10, 64)
	if err != nil || number > math.MaxInt64 {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	runMutation(w, r, current, "KILL "+id)
}
