package main

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func (b testBrowser) mutationWithID(s *server, path, body, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080"+path, strings.NewReader(body))
	req.AddCookie(b.cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", b.token)
	if id != "" {
		req.Header.Set("X-DBKid-Request-ID", id)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func (b testBrowser) mutation(s *server, path, body string) *httptest.ResponseRecorder {
	id, err := randomToken()
	if err != nil {
		panic(err)
	}
	return b.mutationWithID(s, path, body, id)
}

func recordArgs(record fakeRecord) []interface{} {
	args := make([]interface{}, len(record.args))
	for index, arg := range record.args {
		args[index] = arg.Value
	}
	return args
}

// execOnlyEngine returns an engine whose metadata query serves items-shaped
// columns and whose ExecContext executes mutations.
func execOnlyEngine(t *testing.T, table string, exec func(query string, args []driver.NamedValue) (driver.Result, error)) *fakeEngine {
	t.Helper()
	engine := metaEngine(t, table, nil)
	engine.exec = exec
	return engine
}

func TestRowInsertParameterizedSQL(t *testing.T) {
	engine := execOnlyEngine(t, "items", func(query string, args []driver.NamedValue) (driver.Result, error) {
		if !strings.HasPrefix(query, "INSERT INTO") {
			return nil, fmt.Errorf("unexpected mutation: %s", query)
		}
		return driver.RowsAffected(1), nil
	})
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := `{"database":"db","table":"items","values":{` +
		`"id":{"value":"1"},"label":{"value":"x"},"note":{"null":true},` +
		`"raw_data":{"binary":true,"value":"FF00"}},"confirmed":true}`
	rec := b.mutation(s, "/api/row/insert", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("insert: %d %s", rec.Code, rec.Body.String())
	}
	var response affectedRowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.AffectedRows != 1 {
		t.Fatalf("insert response: %s (%v)", rec.Body.String(), err)
	}
	if len(engine.records) != 2 || engine.records[0].exec || !engine.records[1].exec {
		t.Fatalf("expected metadata query then insert: %+v", engine.records)
	}
	want := "INSERT INTO `db`.`items` (`id`,`label`,`note`,`raw_data`) VALUES (?, ?, NULL, UNHEX(?))"
	if engine.records[1].query != want {
		t.Fatalf("insert SQL = %q, want %q", engine.records[1].query, want)
	}
	args := recordArgs(engine.records[1])
	if len(args) != 3 || args[0] != "1" || args[1] != "x" || args[2] != "FF00" {
		t.Fatalf("insert args = %#v", args)
	}
}

func TestRowInsertValidationBranches(t *testing.T) {
	cases := []struct{ name, body, message string }{
		{"unknown column",
			`{"database":"db","table":"items","values":{"nonexistent":{"value":"x"}},"confirmed":true}`,
			"unknown column"},
		{"binary flag on string column",
			`{"database":"db","table":"items","values":{"note":{"binary":true,"value":"FF"}},"confirmed":true}`,
			"binary values require a binary column"},
		{"binary non-hex value",
			`{"database":"db","table":"items","values":{"raw_data":{"binary":true,"value":"GG00"}},"confirmed":true}`,
			"invalid hex value"},
		{"binary odd-length value",
			`{"database":"db","table":"items","values":{"raw_data":{"binary":true,"value":"FFF"}},"confirmed":true}`,
			"invalid hex value"},
		{"binary empty value without null",
			`{"database":"db","table":"items","values":{"raw_data":{"binary":true,"value":""}},"confirmed":true}`,
			"invalid hex value"},
		{"empty values map",
			`{"database":"db","table":"items","values":{},"confirmed":true}`,
			"values are required"},
		{"missing table name",
			`{"database":"db","table":"","values":{"id":{"value":"1"}},"confirmed":true}`,
			"database and table names are required"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			engine := metaEngine(t, "items", nil)
			engine.exec = func(query string, _ []driver.NamedValue) (driver.Result, error) {
				return nil, fmt.Errorf("rejected insert reached the database: %s", query)
			}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.mutation(s, "/api/row/insert", tt.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.message) {
				t.Fatalf("response = %d %s, want 400 containing %q", rec.Code, rec.Body.String(), tt.message)
			}
			if count := recordCount(engine); count > 1 {
				t.Fatalf("rejected insert produced %d statements", count)
			}
		})
	}
}

func TestRowUpdateSQLAndPrimaryKeyRules(t *testing.T) {
	t.Run("exact statement", func(t *testing.T) {
		engine := execOnlyEngine(t, "items", func(query string, args []driver.NamedValue) (driver.Result, error) {
			if !strings.HasPrefix(query, "UPDATE ") {
				return nil, fmt.Errorf("unexpected mutation: %s", query)
			}
			return driver.RowsAffected(1), nil
		})
		s := newServer()
		b := connectFakeDatabase(t, s, engine)
		body := `{"database":"db","table":"items","key":{"id":"7"},` +
			`"values":{"note":{"value":"new"},"raw_data":{"binary":true,"value":"00ff"}},"confirmed":true}`
		rec := b.mutation(s, "/api/row/update", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
		}
		want := "UPDATE `db`.`items` SET `note`=?, `raw_data`=UNHEX(?) WHERE `id`=? LIMIT 1"
		if lastRecordQuery(engine) != want {
			t.Fatalf("update SQL = %q, want %q", lastRecordQuery(engine), want)
		}
		args := recordArgs(engine.records[len(engine.records)-1])
		if len(args) != 3 || args[0] != "new" || args[1] != "00ff" || args[2] != "7" {
			t.Fatalf("update args = %#v", args)
		}
	})
	t.Run("binary primary key accepts display hex", func(t *testing.T) {
		engine := execOnlyEngine(t, "binkey", func(string, []driver.NamedValue) (driver.Result, error) {
			return driver.RowsAffected(1), nil
		})
		s := newServer()
		b := connectFakeDatabase(t, s, engine)
		for _, tt := range []struct{ key, arg string }{
			{"0xFF00", "FF00"}, {"ff00", "ff00"},
		} {
			body := fmt.Sprintf(`{"database":"db","table":"binkey","key":{"id":%q},"values":{"data":{"value":"x"}},"confirmed":true}`, tt.key)
			rec := b.mutation(s, "/api/row/update", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("binary key update: %d %s", rec.Code, rec.Body.String())
			}
			want := "UPDATE `db`.`binkey` SET `data`=? WHERE `id`=UNHEX(?) LIMIT 1"
			if lastRecordQuery(engine) != want {
				t.Fatalf("binary key SQL = %q, want %q", lastRecordQuery(engine), want)
			}
			args := recordArgs(engine.records[len(engine.records)-1])
			if len(args) != 2 || args[1] != tt.arg {
				t.Fatalf("binary key args = %#v, want hex %q", args, tt.arg)
			}
		}
	})
	invalid := []struct {
		name, table, body, message string
	}{
		{"empty key", "items", `{"database":"db","table":"items","key":{},"values":{"note":{"value":"x"}},"confirmed":true}`, "a primary key is required"},
		{"key with non-key column", "items", `{"database":"db","table":"items","key":{"note":"x"},"values":{"note":{"value":"y"}},"confirmed":true}`, "a primary key is required"},
		{"key with extra column", "items", `{"database":"db","table":"items","key":{"id":"7","note":"x"},"values":{"note":{"value":"y"}},"confirmed":true}`, "a primary key is required"},
		{"table without primary key", "nokey", `{"database":"db","table":"nokey","key":{"value":"7"},"values":{"value":{"value":"y"}},"confirmed":true}`, "a primary key is required"},
		{"binary key invalid hex", "binkey", `{"database":"db","table":"binkey","key":{"id":"0xZZ"},"values":{"data":{"value":"y"}},"confirmed":true}`, "invalid hex value"},
		{"unknown set column", "items", `{"database":"db","table":"items","key":{"id":"7"},"values":{"nonexistent":{"value":"y"}},"confirmed":true}`, "unknown column"},
		{"empty values", "items", `{"database":"db","table":"items","key":{"id":"7"},"values":{},"confirmed":true}`, "values are required"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			engine := metaEngine(t, tt.table, nil)
			engine.exec = func(query string, _ []driver.NamedValue) (driver.Result, error) {
				return nil, fmt.Errorf("rejected update reached the database: %s", query)
			}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.mutation(s, "/api/row/update", tt.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.message) {
				t.Fatalf("response = %d %s, want 400 containing %q", rec.Code, rec.Body.String(), tt.message)
			}
		})
	}
}

func TestRowUpdateCompositePrimaryKey(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if !strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS") {
			return nil, nil, fmt.Errorf("unexpected query: %s", query)
		}
		return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, [][]driver.Value{
			{"a", "int", "int", "NO", "PRI", nil, ""},
			{"b", "int", "int", "NO", "PRI", nil, ""},
		}, nil
	}
	engine.exec = func(string, []driver.NamedValue) (driver.Result, error) {
		return driver.RowsAffected(1), nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	complete := `{"database":"db","table":"t","key":{"b":"2","a":"1"},"values":{"b":{"value":"9"}},"confirmed":true}`
	if rec := b.mutation(s, "/api/row/update", complete); rec.Code != http.StatusOK {
		t.Fatalf("composite update: %d %s", rec.Code, rec.Body.String())
	}
	want := "UPDATE `db`.`t` SET `b`=? WHERE `a`=? AND `b`=? LIMIT 1"
	if lastRecordQuery(engine) != want {
		t.Fatalf("composite SQL = %q, want %q", lastRecordQuery(engine), want)
	}
	args := recordArgs(engine.records[len(engine.records)-1])
	if len(args) != 3 || args[0] != "9" || args[1] != "1" || args[2] != "2" {
		t.Fatalf("composite args = %#v", args)
	}
	partial := `{"database":"db","table":"t","key":{"a":"1"},"values":{"b":{"value":"9"}},"confirmed":true}`
	if rec := b.mutation(s, "/api/row/update", partial); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "a primary key is required") {
		t.Fatalf("partial key accepted: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRowDeleteSQL(t *testing.T) {
	t.Run("exact statement", func(t *testing.T) {
		engine := execOnlyEngine(t, "items", func(query string, args []driver.NamedValue) (driver.Result, error) {
			if !strings.HasPrefix(query, "DELETE FROM") {
				return nil, fmt.Errorf("unexpected mutation: %s", query)
			}
			return driver.RowsAffected(1), nil
		})
		s := newServer()
		b := connectFakeDatabase(t, s, engine)
		body := `{"database":"db","table":"items","key":{"id":"7"},"confirmed":true}`
		rec := b.mutation(s, "/api/row/delete", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
		}
		want := "DELETE FROM `db`.`items` WHERE `id`=? LIMIT 1"
		if lastRecordQuery(engine) != want {
			t.Fatalf("delete SQL = %q, want %q", lastRecordQuery(engine), want)
		}
		if args := recordArgs(engine.records[len(engine.records)-1]); len(args) != 1 || args[0] != "7" {
			t.Fatalf("delete args = %#v", args)
		}
	})
	t.Run("binary primary key", func(t *testing.T) {
		engine := execOnlyEngine(t, "binkey", func(string, []driver.NamedValue) (driver.Result, error) {
			return driver.RowsAffected(1), nil
		})
		s := newServer()
		b := connectFakeDatabase(t, s, engine)
		body := `{"database":"db","table":"binkey","key":{"id":"0xAB"},"confirmed":true}`
		rec := b.mutation(s, "/api/row/delete", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("binary delete: %d %s", rec.Code, rec.Body.String())
		}
		want := "DELETE FROM `db`.`binkey` WHERE `id`=UNHEX(?) LIMIT 1"
		if lastRecordQuery(engine) != want {
			t.Fatalf("binary delete SQL = %q, want %q", lastRecordQuery(engine), want)
		}
		if args := recordArgs(engine.records[len(engine.records)-1]); len(args) != 1 || args[0] != "AB" {
			t.Fatalf("binary delete args = %#v", args)
		}
	})
	t.Run("primary key rules", func(t *testing.T) {
		for _, tt := range []struct{ name, table, body string }{
			{"empty key", "items", `{"database":"db","table":"items","key":{},"confirmed":true}`},
			{"non-key column", "items", `{"database":"db","table":"items","key":{"note":"x"},"confirmed":true}`},
			{"no primary key", "nokey", `{"database":"db","table":"nokey","key":{"value":"x"},"confirmed":true}`},
		} {
			t.Run(tt.name, func(t *testing.T) {
				engine := metaEngine(t, tt.table, nil)
				engine.exec = func(query string, _ []driver.NamedValue) (driver.Result, error) {
					return nil, fmt.Errorf("rejected delete reached the database: %s", query)
				}
				s := newServer()
				b := connectFakeDatabase(t, s, engine)
				rec := b.mutation(s, "/api/row/delete", tt.body)
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "a primary key is required") {
					t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
				}
			})
		}
	})
}

