package main

// Import handlers per docs/plans/v2-features.md section A. SQL scripts are
// split with a real scanner that tracks strings, comments and DELIMITER
// routine bodies, then run sequentially on ONE fresh connection after USE.
// CSV files are parsed per RFC-4180 and inserted in prepared batches of 100
// rows. Neither import wraps statements in a transaction: everything that ran
// stays applied, which matches phpMyAdmin's behavior.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

const maxImportContentBytes = 8 << 20
const maxImportStatements = 20000
const maxImportPreviewBytes = 200
const maxImportBatchRows = 100
const maxCSVImportRows = 50000
const maxImportErrors = 100

// A JSON-escaped import body can occupy up to six times its decoded size.
const maxImportBodyBytes = 6*maxImportContentBytes + (64 << 10)

// Import connections legitimately read more than the default inbound cap.
const importWireLimit int64 = 64 << 20

var errTooManyStatements = errors.New("too many statements")
var errInvalidDelimiter = errors.New("invalid DELIMITER directive")

type importSQLRequest struct {
	Database    string `json:"database"`
	Content     string `json:"content"`
	StopOnError bool   `json:"stop_on_error"`
	Confirmed   bool   `json:"confirmed"`
}

type importCSVRequest struct {
	Database  string    `json:"database"`
	Table     string    `json:"table"`
	Content   string    `json:"content"`
	HasHeader bool      `json:"has_header"`
	Delimiter string    `json:"delimiter"`
	Quote     string    `json:"quote"`
	Columns   []*string `json:"columns"`
	Confirmed bool      `json:"confirmed"`
}

type importSQLError struct {
	Index   int    `json:"index"`
	Preview string `json:"preview"`
	Error   string `json:"error"`
}

type importSQLResult struct {
	Executed int              `json:"executed"`
	Failed   int              `json:"failed"`
	Errors   []importSQLError `json:"errors"`
	Stopped  bool             `json:"stopped"`
}

type importCSVError struct {
	Row   int    `json:"row"`
	Error string `json:"error"`
}

type importCSVResult struct {
	Inserted int              `json:"inserted"`
	Failed   int              `json:"failed"`
	Errors   []importCSVError `json:"errors"`
	Stopped  bool             `json:"stopped"`
}

// decodeImportJSON mirrors decodeJSON with the import-specific body limit so
// that up to 8 MiB of content is accepted while oversized imports are refused.
func decodeImportJSON(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		writeError(w, http.StatusUnsupportedMediaType, "application/json is required")
		return false
	}
	mediaType, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		writeError(w, http.StatusUnsupportedMediaType, "application/json is required")
		return false
	}
	if r.ContentLength > maxImportBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "import file is too large")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportBodyBytes))
	if err != nil {
		if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge, "import file is too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' || !utf8.Valid(body) {
		writeError(w, http.StatusBadRequest, "a JSON object is required")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON object")
		return false
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON object")
		return false
	}
	return true
}

// splitSQLStatements splits a script on the active delimiter while tracking
// ” and "" strings (backslash escapes and doubled quotes), backtick
// identifiers (doubled backticks; backslash is literal there), -- and # line
// comments, /* */ block comments and DELIMITER directive lines. Statements
// containing only whitespace and comments are dropped.
func splitSQLStatements(script string) ([]string, error) {
	statements := make([]string, 0, 16)
	delimiter := ";"
	pending := make([]byte, 0, 256)
	hasContent := false
	i, n := 0, len(script)
	for i < n {
		if !hasContent {
			if token, consumed, isDirective := delimiterDirective(script[i:]); isDirective {
				if token == "" {
					return nil, errInvalidDelimiter
				}
				delimiter = token
				pending = pending[:0]
				i += consumed
				continue
			}
		}
		if strings.HasPrefix(script[i:], delimiter) {
			i += len(delimiter)
			if hasContent {
				if statement := strings.TrimSpace(string(pending)); statement != "" {
					statements = append(statements, statement)
					if len(statements) > maxImportStatements {
						return nil, errTooManyStatements
					}
				}
			}
			pending = pending[:0]
			hasContent = false
			continue
		}
		var consumed int
		switch c := script[i]; {
		case c == '\'' || c == '"':
			pending, consumed = appendQuoted(pending, script[i:])
			hasContent = true
		case c == '`':
			pending, consumed = appendBackquoted(pending, script[i:])
			hasContent = true
		case c == '-' && strings.HasPrefix(script[i:], "--") && dashCommentStarts(script, i+2):
			pending, consumed = appendLineComment(pending, script[i:])
		case c == '#':
			pending, consumed = appendLineComment(pending, script[i:])
		case c == '/' && strings.HasPrefix(script[i:], "/*"):
			pending, consumed = appendBlockComment(pending, script[i:])
		default:
			pending = append(pending, c)
			i++
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				hasContent = true
			}
			continue
		}
		i += consumed
	}
	if hasContent {
		if statement := strings.TrimSpace(string(pending)); statement != "" {
			statements = append(statements, statement)
			if len(statements) > maxImportStatements {
				return nil, errTooManyStatements
			}
		}
	}
	return statements, nil
}

