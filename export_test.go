package main

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSQLValueLiteralEscaping(t *testing.T) {
	cases := []struct {
		dataType string
		value    interface{}
		want     string
	}{
		{"varchar", []byte(`O'Reilly`), `'O\'Reilly'`},
		{"varchar", []byte(`a\b`), `'a\\b'`},
		{"varchar", []byte(`say "hi"`), `'say "hi"'`},
		{"varchar", []byte(""), `''`},
		{"text", nil, `NULL`},
		{"int", nil, `NULL`},
		{"int", []byte("10"), `10`},
		{"int", int64(-42), `-42`},
		{"bigint", uint64(18446744073709551615), `18446744073709551615`},
		{"decimal", []byte("12.50"), `12.50`},
		{"double", []byte("3.14e10"), `3.14e10`},
		{"float", []byte("-2.5E-3"), `-2.5E-3`},
		{"int", []byte("12ab"), `'12ab'`},
		{"decimal", []byte(""), `''`},
		{"varchar", []byte("10"), `'10'`},
		{"varbinary", []byte{0xFF, 0x00}, `0xff00`},
		{"blob", []byte{}, `0x`},
		{"datetime", []byte("2026-01-02 03:04:05"), `'2026-01-02 03:04:05'`},
		{"timestamp", []byte("2026-01-02 03:04:05.123"), `'2026-01-02 03:04:05.123'`},
		{"date", []byte("2026-01-02"), `'2026-01-02'`},
		{"time", []byte("10:11:12"), `'10:11:12'`},
		{"year", []byte("2026"), `'2026'`},
		{"json", []byte(`{"a":1}`), `'{"a":1}'`},
		{"enum", []byte("active"), `'active'`},
	}
	for _, test := range cases {
		if got := sqlValueLiteral(test.dataType, test.value); got != test.want {
			t.Errorf("sqlValueLiteral(%s, %#v) = %q, want %q", test.dataType, test.value, got, test.want)
		}
	}
	if got := sqlValueLiteral("varchar", nil); got != "NULL" {
		t.Errorf("nil value must be NULL, got %q", got)
	}
}

func TestValidNumberLiteral(t *testing.T) {
	valid := []string{"1", "-1", "+1", "1.5", "-1.5", ".5", "5.", "1e10", "1E10", "-1.5e-3", "007"}
	invalid := []string{"", "1.2.3", "abc", "1e", "1e+", "--1", "1a", ".", "1,5", "NaN", "Infinity"}
	for _, text := range valid {
		if !validNumberLiteral(text) {
			t.Errorf("validNumberLiteral(%q) = false", text)
		}
	}
	for _, text := range invalid {
		if validNumberLiteral(text) {
			t.Errorf("validNumberLiteral(%q) = true", text)
		}
	}
}

func TestCSVEscaping(t *testing.T) {
	cases := []struct {
		dataType string
		value    interface{}
		want     string
	}{
		{"varchar", []byte("hello"), `"hello"`},
		{"varchar", []byte(`say "hi"`), `"say ""hi"""`},
		{"varchar", []byte("a,b"), `"a,b"`},
		{"varchar", []byte("line1\nline2"), "\"line1\nline2\""},
		{"varchar", []byte("cr\r\nlf"), "\"cr\r\nlf\""},
		{"varchar", []byte(""), `""`},
		{"varchar", nil, ``},
		{"int", nil, ``},
		{"int", []byte("10"), `"10"`},
		{"varbinary", []byte{0xFF, 0x00}, `0xff00`},
		{"datetime", []byte("2026-01-02 03:04:05"), `"2026-01-02 03:04:05"`},
	}
	for _, test := range cases {
		if got := csvCell(test.dataType, test.value); got != test.want {
			t.Errorf("csvCell(%s, %#v) = %q, want %q", test.dataType, test.value, got, test.want)
		}
	}
	if got := csvQuoteField(`a"b`); got != `"a""b"` {
		t.Errorf("csvQuoteField doubling wrong: %q", got)
	}
}

