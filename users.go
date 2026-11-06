package main

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// ------------------------------------------------------- users and privileges
//
// Every operation runs as the connected database account; MySQL enforces what
// that account may do. Passwords are embedded server-side into the built
// statement and must never surface in a response, the history, or a log line;
// the handlers here never call recordHistory and never echo request fields.

type accountInfo struct {
	User string `json:"user"`
	Host string `json:"host"`
}

const maxUsersListed = 1000

// quoteSQLString quotes a MySQL string literal with single-quote doubling
// only, so the statement is safe in NO_BACKSLASH_ESCAPES mode as well.
func quoteSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// accountLiteral renders 'user'@'host' for statements.
func accountLiteral(user, host string) string {
	return quoteSQLString(user) + "@" + quoteSQLString(host)
}

func validAccountPart(value string) bool {
	return value != "" && len(value) <= 160 && !strings.ContainsRune(value, 0)
}

func validPassword(value string) bool {
	return len(value) <= 128 && !strings.ContainsRune(value, 0)
}

var privilegeWhitelist = map[string]bool{
	"ALL": true, "SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"CREATE": true, "DROP": true, "ALTER": true, "INDEX": true, "REFERENCES": true,
	"CREATE VIEW": true, "SHOW VIEW": true, "CREATE ROUTINE": true, "ALTER ROUTINE": true,
	"EXECUTE": true, "TRIGGER": true, "EVENT": true, "LOCK TABLES": true,
	"CREATE TEMPORARY TABLES": true, "SHOW DATABASES": true,
}

func normalizePrivileges(privileges []string) ([]string, string) {
	if len(privileges) < 1 || len(privileges) > 32 {
		return nil, "at least one privilege is required"
	}
	seen := make(map[string]bool, len(privileges))
	result := make([]string, 0, len(privileges))
	for _, privilege := range privileges {
		normalized := strings.ToUpper(strings.TrimSpace(privilege))
		if !privilegeWhitelist[normalized] {
			return nil, "unsupported privilege"
		}
		if normalized == "ALL" {
			return []string{"ALL PRIVILEGES"}, ""
		}
		if !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	return result, ""
}

// privilegeScope renders the ON clause; table scope requires a database.
func privilegeScope(database, table string) (string, string) {
	switch {
	case database == "" && table == "":
		return "*.*", ""
	case database != "" && table == "":
		return quoteIdentifier(database) + ".*", ""
	case database != "" && table != "":
		return quoteIdentifier(database) + "." + quoteIdentifier(table), ""
	default:
		return "", "a database is required to scope privileges to a table"
	}
}

func handleUsers(w http.ResponseWriter, r *http.Request, current *session) {
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
	users, err := listUsers(ctx, conn)
	if err != nil {
		// The connected account cannot read mysql.user; that is a capability
		// report, not a failure.
		_ = budget
		writeJSON(w, http.StatusOK, struct {
			Users      []accountInfo `json:"users"`
			Restricted bool          `json:"restricted"`
		}{Users: []accountInfo{}, Restricted: true})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Users      []accountInfo `json:"users"`
		Restricted bool          `json:"restricted"`
	}{Users: users, Restricted: false})
}

func listUsers(ctx context.Context, conn *sql.Conn) (result []accountInfo, resultErr error) {
	rows, err := conn.QueryContext(ctx, "SELECT User, Host FROM mysql.user ORDER BY User, Host LIMIT ?",
		maxUsersListed)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && resultErr == nil {
			resultErr = closeErr
		}
	}()
	users := make([]accountInfo, 0, 16)
	for rows.Next() {
		var user, host string
		if err := rows.Scan(&user, &host); err != nil {
			return nil, err
		}
		users = append(users, accountInfo{User: user, Host: host})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return users, nil
}

