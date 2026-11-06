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
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

func TestSplitSQLStatements(t *testing.T) {
	routine := "DELIMITER //\nCREATE PROCEDURE p()\nBEGIN\n  SELECT 1; SELECT 2;\nEND//\nDELIMITER ;\nSELECT 3;"
	cases := []struct {
		name   string
		script string
		want   []string
	}{
		{"plain script", "SELECT 1; SELECT 2", []string{"SELECT 1", "SELECT 2"}},
		{"no trailing terminator", "SELECT 1", []string{"SELECT 1"}},
		{"semicolon inside string", "SELECT 'a;b'", []string{"SELECT 'a;b'"}},
		{"backslash-escaped quote", "SELECT 'it\\'s; fine'; SELECT 2", []string{"SELECT 'it\\'s; fine'", "SELECT 2"}},
		{"doubled quote", "SELECT 'a''b;c'; SELECT 2", []string{"SELECT 'a''b;c'", "SELECT 2"}},
		{"double-quoted string", "SELECT \"a;b\"; SELECT 2", []string{"SELECT \"a;b\"", "SELECT 2"}},
		{"backtick identifier", "SELECT `a;b` FROM t; SELECT 2", []string{"SELECT `a;b` FROM t", "SELECT 2"}},
		{"doubled backtick", "SELECT `a``;b` FROM t", []string{"SELECT `a``;b` FROM t"}},
		{"hash comment hides semicolon", "SELECT 1 # comment;\n; SELECT 2", []string{"SELECT 1 # comment;", "SELECT 2"}},
		{"dash comment with space", "SELECT 1 -- c;\n; SELECT 2", []string{"SELECT 1 -- c;", "SELECT 2"}},
		{"dash without space is not a comment", "SELECT 1--2; SELECT 3", []string{"SELECT 1--2", "SELECT 3"}},
		{"apostrophe inside comment", "SELECT 1 -- don't stop; here\n;", []string{"SELECT 1 -- don't stop; here"}},
		{"block comment hides semicolon", "/* a;b */ SELECT 1; SELECT 2", []string{"/* a;b */ SELECT 1", "SELECT 2"}},
		{"delimiter routine", routine, []string{
			"CREATE PROCEDURE p()\nBEGIN\n  SELECT 1; SELECT 2;\nEND", "SELECT 3",
		}},
		{"directive after comment", "-- header\nDELIMITER $$\nSELECT 1$$", []string{"SELECT 1"}},
		{"comment-only statements dropped", "SELECT 1; -- trailing\n/* another */;", []string{"SELECT 1"}},
		{"only separators", ";;;\n  \n; ;", nil},
		{"CRLF line endings", "SELECT 1;\r\nSELECT 2;\r\n", []string{"SELECT 1", "SELECT 2"}},
		{"unterminated string swallows rest", "SELECT 'abc", []string{"SELECT 'abc"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitSQLStatements(tt.script)
			if err != nil {
				t.Fatalf("split: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("split = %#v, want %#v", got, tt.want)
			}
			for index := range got {
				if got[index] != tt.want[index] {
					t.Fatalf("statement %d = %q, want %q", index, got[index], tt.want[index])
				}
			}
		})
	}
	if _, err := splitSQLStatements("DELIMITER\nSELECT 1;"); !errors.Is(err, errInvalidDelimiter) {
		t.Fatalf("empty DELIMITER = %v, want errInvalidDelimiter", err)
	}
	if _, err := splitSQLStatements(strings.Repeat("SELECT 1;", maxImportStatements+1)); !errors.Is(err, errTooManyStatements) {
		t.Fatalf("statement cap = %v, want errTooManyStatements", err)
	}
}

func TestImportSQLRunsWholeScriptOnOneConnection(t *testing.T) {
	engine := &fakeEngine{}
	engine.exec = func(query string, args []driver.NamedValue) (driver.Result, error) {
		if query != "USE `db`" || len(args) != 0 {
			return nil, fmt.Errorf("unexpected exec: %s", query)
		}
		return driver.RowsAffected(0), nil
	}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if len(args) != 0 {
			return nil, nil, errors.New("import statements must not carry parameters")
		}
		return []string{}, nil, nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := `{"database":"db","content":"CREATE TABLE a (id INT);\nINSERT INTO a VALUES (1);\nINSERT INTO a VALUES (2)","stop_on_error":false,"confirmed":true}`
	rec := b.mutation(s, "/api/import/sql", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var result importSQLResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("response: %v", err)
	}
	if result.Executed != 3 || result.Failed != 0 || result.Stopped || len(result.Errors) != 0 {
		t.Fatalf("import result: %+v", result)
	}
	if count := recordCount(engine); count != 4 {
		t.Fatalf("expected USE plus 3 statements, got %d", count)
	}
	for _, record := range engine.records {
		if record.conn != engine.records[0].conn {
			t.Fatal("import statements must share one fresh connection")
		}
	}
	if !engine.records[0].exec || engine.records[0].query != "USE `db`" {
		t.Fatalf("expected USE first: %+v", engine.records[0])
	}
	want := []string{"CREATE TABLE a (id INT)", "INSERT INTO a VALUES (1)", "INSERT INTO a VALUES (2)"}
	for index, statement := range want {
		record := engine.records[index+1]
		if record.exec || record.query != statement {
			t.Fatalf("statement %d = %+v, want query %q", index+1, record, statement)
		}
	}
}

func TestImportSQLRecordsStatementErrors(t *testing.T) {
	for _, stop := range []bool{true, false} {
		t.Run(fmt.Sprintf("stop_on_error=%v", stop), func(t *testing.T) {
			engine := &fakeEngine{}
			engine.exec = func(query string, _ []driver.NamedValue) (driver.Result, error) {
				if query != "USE `db`" {
					return nil, fmt.Errorf("unexpected exec: %s", query)
				}
				return driver.RowsAffected(0), nil
			}
			engine.query = func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
				if strings.HasPrefix(query, "SELECT bad") {
					return nil, nil, &mysql.MySQLError{Number: 1064, Message: "syntax"}
				}
				return []string{}, nil, nil
			}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			body := fmt.Sprintf(`{"database":"db","content":"SELECT good1;\nSELECT bad;\nSELECT good2","stop_on_error":%v,"confirmed":true}`, stop)
			rec := b.mutation(s, "/api/import/sql", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
			}
			var result importSQLResult
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatalf("response: %v", err)
			}
			wantExecuted, wantAttempts := 1, 2
			if !stop {
				wantExecuted, wantAttempts = 2, 3
			}
			if result.Executed != wantExecuted || result.Failed != 1 || result.Stopped != stop {
				t.Fatalf("import result: %+v", result)
			}
			if len(result.Errors) != 1 {
				t.Fatalf("errors: %+v", result.Errors)
			}
			entry := result.Errors[0]
			if entry.Index != 2 || entry.Preview != "SELECT bad" || entry.Error != "database error 1064" {
				t.Fatalf("error entry: %+v", entry)
			}
			if count := recordCount(engine); count != wantAttempts+1 {
				t.Fatalf("expected %d statements, ran %d", wantAttempts+1, count)
			}
		})
	}
}