// delimiterDirective reports whether rest starts a DELIMITER directive line
// and returns the new delimiter token plus the bytes consumed including the
// line break. An empty token means the keyword had no operand.
func delimiterDirective(rest string) (token string, consumed int, isDirective bool) {
	if len(rest) < 9 || !strings.EqualFold(rest[:9], "DELIMITER") {
		return "", 0, false
	}
	if len(rest) > 9 {
		switch c := rest[9]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		default:
			return "", 0, false
		}
	}
	// The keyword is its own word, so this line is a directive.
	lineEnd := strings.IndexByte(rest, '\n')
	line := rest
	if lineEnd >= 0 {
		line = rest[:lineEnd]
		consumed = lineEnd + 1
	} else {
		consumed = len(rest)
	}
	if fields := strings.Fields(line); len(fields) >= 2 {
		token = fields[1]
	}
	return token, consumed, true
}

// dashCommentStarts reports whether "--" opens a line comment: MySQL requires
// whitespace, a control character or end-of-input after the second dash.
func dashCommentStarts(script string, after int) bool {
	if after >= len(script) {
		return true
	}
	c := script[after]
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c < 0x20
}

// appendQuoted copies a quoted string (single or double quotes, with
// backslash escapes and doubled quotes) into dst and returns the bytes
// consumed. An unterminated string consumes the rest of the script.
func appendQuoted(dst []byte, rest string) ([]byte, int) {
	quote := rest[0]
	dst = append(dst, quote)
	i := 1
	for i < len(rest) {
		if rest[i] == '\\' && i+1 < len(rest) {
			dst = append(dst, rest[i], rest[i+1])
			i += 2
			continue
		}
		if rest[i] == quote {
			if i+1 < len(rest) && rest[i+1] == quote {
				dst = append(dst, rest[i], rest[i+1])
				i += 2
				continue
			}
			dst = append(dst, quote)
			i++
			break
		}
		dst = append(dst, rest[i])
		i++
	}
	return dst, i
}

// appendBackquoted copies a backtick identifier; only doubled backticks
// escape inside identifiers, backslash is a literal byte there.
func appendBackquoted(dst []byte, rest string) ([]byte, int) {
	dst = append(dst, '`')
	i := 1
	for i < len(rest) {
		if rest[i] == '`' {
			if i+1 < len(rest) && rest[i+1] == '`' {
				dst = append(dst, rest[i], rest[i+1])
				i += 2
				continue
			}
			dst = append(dst, '`')
			i++
			break
		}
		dst = append(dst, rest[i])
		i++
	}
	return dst, i
}

// appendLineComment copies a -- or # comment up to (not including) the line
// break; the break itself is handled as ordinary whitespace.
func appendLineComment(dst []byte, rest string) ([]byte, int) {
	if end := strings.IndexByte(rest, '\n'); end >= 0 {
		return append(dst, rest[:end]...), end
	}
	return append(dst, rest...), len(rest)
}

// appendBlockComment copies a /* */ comment; an unterminated comment
// consumes the rest of the script.
func appendBlockComment(dst []byte, rest string) ([]byte, int) {
	if end := strings.Index(rest[2:], "*/"); end >= 0 {
		end += 4
		return append(dst, rest[:end]...), end
	}
	return append(dst, rest...), len(rest)
}

// statementPreview returns the first 200 bytes of a statement, cut on a UTF-8
// rune boundary, with invalid sequences replaced.
func statementPreview(statement string) string {
	preview := statement
	if len(preview) > maxImportPreviewBytes {
		preview = preview[:maxImportPreviewBytes]
		for cut := 1; cut <= utf8.UTFMax && cut <= len(preview); cut++ {
			if utf8.ValidString(preview[:len(preview)-cut]) {
				preview = preview[:len(preview)-cut]
				break
			}
		}
	}
	return strings.ToValidUTF8(preview, "\ufffd")
}

// isServerError reports whether the database itself rejected the statement,
// as opposed to a transport failure that leaves the connection unusable.
func isServerError(err error) bool {
	var mysqlError *mysql.MySQLError
	return errors.As(err, &mysqlError)
}

// execImportStatement runs one imported statement through QueryContext;
// row-producing statements are drained by closing the rows immediately.
func execImportStatement(ctx context.Context, conn *sql.Conn, statement string) error {
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		return err
	}
	_, _ = rows.Columns()
	return rows.Close()
}

