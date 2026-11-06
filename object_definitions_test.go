package main

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestObjectDefinitionEscapedEnvelopeBound(t *testing.T) {
	definition := strings.Repeat("a", maxResultJSONBytes-resultEnvelopeBytes-2)
	engine := &fakeEngine{query: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return []string{"Create Procedure"}, [][]driver.Value{{definition}}, nil
	}}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	params := url.Values{"database": {"db"}, "name": {strings.Repeat("<", 1024)}, "kind": {"procedure"}}
	rec := b.request(s, http.MethodGet, "/api/object/definition?"+params.Encode(), "")
	if rec.Code == http.StatusOK && rec.Body.Len() > maxResultJSONBytes {
		t.Fatalf("response exceeded JSON cap: %d > %d", rec.Body.Len(), maxResultJSONBytes)
	}
}

func TestObjectDefinitionProcedureComplete(t *testing.T) {
	definition := "CREATE PROCEDURE `p`()\nBEGIN\n" + strings.Repeat("SELECT 'hello';\n", 1000) + "END"
	engine := &fakeEngine{query: func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return []string{"Procedure", "sql_mode", "Create Procedure", "character_set_client", "collation_connection", "Database Collation"}, [][]driver.Value{{"p", "", []byte(definition), "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci"}}, nil
	}}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/object/definition?database=db&name=p&kind=procedure", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Definition string `json:"definition"`
		Truncated  *bool  `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Name != "p" || result.Kind != "procedure" || result.Definition != definition || result.Truncated == nil || *result.Truncated {
		t.Fatalf("invalid result: name=%q kind=%q definition length=%d truncated=%v", result.Name, result.Kind, len(result.Definition), result.Truncated)
	}
	if got := lastRecordQuery(engine); got != "SHOW CREATE PROCEDURE `db`.`p`" {
		t.Fatalf("query %q", got)
	}
}

func TestObjectDefinitionKindsAndQuotedIdentifiers(t *testing.T) {
	for _, tc := range []struct{ kind, column, query string }{
		{"procedure", "Create Procedure", "PROCEDURE"},
		{"trigger", "SQL Original Statement", "TRIGGER"},
		{"event", "Create Event", "EVENT"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			definition := "CREATE " + tc.query + " `p`\nBEGIN SELECT '🐈'; END"
			engine := &fakeEngine{query: func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
				columns := []string{tc.kind, strings.ToLower(tc.column), "sql_mode", "character_set_client", "collation_connection", "Database Collation"}
				values := []driver.Value{"p`x", []byte(definition), "ANSI_QUOTES", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci"}
				if tc.kind == "event" {
					columns = append(columns, "time_zone")
					values = append(values, "SYSTEM")
				}
				return columns, [][]driver.Value{values}, nil
			}}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			params := url.Values{"database": {"d`b"}, "name": {"p`x"}, "kind": {tc.kind}}
			rec := b.request(s, http.MethodGet, "/api/object/definition?"+params.Encode(), "")
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 7 || body["database"] != "d`b" || body["generation"] == nil || body["metadata"] == nil || body["name"] != "p`x" || body["kind"] != tc.kind || body["definition"] != definition || body["truncated"] != false {
				t.Fatalf("unexpected response %#v", body)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
			if got := lastRecordQuery(engine); got != "SHOW CREATE "+tc.query+" `d``b`.`p``x`" {
				t.Fatalf("query %q", got)
			}
			engine.mu.Lock()
			records, closed := len(engine.records), len(engine.closed)
			exec := engine.records[0].exec
			engine.mu.Unlock()
			if records != 1 || exec || closed != 1 {
				t.Fatalf("records=%d exec=%v closed=%d", records, exec, closed)
			}
		})
	}
}