func TestImportSQLPreviewIsBoundedAndValidUTF8(t *testing.T) {
	engine := &fakeEngine{}
	engine.exec = func(query string, _ []driver.NamedValue) (driver.Result, error) {
		return driver.RowsAffected(0), nil
	}
	engine.query = func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if len(query) > maxImportPreviewBytes {
			return nil, nil, &mysql.MySQLError{Number: 1064, Message: "syntax"}
		}
		return []string{}, nil, nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	long := "SELECT " + strings.Repeat("é", maxImportPreviewBytes)
	body := `{"database":"db","content":"` + long + `;","stop_on_error":false,"confirmed":true}`
	rec := b.mutation(s, "/api/import/sql", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var result importSQLResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("response: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("errors: %+v", result.Errors)
	}
	preview := result.Errors[0].Preview
	if len(preview) > maxImportPreviewBytes || !utf8.ValidString(preview) || !strings.HasPrefix(preview, "SELECT é") {
		t.Fatalf("preview invalid: %q", preview)
	}
}

func TestImportSQLTransportFailureAborts(t *testing.T) {
	engine := &fakeEngine{}
	engine.exec = func(query string, _ []driver.NamedValue) (driver.Result, error) {
		return nil, errors.New("connection broken")
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := `{"database":"db","content":"SELECT 1; SELECT 2","stop_on_error":false,"confirmed":true}`
	rec := b.mutation(s, "/api/import/sql", body)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "database operation failed") {
		t.Fatalf("transport failure: %d %s", rec.Code, rec.Body.String())
	}
	if count := recordCount(engine); count != 1 {
		t.Fatalf("statements attempted after a broken connection: %d", count)
	}
}

// mutationWithContentType posts a mutation body with an explicit Content-Type.
func mutationWithContentType(s *server, b testBrowser, path, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080"+path, strings.NewReader(body))
	req.AddCookie(b.cookie)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-CSRF-Token", b.token)
	req.Header.Set("X-DBKid-Request-ID", "0123456789abcdef0123456789abcdef")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestImportSQLLimits(t *testing.T) {
	oversized, _ := json.Marshal(importSQLRequest{Database: "db", Content: strings.Repeat("a", maxImportContentBytes+1), Confirmed: true})
	many, _ := json.Marshal(importSQLRequest{Database: "db", Content: strings.Repeat("SELECT 1;", maxImportStatements+1), Confirmed: true})
	cases := []struct {
		name    string
		body    string
		status  int
		message string
	}{
		{"oversized content", string(oversized), http.StatusRequestEntityTooLarge, "import file is too large"},
		{"too many statements", string(many), http.StatusBadRequest, "too many statements"},
		{"empty content", `{"database":"db","content":"","stop_on_error":false,"confirmed":true}`, http.StatusBadRequest, "import content is required"},
		{"whitespace content", `{"database":"db","content":"   ","stop_on_error":false,"confirmed":true}`, http.StatusBadRequest, "import content is required"},
		{"invalid database", `{"database":"","content":"SELECT 1","stop_on_error":false,"confirmed":true}`, http.StatusBadRequest, "a valid database name is required"},
		{"wrong content type", `{"database":"db","content":"SELECT 1","stop_on_error":false,"confirmed":true}`, http.StatusUnsupportedMediaType, "application/json is required"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			engine := &fakeEngine{}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			var rec *httptest.ResponseRecorder
			if tt.name == "wrong content type" {
				rec = mutationWithContentType(s, b, "/api/import/sql", "text/plain", tt.body)
			} else {
				rec = b.mutationWithID(s, "/api/import/sql", tt.body, "0123456789abcdef0123456789abcdef")
			}
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.message) {
				t.Fatalf("response = %d %s, want %d containing %q", rec.Code, rec.Body.String(), tt.status, tt.message)
			}
			if count := recordCount(engine); count != 0 {
				t.Fatalf("rejected import reached the database %d times", count)
			}
		})
	}
}