// runSQLImport executes the script's statements sequentially on one fresh
// connection after USE `database`. Server-reported statement errors are
// recorded per statement (and stop the script on request); transport failures
// abort the whole request because the connection is unusable.
func runSQLImport(ctx context.Context, conn *sql.Conn, budget *connectionBudget, database string, statements []string, stopOnError bool) (importSQLResult, error) {
	result := importSQLResult{Errors: make([]importSQLError, 0)}
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdentifier(database)); err != nil {
		return result, budget.resultError(err)
	}
	for index, statement := range statements {
		err := execImportStatement(ctx, conn, statement)
		if err == nil {
			result.Executed++
			continue
		}
		if !isServerError(err) {
			return result, budget.resultError(err)
		}
		result.Failed++
		if len(result.Errors) < maxImportErrors {
			result.Errors = append(result.Errors, importSQLError{
				Index:   index + 1,
				Preview: statementPreview(statement),
				Error:   databaseErrorMessage(err),
			})
		}
		if stopOnError {
			result.Stopped = true
			return result, nil
		}
	}
	return result, nil
}

type csvField struct {
	value  string
	quoted bool
}

// parseCSVRecords parses RFC-4180 records: quoted fields with doubled
// quotes, newlines inside quotes and CRLF/LF line terminators. Each field
// remembers whether it was quoted: an unquoted empty field is a NULL
// candidate, a quoted empty field is the empty string.
func parseCSVRecords(content string, delimiter, quote byte) [][]csvField {
	records := make([][]csvField, 0, 16)
	fields := make([]csvField, 0, 8)
	var value []byte
	quoted, inQuotes := false, false
	endRecord := func() {
		fields = append(fields, csvField{string(value), quoted})
		records = append(records, fields)
		fields = make([]csvField, 0, len(fields))
		value, quoted, inQuotes = nil, false, false
	}
	i, n := 0, len(content)
	for i < n {
		c := content[i]
		if inQuotes {
			if c == quote {
				if i+1 < n && content[i+1] == quote {
					value = append(value, quote)
					i += 2
					continue
				}
				inQuotes = false
				i++
				continue
			}
			value = append(value, c)
			i++
			continue
		}
		switch c {
		case quote:
			if len(value) == 0 && !quoted {
				quoted, inQuotes = true, true
			} else {
				// A stray quote inside a field stays literal.
				value = append(value, c)
			}
			i++
		case delimiter:
			fields = append(fields, csvField{string(value), quoted})
			value, quoted = nil, false
			i++
		case '\r':
			if i+1 < n && content[i+1] == '\n' {
				i++
			}
			endRecord()
			i++
		case '\n':
			endRecord()
			i++
		default:
			value = append(value, c)
			i++
		}
	}
	if inQuotes || quoted || len(value) > 0 || len(fields) > 0 {
		endRecord()
	}
	return records
}

// csvFieldArg converts one parsed field for its target column: unquoted empty
// fields become NULL on nullable columns and the empty string otherwise;
// quoted fields and non-empty fields are used verbatim.
func csvFieldArg(field csvField, meta columnMeta) interface{} {
	if !field.quoted && field.value == "" {
		if meta.Nullable {
			return nil
		}
		return ""
	}
	return field.value
}

type csvColumnTarget struct {
	fieldIndex int
	meta       columnMeta
}

// runCSVImport inserts the parsed rows in prepared batches of 100 via one
// multi-row INSERT per batch. The first batch error is reported with its
// first row number and stops the import.
func runCSVImport(ctx context.Context, conn *sql.Conn, budget *connectionBudget, request importCSVRequest, dataRows [][]csvField, targets []csvColumnTarget) (importCSVResult, error) {
	result := importCSVResult{Errors: make([]importCSVError, 0)}
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdentifier(request.Database)); err != nil {
		return result, budget.resultError(err)
	}
	names := make([]string, len(targets))
	for index, target := range targets {
		names[index] = target.meta.Name
	}
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", len(targets)), ",") + ")"
	prefix := "INSERT INTO " + quoteIdentifier(request.Database) + "." + quoteIdentifier(request.Table) +
		" (" + quoteIdentifierList(names) + ") VALUES "
	for start := 0; start < len(dataRows); start += maxImportBatchRows {
		end := start + maxImportBatchRows
		if end > len(dataRows) {
			end = len(dataRows)
		}
		statement := prefix + strings.TrimSuffix(strings.Repeat(tuple+",", end-start), ",")
		args := make([]interface{}, 0, (end-start)*len(targets))
		for _, row := range dataRows[start:end] {
			for _, target := range targets {
				args = append(args, csvFieldArg(row[target.fieldIndex], target.meta))
			}
		}
		execResult, err := conn.ExecContext(ctx, statement, args...)
		if err == nil {
			var affected int64
			affected, err = execResult.RowsAffected()
			if err == nil {
				result.Inserted += int(affected)
				continue
			}
		}
		if !isServerError(err) {
			return result, budget.resultError(err)
		}
		result.Failed = len(dataRows) - result.Inserted
		if len(result.Errors) < maxImportErrors {
			result.Errors = append(result.Errors, importCSVError{
				Row:   start + 1,
				Error: databaseErrorMessage(err),
			})
		}
		result.Stopped = true
		return result, nil
	}
	return result, nil
}