func TestObjectDefinitionUnavailableAndOversize(t *testing.T) {
	cases := []struct {
		name    string
		columns []string
		rows    [][]driver.Value
		err     error
		status  int
		message string
	}{
		{"null", []string{"Create Procedure"}, [][]driver.Value{{nil}}, nil, 502, "unavailable"},
		{"empty", []string{"Create Procedure"}, [][]driver.Value{{""}}, nil, 502, "unavailable"},
		{"missing column", []string{"Procedure", "Create Function"}, [][]driver.Value{{"p", "SELECT 1"}}, nil, 502, "unavailable"},
		{"ambiguous column", []string{"Create Procedure", "create procedure"}, [][]driver.Value{{"one", "two"}}, nil, 502, "ambiguous"},
		{"missing row", []string{"Create Procedure"}, nil, nil, 404, "not found"},
		{"invalid utf8", []string{"Create Procedure"}, [][]driver.Value{{[]byte{0xff}}}, nil, 502, "UTF-8"},
		{"too large", []string{"Create Procedure"}, [][]driver.Value{{strings.Repeat("x", maxResultJSONBytes)}}, nil, 502, "size limit"},
		{"escaping expands", []string{"Create Procedure"}, [][]driver.Value{{strings.Repeat("<", maxResultJSONBytes/6)}}, nil, 502, "size limit"},
		{"database error", nil, nil, errors.New("private server details"), 502, "database operation failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeEngine{query: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
				return tc.columns, tc.rows, tc.err
			}}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.request(s, http.MethodGet, "/api/object/definition?database=db&name=p&kind=procedure", "")
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.message) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["definition"]; ok {
				t.Fatalf("failure returned editable definition: %#v", body)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
			if strings.Contains(rec.Body.String(), "private server details") {
				t.Fatal("leaked server error")
			}
		})
	}
}

func TestObjectDefinitionRequestValidation(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	for _, tc := range []struct {
		method, query string
		status        int
	}{
		{http.MethodPost, "database=db&name=p&kind=procedure", 405},
		{http.MethodGet, "database=&name=p&kind=procedure", 400},
		{http.MethodGet, "database=db&name=&kind=procedure", 400},
		{http.MethodGet, "database=db&name=p&kind=function", 400},
		{http.MethodGet, "database=db&name=p&kind=PROCEDURE", 400},
		{http.MethodGet, "database=db&name=%00&kind=event", 400},
	} {
		rec := b.request(s, tc.method, "/api/object/definition?"+tc.query, "")
		if rec.Code != tc.status || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s: status=%d headers=%v", tc.method, tc.query, rec.Code, rec.Header())
		}
	}
	if count := recordCount(engine); count != 0 {
		t.Fatalf("invalid requests performed %d database operations", count)
	}
	unconnected := newServer()
	browser := bootstrapTestBrowser(t, unconnected)
	rec := browser.request(unconnected, http.MethodGet, "/api/object/definition?database=db&name=p&kind=event", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("unconnected status %d", rec.Code)
	}
}

func TestObjectDefinitionAbortsUnreadStream(t *testing.T) {
	for _, columns := range [][]string{{"Create Procedure"}, {"unexpected"}} {
		client, peer := net.Pipe()
		limited := &boundedConn{Conn: client, remaining: 1024}
		budget := &connectionBudget{conn: limited}
		driverConn := &closeWaitDriverConn{rows: &closeWaitRows{peer: peer, columns: columns, values: []driver.Value{"CREATE PROCEDURE p() SELECT 1"}, total: 2}}
		db := sql.OpenDB(closeWaitConnector{&trackedMySQLConn{mysqlDriverConnection: driverConn, budget: budget}})
		db.SetMaxIdleConns(0)
		done := make(chan int, 1)
		go func() {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/object/definition?database=db&name=p&kind=procedure", nil)
			handleObjectDefinition(rec, req, &session{db: db})
			done <- rec.Code
		}()
		select {
		case status := <-done:
			if status != 502 {
				t.Errorf("unexpected status %d", status)
			}
		case <-time.After(time.Second):
			_ = limited.Close()
			<-done
			t.Error("Rows.Close attempted to drain unread data")
		}
		_ = db.Close()
		_ = peer.Close()
	}
}

func TestDefinitionJSONFitsExactEscaping(t *testing.T) {
	for _, value := range []string{"ascii", "\"\\\b\f\n\r\t\x00", "<>&", "🐈\u2028\u2029", strings.Repeat("<", 200)} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		// Backspace/formfeed can encode as \\u0008/\\u000c on older Go.
		backspace, _ := json.Marshal("\b")
		formfeed, _ := json.Marshal("\f")
		bound := len(encoded) + strings.Count(value, "\b")*(8-len(backspace)) + strings.Count(value, "\f")*(8-len(formfeed))
		used := maxResultJSONBytes - bound
		if !definitionJSONFits([]byte(value), used) || definitionJSONFits([]byte(value), used+1) {
			t.Fatalf("incorrect JSON boundary for %q", value)
		}
	}
	if definitionJSONFits([]byte{0xff}, 0) {
		t.Fatal("invalid UTF-8 accepted")
	}
}