// exportEngine fakes a database whose items table holds the rows used by the
// export flow tests.
func exportEngine(t *testing.T, itemsRows [][]driver.Value) *fakeEngine {
	t.Helper()
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS"):
			return columnsFixture("items")
		case strings.HasPrefix(query, "SHOW CREATE TABLE `db`.`items`"):
			return []string{"Table", "Create Table"}, [][]driver.Value{{"items", "CREATE TABLE `items` (\n  `id` int NOT NULL PRIMARY KEY\n) ENGINE=InnoDB"}}, nil
		case query == "SELECT * FROM `db`.`items`":
			return []string{"id", "label", "quantity", "note", "raw_data"}, itemsRows, nil
		case query == "SELECT * FROM `db`.`items` ORDER BY `id`":
			return []string{"id", "label", "quantity", "note", "raw_data"}, itemsRows, nil
		case strings.HasPrefix(query, "SELECT * FROM `db`.`"):
			return []string{"id", "label", "quantity", "note", "raw_data"}, itemsRows, nil
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	return engine
}

func exportItemsRows() [][]driver.Value {
	return [][]driver.Value{
		{int64(1), []byte("Alpha"), int64(10), nil, []byte{0xFF, 0x00}},
		{int64(2), []byte("O'Reilly"), int64(0), []byte(""), []byte{0xE2, 0x82, 0xAC}},
		{int64(3), []byte("<img src=x onerror=alert(1)>"), int64(30), []byte("plain text"), nil},
	}
}

func requestExportToken(t *testing.T, s *server, b testBrowser, body string) string {
	t.Helper()
	rec := b.request(s, http.MethodPost, "/api/export/token", body)
	var issued struct {
		URL string `json:"url"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &issued) != nil || !strings.HasPrefix(issued.URL, "/api/export?token=") {
		t.Fatalf("export token issuance failed: %d %s", rec.Code, rec.Body.String())
	}
	return issued.URL
}

func getDownload(s *server, b testBrowser, url string) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodGet, "http://localhost:8080"+url, nil)
	req.AddCookie(b.cookie)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	return recorder.Result(), recorder.Body.String()
}

func TestExportCSVFlow(t *testing.T) {
	engine := exportEngine(t, exportItemsRows())
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	url := requestExportToken(t, s, b, `{"database":"db","table":"items","format":"csv","what":"data"}`)
	response, body := getDownload(s, b, url)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("csv export failed: %d %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Fatalf("csv content type wrong: %q", got)
	}
	if got := response.Header.Get("Content-Disposition"); got != `attachment; filename="items.csv"` {
		t.Fatalf("csv disposition wrong: %q", got)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("export must not be cacheable")
	}
	lines := strings.Split(body, "\n")
	if len(lines) != 5 || lines[4] != "" {
		t.Fatalf("csv row count wrong: %q", body)
	}
	if lines[0] != `"id","label","quantity","note","raw_data"` {
		t.Fatalf("csv header wrong: %q", lines[0])
	}
	if lines[1] != `"1","Alpha","10",,0xff00` {
		t.Fatalf("csv first row wrong: %q", lines[1])
	}
	if lines[2] != `"2","O'Reilly","0","",0xe282ac` {
		t.Fatalf("csv second row wrong: %q", lines[2])
	}
	if lines[3] != `"3","<img src=x onerror=alert(1)>","30","plain text",` {
		t.Fatalf("csv third row wrong: %q", lines[3])
	}
	// The token is single-use.
	response, body = getDownload(s, b, url)
	if response.StatusCode != http.StatusForbidden || !strings.Contains(body, "invalid or expired download token") {
		t.Fatalf("csv export replay must 403, got %d %s", response.StatusCode, body)
	}
}

func TestExportSQLFlowForTable(t *testing.T) {
	engine := exportEngine(t, exportItemsRows())
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	url := requestExportToken(t, s, b, `{"database":"db","table":"items","format":"sql","what":"both"}`)
	response, body := getDownload(s, b, url)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sql export failed: %d %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != "application/sql; charset=utf-8" {
		t.Fatalf("sql content type wrong: %q", got)
	}
	if got := response.Header.Get("Content-Disposition"); got != `attachment; filename="items.sql"` {
		t.Fatalf("sql disposition wrong: %q", got)
	}
	if !strings.Contains(body, "-- DBKid export db 2") || !strings.Contains(body, "SET NAMES utf8mb4;") {
		t.Fatalf("sql header comment missing: %q", body)
	}
	if !strings.Contains(body, "-- DROP TABLE IF EXISTS `items`;") {
		t.Fatalf("commented drop line missing: %q", body)
	}
	if !strings.Contains(body, "CREATE TABLE `items` (") || !strings.Contains(body, ") ENGINE=InnoDB;") {
		t.Fatalf("create table statement missing: %q", body)
	}
	if !strings.Contains(body, "INSERT INTO `items` VALUES (1,'Alpha',10,NULL,0xff00),(2,'O"+`\'`+"Reilly',0,'',0xe282ac),(3,'<img src=x onerror=alert(1)>',30,'plain text',NULL);") {
		t.Fatalf("insert statement wrong: %q", body)
	}
	if strings.Contains(body, "TRUNCATED") {
		t.Fatalf("untruncated export must not carry the marker: %q", body)
	}
}

