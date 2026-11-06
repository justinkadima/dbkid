package main

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestUserStatementsExactSQL(t *testing.T) {
	cases := []struct {
		path string
		body string
		want []string
	}{
		{"/api/user/create",
			`{"user":"reader","host":"127.0.0.1","password":"pw","confirmed":true}`,
			[]string{"CREATE USER 'reader'@'127.0.0.1' IDENTIFIED BY 'pw'"}},
		{"/api/user/create",
			`{"user":"o'brien","host":"%.example","password":"p'w\\x","confirmed":true}`,
			[]string{"CREATE USER 'o''brien'@'%.example' IDENTIFIED BY 'p''w\\x'"}},
		{"/api/user/create",
			`{"user":"fixture","host":"localhost","password":"","confirmed":true}`,
			[]string{"CREATE USER 'fixture'@'localhost'"}},
		{"/api/user/password",
			`{"user":"reader","host":"127.0.0.1","password":"new-pw","confirmed":true}`,
			[]string{"ALTER USER 'reader'@'127.0.0.1' IDENTIFIED BY 'new-pw'"}},
		{"/api/user/drop",
			`{"user":"reader","host":"127.0.0.1","confirmed":true}`,
			[]string{"DROP USER 'reader'@'127.0.0.1'"}},
		{"/api/grant",
			`{"user":"reader","host":"127.0.0.1","privileges":["select","Insert"],"database":"","table":"","grant_option":false,"confirmed":true}`,
			[]string{"GRANT SELECT, INSERT ON *.* TO 'reader'@'127.0.0.1'"}},
		{"/api/grant",
			`{"user":"reader","host":"127.0.0.1","privileges":["select"],"database":"dbkid","table":"","grant_option":true,"confirmed":true}`,
			[]string{"GRANT SELECT ON `dbkid`.* TO 'reader'@'127.0.0.1' WITH GRANT OPTION"}},
		{"/api/grant",
			`{"user":"reader","host":"127.0.0.1","privileges":["ALL"],"database":"dbkid","table":"items","grant_option":false,"confirmed":true}`,
			[]string{"GRANT ALL PRIVILEGES ON `dbkid`.`items` TO 'reader'@'127.0.0.1'"}},
		{"/api/revoke",
			`{"user":"reader","host":"127.0.0.1","privileges":["select","insert"],"database":"dbkid","table":"","grant_option":false,"confirmed":true}`,
			[]string{"REVOKE SELECT, INSERT ON `dbkid`.* FROM 'reader'@'127.0.0.1'"}},
		{"/api/revoke",
			`{"user":"reader","host":"127.0.0.1","privileges":["select"],"database":"dbkid","table":"","grant_option":true,"confirmed":true}`,
			[]string{
				"REVOKE GRANT OPTION ON `dbkid`.* FROM 'reader'@'127.0.0.1'",
				"REVOKE SELECT ON `dbkid`.* FROM 'reader'@'127.0.0.1'",
			}},
		{"/api/revoke",
			`{"user":"reader","host":"127.0.0.1","privileges":["all"],"database":"","table":"","grant_option":false,"confirmed":true}`,
			[]string{"REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'reader'@'127.0.0.1'"}},
	}
	for _, tt := range cases {
		t.Run(tt.path+" "+strings.Join(tt.want, " | "), func(t *testing.T) {
			engine := &fakeEngine{}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.mutation(s, tt.path, tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("mutation: %d %s", rec.Code, rec.Body.String())
			}
			if recordCount(engine) != len(tt.want) {
				t.Fatalf("statements = %d, want %d: %+v", recordCount(engine), len(tt.want), engine.records)
			}
			for index, want := range tt.want {
				if !engine.records[index].exec {
					t.Fatalf("statement %d was not an exec: %+v", index, engine.records[index])
				}
				if engine.records[index].query != want {
					t.Fatalf("statement %d = %q, want %q", index, engine.records[index].query, want)
				}
			}
		})
	}
}

func TestUserValidationBranches(t *testing.T) {
	cases := []struct{ path, body, message string }{
		{"/api/user/create", `{"user":"","host":"h","password":"","confirmed":true}`, "a user name is required"},
		{"/api/user/create", `{"user":"u","host":"","password":"","confirmed":true}`, "a host is required"},
		{"/api/user/create", `{"user":"u","host":"` + strings.Repeat("h", 161) + `","password":"","confirmed":true}`, "a host is required"},
		{"/api/user/create", `{"user":"u","host":"h","password":"` + strings.Repeat("p", 129) + `","confirmed":true}`, "the password is too long (128 bytes maximum)"},
		{"/api/user/password", `{"user":"u","host":"h","password":"","confirmed":true}`, "a password is required"},
		{"/api/user/password", `{"user":"","host":"h","password":"x","confirmed":true}`, "a user name is required"},
		{"/api/user/drop", `{"user":"u","host":"","confirmed":true}`, "a host is required"},
		{"/api/grant", `{"user":"u","host":"h","privileges":[],"database":"","table":"","confirmed":true}`, "at least one privilege is required"},
		{"/api/grant", `{"user":"u","host":"h","privileges":["super"],"database":"","table":"","confirmed":true}`, "unsupported privilege"},
		{"/api/grant", `{"user":"u","host":"h","privileges":["select"],"database":"","table":"t","confirmed":true}`, "a database is required to scope privileges to a table"},
		{"/api/revoke", `{"user":"","host":"h","privileges":["select"],"database":"","table":"","confirmed":true}`, "a user name and host are required"},
	}
	for _, tt := range cases {
		t.Run(tt.path+" "+tt.message, func(t *testing.T) {
			engine := &fakeEngine{}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.mutation(s, tt.path, tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			var payload struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &payload) != nil || payload.Error != tt.message {
				t.Fatalf("error = %q, want %q", rec.Body.String(), tt.message)
			}
			if recordCount(engine) != 0 {
				t.Fatalf("rejected request must not execute SQL: %+v", engine.records)
			}
		})
	}
}