func handleGrants(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	values := r.URL.Query()
	user, host := values.Get("user"), values.Get("host")
	if (user == "") != (host == "") {
		writeError(w, http.StatusBadRequest, "user and host are required")
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
	statement := "SHOW GRANTS"
	if user != "" {
		if !validAccountPart(user) || !validAccountPart(host) {
			writeError(w, http.StatusBadRequest, "user and host are required")
			return
		}
		statement = "SHOW GRANTS FOR " + accountLiteral(user, host)
	}
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	grants := make([]string, 0, 4)
	for rows.Next() {
		targets := make([]interface{}, len(columns))
		values := make([]sql.NullString, len(columns))
		for index := range targets {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		if values[0].Valid {
			grants = append(grants, values[0].String)
		}
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Grants []string `json:"grants"`
	}{grants})
}

type userCreateRequest struct {
	User      string `json:"user"`
	Host      string `json:"host"`
	Password  string `json:"password"`
	Confirmed bool   `json:"confirmed"`
}

func handleUserCreate(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request userCreateRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validAccountPart(request.User) {
		writeError(w, http.StatusBadRequest, "a user name is required")
		return
	}
	if !validAccountPart(request.Host) {
		writeError(w, http.StatusBadRequest, "a host is required")
		return
	}
	if !validPassword(request.Password) {
		writeError(w, http.StatusBadRequest, "the password is too long (128 bytes maximum)")
		return
	}
	statement := "CREATE USER " + accountLiteral(request.User, request.Host)
	if request.Password != "" {
		statement += " IDENTIFIED BY " + quoteSQLString(request.Password)
	}
	runMutation(w, r, current, statement)
}

type userPasswordRequest struct {
	User      string `json:"user"`
	Host      string `json:"host"`
	Password  string `json:"password"`
	Confirmed bool   `json:"confirmed"`
}

func handleUserPassword(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request userPasswordRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validAccountPart(request.User) {
		writeError(w, http.StatusBadRequest, "a user name is required")
		return
	}
	if !validAccountPart(request.Host) {
		writeError(w, http.StatusBadRequest, "a host is required")
		return
	}
	if request.Password == "" {
		writeError(w, http.StatusBadRequest, "a password is required")
		return
	}
	if !validPassword(request.Password) {
		writeError(w, http.StatusBadRequest, "the password is too long (128 bytes maximum)")
		return
	}
	runMutation(w, r, current, "ALTER USER "+accountLiteral(request.User, request.Host)+
		" IDENTIFIED BY "+quoteSQLString(request.Password))
}

type userDropRequest struct {
	User      string `json:"user"`
	Host      string `json:"host"`
	Confirmed bool   `json:"confirmed"`
}

func handleUserDrop(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request userDropRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validAccountPart(request.User) {
		writeError(w, http.StatusBadRequest, "a user name is required")
		return
	}
	if !validAccountPart(request.Host) {
		writeError(w, http.StatusBadRequest, "a host is required")
		return
	}
	runMutation(w, r, current, "DROP USER "+accountLiteral(request.User, request.Host))
}

type privilegeRequest struct {
	User        string   `json:"user"`
	Host        string   `json:"host"`
	Privileges  []string `json:"privileges"`
	Database    string   `json:"database"`
	Table       string   `json:"table"`
	GrantOption bool     `json:"grant_option"`
	Confirmed   bool     `json:"confirmed"`
}

// runMutations executes the statements on one connection so multi-statement
// revokes share the outcome reporting of a single request.
func runMutations(w http.ResponseWriter, r *http.Request, current *session, statements []string) {
	ctx, cancel, conn, budget, ok := openMutationConn(w, r, current, 0)
	if !ok {
		return
	}
	defer cancel()
	defer closeRequestConn(conn)
	affected := int64(0)
	for _, statement := range statements {
		result, err := conn.ExecContext(ctx, statement)
		if err == nil {
			var rowsAffected int64
			rowsAffected, err = result.RowsAffected()
			if err == nil {
				affected += rowsAffected
				continue
			}
		}
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	writeJSON(w, http.StatusOK, affectedRowsResponse{affected})
}

func handleGrant(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request privilegeRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	privileges, message := normalizePrivileges(request.Privileges)
	if message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	scope, message := privilegeScope(request.Database, request.Table)
	if message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	if !validAccountPart(request.User) || !validAccountPart(request.Host) {
		writeError(w, http.StatusBadRequest, "a user name and host are required")
		return
	}
	statement := "GRANT " + strings.Join(privileges, ", ") + " ON " + scope + " TO " +
		accountLiteral(request.User, request.Host)
	if request.GrantOption {
		statement += " WITH GRANT OPTION"
	}
	runMutations(w, r, current, []string{statement})
}

func handleRevoke(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request privilegeRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !writeGate(w, r, current, request.Confirmed) {
		return
	}
	if !validAccountPart(request.User) || !validAccountPart(request.Host) {
		writeError(w, http.StatusBadRequest, "a user name and host are required")
		return
	}
	privileges, message := normalizePrivileges(request.Privileges)
	if message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	if len(privileges) == 1 && privileges[0] == "ALL PRIVILEGES" {
		// Revoking ALL requires the special form without a scope clause.
		runMutations(w, r, current, []string{"REVOKE ALL PRIVILEGES, GRANT OPTION FROM " +
			accountLiteral(request.User, request.Host)})
		return
	}
	scope, message := privilegeScope(request.Database, request.Table)
	if message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	statements := make([]string, 0, 2)
	if request.GrantOption {
		statements = append(statements, "REVOKE GRANT OPTION ON "+scope+" FROM "+
			accountLiteral(request.User, request.Host))
	}
	statements = append(statements, "REVOKE "+strings.Join(privileges, ", ")+" ON "+scope+" FROM "+
		accountLiteral(request.User, request.Host))
	runMutations(w, r, current, statements)
}