func TestExportSQLStructureAndDataOnlySelections(t *testing.T) {
	engine := exportEngine(t, exportItemsRows())
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	url := requestExportToken(t, s, b, `{"database":"db","table":"items","format":"sql","what":"structure"}`)
	_, body := getDownload(s, b, url)
	if !strings.Contains(body, "CREATE TABLE `items` (") || strings.Contains(body, "INSERT INTO") ||
		strings.Contains(body, "DROP TABLE") {
		t.Fatalf("structure-only export wrong: %q", body)
	}
	url = requestExportToken(t, s, b, `{"database":"db","table":"items","format":"sql","what":"data"}`)
	_, body = getDownload(s, b, url)
	if strings.Contains(body, "CREATE TABLE") || !strings.Contains(body, "INSERT INTO `items` VALUES") ||
		strings.Contains(body, "DROP TABLE") {
		t.Fatalf("data-only export wrong: %q", body)
	}
}

func TestExportSQLDatabaseIncludesAllTables(t *testing.T) {
	engine := exportEngine(t, exportItemsRows())
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS"):
			return columnsFixture("items")
		case strings.Contains(query, "FROM INFORMATION_SCHEMA.TABLES"):
			if len(args) != 1 || args[0].Value != "db" {
				return nil, nil, errors.New("unexpected table listing arguments")
			}
			return []string{"TABLE_NAME"}, [][]driver.Value{{"items"}, {"second"}}, nil
		case strings.HasPrefix(query, "SHOW CREATE TABLE "):
			return []string{"Table", "Create Table"}, [][]driver.Value{{"t", "CREATE TABLE `second` (`id` int)"}}, nil
		case strings.HasPrefix(query, "SELECT * FROM "):
			return []string{"id", "label", "quantity", "note", "raw_data"}, exportItemsRows(), nil
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	url := requestExportToken(t, s, b, `{"database":"db","format":"sql","what":"both"}`)
	response, body := getDownload(s, b, url)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("database export failed: %d %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Disposition"); got != `attachment; filename="db.sql"` {
		t.Fatalf("database export disposition wrong: %q", got)
	}
	if !strings.Contains(body, "INSERT INTO `items` VALUES") || !strings.Contains(body, "INSERT INTO `second` VALUES") {
		t.Fatalf("database export missing tables: %q", body)
	}
}

func TestExportSQLBatchesInsertsEveryHundredRows(t *testing.T) {
	rows := make([][]driver.Value, 250)
	for i := range rows {
		rows[i] = []driver.Value{int64(i + 1), []byte(fmt.Sprintf("label-%d", i+1)), int64(i), nil, nil}
	}
	engine := exportEngine(t, rows)
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	url := requestExportToken(t, s, b, `{"database":"db","table":"items","format":"sql","what":"data"}`)
	_, body := getDownload(s, b, url)
	count := strings.Count(body, "INSERT INTO `items` VALUES")
	if count != 3 {
		t.Fatalf("250 rows must produce 3 INSERT statements, got %d", count)
	}
	if strings.Count(body, "(") < 250 {
		t.Fatalf("export lost rows: %d tuples", strings.Count(body, "("))
	}
}