// The password travels into the built statement server-side and must never
// surface in a response body or the SQL history.
func TestPasswordNeverSurfacesInResponsesOrHistory(t *testing.T) {
	const password = "s3cr't-p<w>-x;"
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	for _, path := range []string{"/api/user/create", "/api/user/password"} {
		rec := b.mutation(s, path, `{"user":"probe","host":"127.0.0.1","password":"`+password+`","confirmed":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), password) {
			t.Fatalf("%s response leaked the password: %s", path, rec.Body.String())
		}
	}
	executed := engine.records[len(engine.records)-1].query
	if !strings.Contains(executed, "'s3cr''t-p<w>-x;'") {
		t.Fatalf("the password must be embedded server-side: %q", executed)
	}
	history := b.request(s, http.MethodGet, "/api/history", "")
	if strings.Contains(history.Body.String(), password) {
		t.Fatalf("history leaked the password: %s", history.Body.String())
	}
}

func TestUsersListAndRestrictedMode(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if !strings.Contains(query, "mysql.user") {
			return nil, nil, errors.New("unexpected query: " + query)
		}
		return []string{"User", "Host"}, [][]driver.Value{
			{"admin", "localhost"}, {"reader", "%"},
		}, nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Users      []accountInfo `json:"users"`
		Restricted bool          `json:"restricted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if payload.Restricted || len(payload.Users) != 2 || payload.Users[0].User != "admin" {
		t.Fatalf("unexpected users payload: %s", rec.Body.String())
	}

	denied := &fakeEngine{}
	denied.query = func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return nil, nil, errors.New("SELECT command denied to user")
	}
	s2 := newServer()
	b2 := connectFakeDatabase(t, s2, denied)
	rec2 := b2.request(s2, http.MethodGet, "/api/users", "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("restricted users: %d %s", rec2.Code, rec2.Body.String())
	}
	var payload2 struct {
		Users      []accountInfo `json:"users"`
		Restricted bool          `json:"restricted"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &payload2); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if !payload2.Restricted || len(payload2.Users) != 0 {
		t.Fatalf("restricted mode must report empty list: %s", rec2.Body.String())
	}
}

func TestGrantsEndpointHandlesWideRows(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if !strings.HasPrefix(query, "SHOW GRANTS") {
			return nil, nil, errors.New("unexpected query: " + query)
		}
		// MariaDB returns an extra host column; only column 0 is the grant text.
		return []string{"Grants for reader@%", "Host"},
			[][]driver.Value{
				{"GRANT SELECT ON `db`.* TO 'reader'@'%'", "%"},
				{"GRANT USAGE ON *.* TO 'reader'@'%'", "%"},
			}, nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/grants?user=reader&host=%25", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("grants: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(queryString(engine), "SHOW GRANTS FOR 'reader'@'%'") {
		t.Fatalf("missing SHOW GRANTS FOR: %q", queryString(engine))
	}
	var payload struct {
		Grants []string `json:"grants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.Grants) != 2 {
		t.Fatalf("grants payload wrong: %s", rec.Body.String())
	}
	// Self mode: no user/host runs a plain SHOW GRANTS.
	engine2 := &fakeEngine{}
	engine2.query = func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if query != "SHOW GRANTS" {
			return nil, nil, errors.New("unexpected query: " + query)
		}
		return []string{"Grants"}, [][]driver.Value{{"GRANT USAGE ON *.* TO 'me'@'%'"}}, nil
	}
	s2 := newServer()
	b2 := connectFakeDatabase(t, s2, engine2)
	rec2 := b2.request(s2, http.MethodGet, "/api/grants", "")
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "GRANT USAGE") {
		t.Fatalf("self grants: %d %s", rec2.Code, rec2.Body.String())
	}
	// One-sided parameters are rejected.
	rec3 := b2.request(s2, http.MethodGet, "/api/grants?user=only", "")
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("one-sided params must be 400: %d %s", rec3.Code, rec3.Body.String())
	}
}

func TestUserMutationReplayIsRejected(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := `{"user":"probe","host":"127.0.0.1","password":"x","confirmed":true}`
	first := b.mutationWithID(s, "/api/user/create", body, "fixed-request-id-123456")
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	replay := b.mutationWithID(s, "/api/user/create", body, "fixed-request-id-123456")
	if replay.Code != http.StatusConflict {
		t.Fatalf("replay = %d, want 409", replay.Code)
	}
	if recordCount(engine) != 1 {
		t.Fatalf("replay must not execute twice: %+v", engine.records)
	}
}

func queryString(engine *fakeEngine) string {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	var queries []string
	for _, record := range engine.records {
		queries = append(queries, record.query)
	}
	return strings.Join(queries, "; ")
}