func TestRowMutationsMapDatabaseErrors(t *testing.T) {
	engine := execOnlyEngine(t, "items", func(string, []driver.NamedValue) (driver.Result, error) {
		return nil, &mysql.MySQLError{Number: 1048, Message: "Column cannot be null"}
	})
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := `{"database":"db","table":"items","values":{"id":{"value":"1"}},"confirmed":true}`
	rec := b.mutation(s, "/api/row/insert", body)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "database error 1048") {
		t.Fatalf("database error mapping: %d %s", rec.Code, rec.Body.String())
	}
	failing := &fakeEngine{}
	failing.query = func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return nil, nil, errors.New("metadata transport failure")
	}
	s = newServer()
	b = connectFakeDatabase(t, s, failing)
	rec = b.mutation(s, "/api/row/insert", body)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "database operation failed") {
		t.Fatalf("transport error mapping: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMutationEndpointsRejectBadMethodMissingGateAndReplay(t *testing.T) {
	endpoints := []struct{ path, body string }{
		{"/api/database/create", `{"name":"x","confirmed":false}`},
		{"/api/database/drop", `{"database":"x","confirmed":false}`},
		{"/api/table/create", `{"database":"d","name":"t","engine":"","columns":[],"confirmed":false}`},
		{"/api/table/rename", `{"database":"d","table":"t","new_name":"u","confirmed":false}`},
		{"/api/table/drop", `{"database":"d","table":"t","confirmed":false}`},
		{"/api/column/add", `{"database":"d","table":"t","column":{"name":"c","type":"int"},"position":"","confirmed":false}`},
		{"/api/column/modify", `{"database":"d","table":"t","old_name":"o","column":{"name":"c","type":"int"},"confirmed":false}`},
		{"/api/column/drop", `{"database":"d","table":"t","column":"c","confirmed":false}`},
		{"/api/index/create", `{"database":"d","table":"t","name":"i","columns":["a"],"unique":false,"confirmed":false}`},
		{"/api/index/drop", `{"database":"d","table":"t","name":"i","confirmed":false}`},
		{"/api/routine/drop", `{"database":"d","name":"n","kind":"procedure","confirmed":false}`},
		{"/api/object/create", `{"database":"d","name":"n","kind":"procedure","sql":"CREATE PROCEDURE n() SELECT 1","confirmed":false}`},
		{"/api/process/kill", `{"id":1,"confirmed":false}`},
		{"/api/row/insert", `{"database":"d","table":"t","values":{},"confirmed":false}`},
		{"/api/row/update", `{"database":"d","table":"t","key":{},"values":{},"confirmed":false}`},
		{"/api/row/delete", `{"database":"d","table":"t","key":{},"confirmed":false}`},
		{"/api/import/sql", `{"database":"d","content":"SELECT 1","stop_on_error":false,"confirmed":false}`},
		{"/api/import/csv", `{"database":"d","table":"t","content":"1\n","has_header":false,"delimiter":",","quote":"\"","columns":["a"],"confirmed":false}`},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.path, func(t *testing.T) {
			engine := &fakeEngine{
				query: func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
					return nil, nil, fmt.Errorf("an ungated request reached the database: %s", query)
				},
				exec: func(query string, _ []driver.NamedValue) (driver.Result, error) {
					return nil, fmt.Errorf("an ungated mutation reached the database: %s", query)
				},
			}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			get := httptest.NewRequest(http.MethodGet, "http://localhost:8080"+endpoint.path, nil)
			get.AddCookie(b.cookie)
			get.Header.Set("X-CSRF-Token", b.token)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, get)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("GET = %d, want 405", rec.Code)
			}
			unconfirmed := b.mutation(s, endpoint.path, endpoint.body)
			if unconfirmed.Code != http.StatusBadRequest ||
				!strings.Contains(unconfirmed.Body.String(), "explicit SQL confirmation is required") {
				t.Fatalf("unconfirmed = %d %s", unconfirmed.Code, unconfirmed.Body.String())
			}
			confirmed := strings.Replace(endpoint.body, `"confirmed":false`, `"confirmed":true`, 1)
			if endpoint.path == "/api/object/create" {
				confirmed = createBodyForSession(s, b, confirmed)
			}
			noID := b.mutationWithID(s, endpoint.path, confirmed, "")
			if noID.Code != http.StatusBadRequest ||
				!strings.Contains(noID.Body.String(), "a unique SQL request ID is required") {
				t.Fatalf("missing request ID = %d %s", noID.Code, noID.Body.String())
			}
			if count := recordCount(engine); count != 0 {
				t.Fatalf("a rejected request reached the database %d times", count)
			}
		})
	}
}

func TestMutationReplayRejected(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	const id = "0123456789abcdef0123456789abcdef"
	body := `{"database":"d","table":"t","confirmed":true}`
	first := b.mutationWithID(s, "/api/table/drop", body, id)
	if first.Code != http.StatusOK {
		t.Fatalf("first mutation: %d %s", first.Code, first.Body.String())
	}
	replay := b.mutationWithID(s, "/api/table/drop", body, id)
	if replay.Code != http.StatusConflict ||
		!strings.Contains(replay.Body.String(), "SQL request already submitted") {
		t.Fatalf("replay = %d %s, want 409", replay.Code, replay.Body.String())
	}
	if count := recordCount(engine); count != 1 {
		t.Fatalf("replayed mutation executed %d times", count)
	}
}