func TestExportTruncatesAtRowCapWithMarker(t *testing.T) {
	rows := make([][]driver.Value, exportRowCap+5)
	for i := range rows {
		rows[i] = []driver.Value{int64(i + 1), []byte("x"), int64(0), nil, nil}
	}
	engine := exportEngine(t, rows)
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	url := requestExportToken(t, s, b, `{"database":"db","table":"items","format":"csv","what":"data"}`)
	response, body := getDownload(s, b, url)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("truncated csv export failed: %d %s", response.StatusCode, body)
	}
	if response.Header.Get("X-DBKid-Truncated") != "true" {
		t.Fatal("truncation header missing")
	}
	if !strings.HasSuffix(strings.TrimSuffix(body, "\n"), "-- TRUNCATED") {
		t.Fatalf("csv truncation marker missing at end: %q", body[len(body)-40:])
	}
	lines := strings.Split(body, "\n")
	// header + 50000 data rows + marker line + trailing empty split part
	if len(lines) != exportRowCap+3 {
		t.Fatalf("truncated csv must stop at %d rows, got %d lines", exportRowCap, len(lines)-3)
	}

	url = requestExportToken(t, s, b, `{"database":"db","table":"items","format":"sql","what":"both"}`)
	response, body = getDownload(s, b, url)
	if response.Header.Get("X-DBKid-Truncated") != "true" || !strings.HasSuffix(strings.TrimSuffix(body, "\n"), "-- TRUNCATED BY DBKID") {
		t.Fatalf("sql truncation marker missing: %q", body[len(body)-40:])
	}
}

func TestExportTokenRejectsInvalidRequests(t *testing.T) {
	engine := exportEngine(t, nil)
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{"bad format", `{"database":"db","table":"items","format":"xlsx","what":"data"}`, "unsupported export format"},
		{"bad what", `{"database":"db","table":"items","format":"csv","what":"everything"}`, "unsupported export selection"},
		{"csv without table", `{"database":"db","format":"csv","what":"data"}`, "a table is required for CSV export"},
		{"sort for sql", `{"database":"db","table":"items","format":"sql","what":"data","sort":"id","dir":"asc"}`, "invalid sort or filter"},
		{"filter for whole database", `{"database":"db","format":"sql","what":"data","filter":"[\"x\"]"}`, "invalid sort or filter"},
		{"bad sort column", `{"database":"db","table":"items","format":"csv","what":"data","sort":"nope","dir":"asc"}`, "invalid sort or filter"},
		{"bad filter column", `{"database":"db","table":"items","format":"csv","what":"data","filter":"[{\"column\":\"nope\",\"op\":\"=\",\"value\":\"1\"}]"}`, "invalid sort or filter"},
		{"missing database", `{"table":"items","format":"csv","what":"data"}`, "database and table names are required"},
		{"unknown field", `{"database":"db","table":"items","format":"csv","what":"data","evil":1}`, "invalid JSON object"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rec := b.request(s, http.MethodPost, "/api/export/token", test.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), test.message) {
				t.Fatalf("expected 400 %q, got %d %s", test.message, rec.Code, rec.Body.String())
			}
		})
	}
	if rec := b.request(s, http.MethodGet, "/api/export?database=db", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("export without token must 403, got %d", rec.Code)
	}
	if rec := b.request(s, http.MethodGet, "/api/export/token?database=db", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("export token GET must 405, got %d", rec.Code)
	}
}

func TestExportCSVHonoursSortAndFilter(t *testing.T) {
	engine := exportEngine(t, exportItemsRows())
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := `{"database":"db","table":"items","format":"csv","what":"data","sort":"id","dir":"desc","filter":"[{\"column\":\"id\",\"op\":\">=\",\"value\":\"2\"}]"}`
	url := requestExportToken(t, s, b, body)
	response, csvBody := getDownload(s, b, url)
	if response.StatusCode != http.StatusOK || !strings.Contains(csvBody, `"id"`) {
		t.Fatalf("sorted csv export failed: %d %s", response.StatusCode, csvBody)
	}
	engine.mu.Lock()
	var selectQuery string
	for _, record := range engine.records {
		if strings.HasPrefix(record.query, "SELECT * FROM `db`.`items`") {
			selectQuery = record.query
		}
	}
	engine.mu.Unlock()
	if selectQuery != "SELECT * FROM `db`.`items` WHERE (`id` >= ?) ORDER BY `id` DESC" {
		t.Fatalf("csv export query wrong: %q", selectQuery)
	}
}

func TestExportWireBudgetUses64MiBContext(t *testing.T) {
	// The handler must set the 64 MiB wire limit before opening its
	// connection; assert the context value plumbing directly.
	ctx := exportContext(context.Background())
	limit, ok := ctx.Value(wireLimitKey{}).(int64)
	if !ok || limit != int64(64<<20) {
		t.Fatalf("export wire limit context wrong: %d %v", limit, ok)
	}
	if limit == int64(maxIncomingBytes) {
		t.Fatal("export must override the default 8 MiB wire budget")
	}
}