func handleImportSQL(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request importSQLRequest
	if !decodeImportJSON(w, r, &request) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) {
		writeError(w, http.StatusBadRequest, "a valid database name is required")
		return
	}
	if strings.TrimSpace(request.Content) == "" {
		writeError(w, http.StatusBadRequest, "import content is required")
		return
	}
	if len(request.Content) > maxImportContentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "import file is too large")
		return
	}
	statements, err := splitSQLStatements(request.Content)
	if err != nil {
		if errors.Is(err, errTooManyStatements) {
			writeError(w, http.StatusBadRequest, "too many statements")
		} else {
			writeError(w, http.StatusBadRequest, "invalid DELIMITER directive")
		}
		return
	}
	ctx, cancel, conn, budget, ok := openMutationConn(w, r, current, importWireLimit)
	if !ok {
		return
	}
	defer cancel()
	defer closeRequestConn(conn)
	result, err := runSQLImport(ctx, conn, budget, request.Database, statements, request.StopOnError)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func handleImportCSV(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request importCSVRequest
	if !decodeImportJSON(w, r, &request) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validIdentifier(request.Database) || !validIdentifier(request.Table) {
		writeError(w, http.StatusBadRequest, "database and table names are required")
		return
	}
	if strings.TrimSpace(request.Content) == "" {
		writeError(w, http.StatusBadRequest, "import content is required")
		return
	}
	if len(request.Content) > maxImportContentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "import file is too large")
		return
	}
	delimiter, validDelimiter := csvDelimiter(request.Delimiter)
	if !validDelimiter {
		writeError(w, http.StatusBadRequest, "invalid csv delimiter")
		return
	}
	quote, validQuote := csvQuoteCharacter(request.Quote)
	if !validQuote {
		writeError(w, http.StatusBadRequest, "invalid csv quote")
		return
	}
	records := parseCSVRecords(request.Content, delimiter, quote)
	filtered := make([][]csvField, 0, len(records))
	for _, record := range records {
		if len(record) == 1 && !record[0].quoted && record[0].value == "" {
			continue // a blank line
		}
		filtered = append(filtered, record)
	}
	if len(filtered) == 0 {
		writeError(w, http.StatusBadRequest, "import content is required")
		return
	}
	fieldCount := len(filtered[0])
	for _, record := range filtered {
		if len(record) != fieldCount {
			writeError(w, http.StatusBadRequest, "inconsistent csv columns")
			return
		}
	}
	if len(request.Columns) != fieldCount {
		writeError(w, http.StatusBadRequest, "inconsistent csv columns")
		return
	}
	dataRows := filtered
	if request.HasHeader {
		dataRows = filtered[1:]
	}
	if len(dataRows) > maxCSVImportRows {
		writeError(w, http.StatusBadRequest, "too many rows")
		return
	}
	ctx, cancel, conn, budget, ok := openMutationConn(w, r, current, importWireLimit)
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
	byName := columnMetaMap(meta)
	targets := make([]csvColumnTarget, 0, len(request.Columns))
	seen := make(map[string]bool, len(request.Columns))
	for index, name := range request.Columns {
		if name == nil {
			continue
		}
		column, known := byName[*name]
		if !known {
			writeError(w, http.StatusBadRequest, "unknown column")
			return
		}
		if seen[*name] {
			writeError(w, http.StatusBadRequest, "duplicate column mapping")
			return
		}
		seen[*name] = true
		targets = append(targets, csvColumnTarget{fieldIndex: index, meta: column})
	}
	if len(targets) == 0 && len(dataRows) > 0 {
		writeError(w, http.StatusBadRequest, "a column mapping is required")
		return
	}
	result, err := runCSVImport(ctx, conn, budget, request, dataRows, targets)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func csvDelimiter(value string) (byte, bool) {
	switch value {
	case ",":
		return ',', true
	case ";":
		return ';', true
	case "\t":
		return '\t', true
	case "|":
		return '|', true
	}
	return 0, false
}

func csvQuoteCharacter(value string) (byte, bool) {
	switch value {
	case "\"":
		return '"', true
	case "'":
		return '\'', true
	}
	return 0, false
}