func TestImportCSVExactValuesAndMapping(t *testing.T) {
	columns := []*string{strPointer("id"), strPointer("label"), strPointer("note")}
	content := strings.Join([]string{
		"id;label;note",
		"1;Alice;\"quoted;field\"",
		"2;Bob;",
		"3;\"O\"\"Reilly\";\"multi\nline\"",
	}, "\n")
	engine := metaEngine(t, "items", nil)
	engine.exec = func(query string, args []driver.NamedValue) (driver.Result, error) {
		if strings.HasPrefix(query, "USE ") {
			return driver.RowsAffected(0), nil
		}
		if !strings.HasPrefix(query, "INSERT INTO") {
			return nil, fmt.Errorf("unexpected mutation: %s", query)
		}
		return driver.RowsAffected(int64(strings.Count(query, "),(") + 1)), nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	payload, err := json.Marshal(importCSVRequest{
		Database: "db", Table: "items", Content: content,
		HasHeader: true, Delimiter: ";", Quote: "\"", Columns: columns, Confirmed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := b.mutation(s, "/api/import/csv", string(payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("csv import: %d %s", rec.Code, rec.Body.String())
	}
	var result importCSVResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("response: %v", err)
	}
	if result.Inserted != 3 || result.Failed != 0 || result.Stopped || len(result.Errors) != 0 {
		t.Fatalf("csv result: %+v", result)
	}
	// records: metadata query, USE exec, insert exec — all on one connection
	if count := recordCount(engine); count != 3 {
		t.Fatalf("expected metadata, USE and insert, got %d records", count)
	}
	want := "INSERT INTO `db`.`items` (`id`,`label`,`note`) VALUES (?,?,?),(?,?,?),(?,?,?)"
	insert := engine.records[len(engine.records)-1]
	if insert.exec != true || insert.query != want {
		t.Fatalf("insert = %q, want %q", insert.query, want)
	}
	args := recordArgs(insert)
	expected := []interface{}{"1", "Alice", "quoted;field", "2", "Bob", nil, "3", "O\"Reilly", "multi\nline"}
	if len(args) != len(expected) {
		t.Fatalf("args = %#v", args)
	}
	for index := range expected {
		if args[index] != expected[index] {
			t.Fatalf("arg %d = %#v, want %#v", index, args[index], expected[index])
		}
	}
}

func TestImportCSVColumnMappingRules(t *testing.T) {
	id, name := strPointer("id"), strPointer("name")
	cases := []struct {
		name    string
		body    importCSVRequest
		status  int
		message string
	}{
		{"skipped fields are not inserted", importCSVRequest{
			Database: "db", Table: "items", Content: "1;skip;A\n2;skip;B\n",
			Delimiter: ";", Quote: "\"", Columns: []*string{id, nil, strPointer("note")}, Confirmed: true,
		}, http.StatusOK, ""},
		{"unknown mapped column", importCSVRequest{
			Database: "db", Table: "items", Content: "1;a\n",
			Delimiter: ";", Quote: "\"", Columns: []*string{id, strPointer("bogus")}, Confirmed: true,
		}, http.StatusBadRequest, "unknown column"},
		{"duplicate mapped column", importCSVRequest{
			Database: "db", Table: "items", Content: "1;a;b\n",
			Delimiter: ";", Quote: "\"", Columns: []*string{id, id, strPointer("note")}, Confirmed: true,
		}, http.StatusBadRequest, "duplicate column mapping"},
		{"mapping length mismatch", importCSVRequest{
			Database: "db", Table: "items", Content: "1;a;b\n",
			Delimiter: ";", Quote: "\"", Columns: []*string{id, name}, Confirmed: true,
		}, http.StatusBadRequest, "inconsistent csv columns"},
		{"inconsistent rows", importCSVRequest{
			Database: "db", Table: "items", Content: "1;a\n2;a;b\n",
			Delimiter: ";", Quote: "\"", Columns: []*string{id, name, strPointer("note")}, Confirmed: true,
		}, http.StatusBadRequest, "inconsistent csv columns"},
		{"no mapped columns with data", importCSVRequest{
			Database: "db", Table: "items", Content: "1;2\n",
			Delimiter: ";", Quote: "\"", Columns: []*string{nil, nil}, Confirmed: true,
		}, http.StatusBadRequest, "a column mapping is required"},
		{"invalid delimiter", importCSVRequest{
			Database: "db", Table: "items", Content: "1;a\n",
			Delimiter: ":", Quote: "\"", Columns: []*string{id, name}, Confirmed: true,
		}, http.StatusBadRequest, "invalid csv delimiter"},
		{"two-character delimiter", importCSVRequest{
			Database: "db", Table: "items", Content: "1;;a\n",
			Delimiter: ";;", Quote: "\"", Columns: []*string{id, name}, Confirmed: true,
		}, http.StatusBadRequest, "invalid csv delimiter"},
		{"invalid quote", importCSVRequest{
			Database: "db", Table: "items", Content: "1;a\n",
			Delimiter: ";", Quote: "x", Columns: []*string{id, name}, Confirmed: true,
		}, http.StatusBadRequest, "invalid csv quote"},
		{"empty content", importCSVRequest{
			Database: "db", Table: "items", Content: "\n\n",
			Delimiter: ";", Quote: "\"", Columns: []*string{id, name}, Confirmed: true,
		}, http.StatusBadRequest, "import content is required"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			engine := metaEngine(t, "items", nil)
			engine.exec = func(query string, _ []driver.NamedValue) (driver.Result, error) {
				if tt.status == http.StatusOK {
					return driver.RowsAffected(int64(strings.Count(query, "),(") + 1)), nil
				}
				return nil, fmt.Errorf("rejected csv import reached the database: %s", query)
			}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			payload, err := json.Marshal(tt.body)
			if err != nil {
				t.Fatal(err)
			}
			rec := b.mutation(s, "/api/import/csv", string(payload))
			if tt.status == http.StatusOK {
				if rec.Code != http.StatusOK {
					t.Fatalf("csv import: %d %s", rec.Code, rec.Body.String())
				}
				insert := engine.records[len(engine.records)-1]
				if insert.query != "INSERT INTO `db`.`items` (`id`,`note`) VALUES (?,?),(?,?)" {
					t.Fatalf("mapped insert = %q", insert.query)
				}
				if args := recordArgs(insert); len(args) != 4 || args[0] != "1" || args[1] != "A" || args[2] != "2" || args[3] != "B" {
					t.Fatalf("mapped args = %#v", args)
				}
				return
			}
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.message) {
				t.Fatalf("response = %d %s, want %d containing %q", rec.Code, rec.Body.String(), tt.status, tt.message)
			}
			for _, record := range engine.records {
				if strings.HasPrefix(record.query, "INSERT INTO") {
					t.Fatalf("rejected csv import built an insert: %q", record.query)
				}
			}
		})
	}
}

func TestImportCSVEmptyUnquotedFieldsFollowNullability(t *testing.T) {
	// The items fixture's note column is nullable: an unquoted empty field
	// becomes NULL while a quoted empty field stays the empty string.
	engine := metaEngine(t, "items", nil)
	engine.exec = func(query string, _ []driver.NamedValue) (driver.Result, error) {
		if strings.HasPrefix(query, "USE ") {
			return driver.RowsAffected(0), nil
		}
		return driver.RowsAffected(2), nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	payload, _ := json.Marshal(importCSVRequest{
		Database: "db", Table: "items", Content: "1;\n2;''\n",
		Delimiter: ";", Quote: "'", Columns: []*string{strPointer("id"), strPointer("note")}, Confirmed: true,
	})
	rec := b.mutation(s, "/api/import/csv", string(payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("csv import: %d %s", rec.Code, rec.Body.String())
	}
	insert := engine.records[len(engine.records)-1]
	if insert.query != "INSERT INTO `db`.`items` (`id`,`note`) VALUES (?,?),(?,?)" {
		t.Fatalf("insert = %q", insert.query)
	}
	args := recordArgs(insert)
	if len(args) != 4 || args[0] != "1" || args[1] != nil || args[2] != "2" || args[3] != "" {
		t.Fatalf("empty-field args = %#v, want [1 nil 2 \"\"]", args)
	}
}

func TestImportCSVBatchesAndBatchError(t *testing.T) {
	t.Run("batches of one hundred", func(t *testing.T) {
		engine := metaEngine(t, "items", nil)
		engine.exec = func(query string, args []driver.NamedValue) (driver.Result, error) {
			if strings.HasPrefix(query, "USE ") {
				return driver.RowsAffected(0), nil
			}
			return driver.RowsAffected(int64(strings.Count(query, "),(") + 1)), nil
		}
		s := newServer()
		b := connectFakeDatabase(t, s, engine)
		var lines []string
		for index := 1; index <= 250; index++ {
			lines = append(lines, fmt.Sprintf("%d;row-%d;", index, index))
		}
		payload, _ := json.Marshal(importCSVRequest{
			Database: "db", Table: "items", Content: strings.Join(lines, "\n"),
			Delimiter: ";", Quote: "\"",
			Columns:   []*string{strPointer("id"), strPointer("label"), strPointer("note")},
			Confirmed: true,
		})
		rec := b.mutation(s, "/api/import/csv", string(payload))
		if rec.Code != http.StatusOK {
			t.Fatalf("csv import: %d %s", rec.Code, rec.Body.String())
		}
		var result importCSVResult
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Inserted != 250 ||
			result.Failed != 0 || result.Stopped || len(result.Errors) != 0 {
			t.Fatalf("csv result: %+v (%v)", result, err)
		}
		var inserts int
		for _, record := range engine.records {
			if strings.HasPrefix(record.query, "INSERT INTO") {
				inserts++
				expected := []int{100, 100, 50}[inserts-1]
				if tuples := strings.Count(record.query, "),(") + 1; tuples != expected {
					t.Fatalf("batch %d has %d tuples, want %d", inserts, tuples, expected)
				}
			}
		}
		if inserts != 3 {
			t.Fatalf("expected 3 batches, got %d", inserts)
		}
	})
	t.Run("batch error reports first row and stops", func(t *testing.T) {
		engine := metaEngine(t, "items", nil)
		engine.exec = func(query string, args []driver.NamedValue) (driver.Result, error) {
			if strings.HasPrefix(query, "USE ") {
				return driver.RowsAffected(0), nil
			}
			return nil, &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
		}
		s := newServer()
		b := connectFakeDatabase(t, s, engine)
		payload, _ := json.Marshal(importCSVRequest{
			Database: "db", Table: "items", Content: "1;a\n2;b\n3;c\n",
			Delimiter: ";", Quote: "\"",
			Columns:   []*string{strPointer("id"), strPointer("label")},
			Confirmed: true,
		})
		rec := b.mutation(s, "/api/import/csv", string(payload))
		if rec.Code != http.StatusOK {
			t.Fatalf("csv import: %d %s", rec.Code, rec.Body.String())
		}
		var result importCSVResult
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatalf("response: %v", err)
		}
		if result.Inserted != 0 || result.Failed != 3 || !result.Stopped {
			t.Fatalf("csv result: %+v", result)
		}
		if len(result.Errors) != 1 || result.Errors[0].Row != 1 || result.Errors[0].Error != "database error 1062" {
			t.Fatalf("csv errors: %+v", result.Errors)
		}
	})
}

func TestImportCSVRowAndSizeLimits(t *testing.T) {
	var lines []string
	for index := 0; index <= maxCSVImportRows; index++ {
		lines = append(lines, fmt.Sprintf("%d", index))
	}
	tooMany, _ := json.Marshal(importCSVRequest{
		Database: "db", Table: "items", Content: strings.Join(lines, "\n"),
		Delimiter: ",", Quote: "\"", Columns: []*string{strPointer("id")}, Confirmed: true,
	})
	oversized, _ := json.Marshal(importCSVRequest{
		Database: "db", Table: "items", Content: strings.Repeat("a", maxImportContentBytes+1),
		Delimiter: ",", Quote: "\"", Columns: []*string{strPointer("id")}, Confirmed: true,
	})
	cases := []struct {
		name, body, message string
		status              int
	}{
		{"too many rows", string(tooMany), "too many rows", http.StatusBadRequest},
		{"oversized content", string(oversized), "import file is too large", http.StatusRequestEntityTooLarge},
		{"missing table", `{"database":"db","table":"","content":"1\n","delimiter":",","quote":"\"","columns":["id"],"confirmed":true}`, "database and table names are required", http.StatusBadRequest},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			engine := metaEngine(t, "items", nil)
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.mutation(s, "/api/import/csv", tt.body)
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.message) {
				t.Fatalf("response = %d %s, want %d containing %q", rec.Code, rec.Body.String(), tt.status, tt.message)
			}
			if count := recordCount(engine); count != 0 {
				t.Fatalf("rejected import reached the database %d times", count)
			}
		})
	}
}

func strPointer(value string) *string {
	return &value
}
