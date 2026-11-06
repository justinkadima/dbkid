package main

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Fake INFORMATION_SCHEMA.COLUMNS metadata for a table shaped like the
// integration fixture's items table, plus a binary-keyed variant.
func columnsFixture(table string) ([]string, [][]driver.Value, error) {
	switch table {
	case "items":
		return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, [][]driver.Value{
			{"id", "int", "int", "NO", "PRI", nil, ""},
			{"label", "varchar", "varchar(255)", "NO", "", nil, ""},
			{"quantity", "int", "int", "NO", "", nil, ""},
			{"note", "text", "text", "YES", "", nil, ""},
			{"raw_data", "varbinary", "varbinary(20)", "YES", "", nil, ""},
		}, nil
	case "nokey":
		return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, [][]driver.Value{
			{"value", "varchar", "varchar(255)", "NO", "", nil, ""},
		}, nil
	case "binkey":
		return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, [][]driver.Value{
			{"id", "varbinary", "varbinary(16)", "NO", "PRI", nil, ""},
			{"data", "mediumblob", "mediumblob", "YES", "", nil, ""},
		}, nil
	default:
		return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, nil, nil
	}
}

// metaEngine answers the INFORMATION_SCHEMA.COLUMNS lookup for the given
// table and fails loudly on any other statement unless overridden.
func metaEngine(t *testing.T, table string, selectResult func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error)) *fakeEngine {
	t.Helper()
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS") {
			if len(args) != 2 || args[0].Value != "db" || args[1].Value != table {
				return nil, nil, errors.New("unexpected columns lookup arguments")
			}
			return columnsFixture(table)
		}
		if selectResult != nil {
			return selectResult(query, args)
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	return engine
}

func browsePath(database, table string, extra url.Values) string {
	values := url.Values{}
	for key, list := range extra {
		for _, item := range list {
			values.Add(key, item)
		}
	}
	values.Set("database", database)
	values.Set("table", table)
	return "/api/rows?" + values.Encode()
}

func lastRecordQuery(engine *fakeEngine) string {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.records) == 0 {
		return ""
	}
	return engine.records[len(engine.records)-1].query
}

func recordCount(engine *fakeEngine) int {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return len(engine.records)
}

func TestBrowseRowsV2SortAndFilterSQL(t *testing.T) {
	cases := []struct {
		name      string
		extra     url.Values
		wantQuery string
		wantArgs  []driver.Value
		invalid   bool
		// metadataQueries: how many INFORMATION_SCHEMA lookups an invalid
		// case may reach (0 = rejected before any connection).
		metadataQueries int
	}{
		{
			name:      "sort ascending",
			extra:     url.Values{"sort": {"id"}, "dir": {"asc"}},
			wantQuery: "SELECT * FROM `db`.`items` ORDER BY `id` ASC LIMIT ? OFFSET ?",
			wantArgs:  []driver.Value{int64(51), int64(0)},
		},
		{
			name:      "sort descending page two",
			extra:     url.Values{"sort": {"id"}, "dir": {"desc"}, "page": {"2"}, "page_size": {"25"}},
			wantQuery: "SELECT * FROM `db`.`items` ORDER BY `id` DESC LIMIT ? OFFSET ?",
			wantArgs:  []driver.Value{int64(26), int64(25)},
		},
		{
			name:      "filter equals",
			extra:     url.Values{"filter": {`[{"column":"id","op":"=","value":"1"}]`}},
			wantQuery: "SELECT * FROM `db`.`items` WHERE (`id` = ?) LIMIT ? OFFSET ?",
			wantArgs:  []driver.Value{"1", int64(51), int64(0)},
		},
		{
			name:      "filter like wraps value",
			extra:     url.Values{"filter": {`[{"column":"label","op":"like","value":"O'Re"}]`}},
			wantQuery: "SELECT * FROM `db`.`items` WHERE (`label` LIKE ?) LIMIT ? OFFSET ?",
			wantArgs:  []driver.Value{"%O'Re%", int64(51), int64(0)},
		},
		{
			name:      "filter not like",
			extra:     url.Values{"filter": {`[{"column":"label","op":"not like","value":"x"}]`}},
			wantQuery: "SELECT * FROM `db`.`items` WHERE (`label` NOT LIKE ?) LIMIT ? OFFSET ?",
			wantArgs:  []driver.Value{"%x%", int64(51), int64(0)},
		},
		{
			name:      "filter is null takes no value",
			extra:     url.Values{"filter": {`[{"column":"note","op":"is null","value":"ignored"}]`}},
			wantQuery: "SELECT * FROM `db`.`items` WHERE (`note` IS NULL) LIMIT ? OFFSET ?",
			wantArgs:  []driver.Value{int64(51), int64(0)},
		},
		{
			name:      "filter is not null",
			extra:     url.Values{"filter": {`[{"column":"note","op":"is not null"}]`}},
			wantQuery: "SELECT * FROM `db`.`items` WHERE (`note` IS NOT NULL) LIMIT ? OFFSET ?",
			wantArgs:  []driver.Value{int64(51), int64(0)},
		},
		{
			name:      "three filters join with AND",
			extra:     url.Values{"filter": {`[{"column":"id","op":">=","value":"5"},{"column":"label","op":"like","value":"f"},{"column":"quantity","op":"<","value":"10"}]`}},
			wantQuery: "SELECT * FROM `db`.`items` WHERE (`id` >= ? AND `label` LIKE ? AND `quantity` < ?) LIMIT ? OFFSET ?",
			wantArgs:  []driver.Value{"5", "%f%", "10", int64(51), int64(0)},
		},
		{
			name:      "filter plus sort",
			extra:     url.Values{"sort": {"id"}, "dir": {"desc"}, "filter": {`[{"column":"id","op":"!=","value":"3"}]`}},
			wantQuery: "SELECT * FROM `db`.`items` WHERE (`id` != ?) ORDER BY `id` DESC LIMIT ? OFFSET ?",
			wantArgs:  []driver.Value{"3", int64(51), int64(0)},
		},
		{
			name:            "unknown sort column",
			extra:           url.Values{"sort": {"nope"}, "dir": {"asc"}},
			invalid:         true,
			metadataQueries: 1,
		},
		{
			name:            "bad sort direction",
			extra:           url.Values{"sort": {"id"}, "dir": {"ascending"}},
			invalid:         true,
			metadataQueries: 1,
		},
		{
			name:            "sort without direction",
			extra:           url.Values{"sort": {"id"}},
			invalid:         true,
			metadataQueries: 1,
		},
		{
			name:            "direction without sort",
			extra:           url.Values{"dir": {"asc"}},
			invalid:         true,
			metadataQueries: 1,
		},
		{
			name:            "unknown filter column",
			extra:           url.Values{"filter": {`[{"column":"nope","op":"=","value":"1"}]`}},
			invalid:         true,
			metadataQueries: 1,
		},
		{
			name:            "unsupported filter op",
			extra:           url.Values{"filter": {`[{"column":"id","op":"contains","value":"1"}]`}},
			invalid:         true,
			metadataQueries: 1,
		},
		{
			name:    "four filters",
			extra:   url.Values{"filter": {`[{"column":"id","op":"=","value":"1"},{"column":"id","op":"=","value":"2"},{"column":"id","op":"=","value":"3"},{"column":"id","op":"=","value":"4"}]`}},
			invalid: true,
		},
		{
			name:    "empty filter array",
			extra:   url.Values{"filter": {`[]`}},
			invalid: true,
		},
		{
			name:    "malformed filter json",
			extra:   url.Values{"filter": {`[{"column":"id","op":`}},
			invalid: true,
		},
		{
			name:    "filter object not array",
			extra:   url.Values{"filter": {`{"column":"id","op":"=","value":"1"}`}},
			invalid: true,
		},
		{
			name:    "unknown filter field",
			extra:   url.Values{"filter": {`[{"column":"id","op":"=","value":"1","evil":true}]`}},
			invalid: true,
		},
		{
			name:    "filter value not a string",
			extra:   url.Values{"filter": {`[{"column":"id","op":"=","value":1}]`}},
			invalid: true,
		},
		{
			name:    "trailing junk after filter json",
			extra:   url.Values{"filter": {`[{"column":"id","op":"=","value":"1"}] 5`}},
			invalid: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			engine := metaEngine(t, "items", func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
				return []string{"id", "label", "quantity", "note", "raw_data"}, [][]driver.Value{{int64(1), []byte("Alpha"), int64(10), nil, []byte{0xff, 0x00}}}, nil
			})
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			before := recordCount(engine)
			rec := b.request(s, http.MethodGet, browsePath("db", "items", test.extra), "")
			if test.invalid {
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid sort or filter") {
					t.Fatalf("expected 400 invalid sort or filter, got %d %s", rec.Code, rec.Body.String())
				}
				if got := recordCount(engine) - before; got != test.metadataQueries {
					t.Fatalf("invalid browse issued %d queries, want %d", got, test.metadataQueries)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("browse failed: %d %s", rec.Code, rec.Body.String())
			}
			got := lastRecordQuery(engine)
			if got != test.wantQuery {
				t.Fatalf("query = %q, want %q", got, test.wantQuery)
			}
			engine.mu.Lock()
			record := engine.records[len(engine.records)-1]
			engine.mu.Unlock()
			if len(record.args) != len(test.wantArgs) {
				t.Fatalf("arg count = %d, want %d", len(record.args), len(test.wantArgs))
			}
			for i, want := range test.wantArgs {
				if record.args[i].Value != want {
					t.Fatalf("arg %d = %#v, want %#v (query %s)", i, record.args[i].Value, want, got)
				}
			}
			var result struct {
				Rows [][]interface{} `json:"rows"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || len(result.Rows) != 1 {
				t.Fatalf("browse response did not include the row: %v %s", err, rec.Body.String())
			}
		})
	}
}

func TestBrowseRowsV2WithoutSortAndFilterUsesV1Query(t *testing.T) {
	engine := metaEngine(t, "items", func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if query != "SELECT * FROM `db`.`items` LIMIT ? OFFSET ?" {
			return nil, nil, fmt.Errorf("unexpected query: %s", query)
		}
		if len(args) != 2 || args[0].Value != int64(51) || args[1].Value != int64(0) {
			return nil, nil, errors.New("unexpected pagination arguments")
		}
		return []string{"id"}, [][]driver.Value{{int64(1)}}, nil
	})
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	if rec := b.request(s, http.MethodGet, browsePath("db", "items", nil), ""); rec.Code != http.StatusOK {
		t.Fatalf("plain browse failed: %d %s", rec.Code, rec.Body.String())
	}
	if recordCount(engine) != 1 {
		t.Fatalf("plain browse must not probe INFORMATION_SCHEMA, got %d records", recordCount(engine))
	}
}

func TestRowFetchByFullPrimaryKey(t *testing.T) {
	engine := metaEngine(t, "items", func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if query != "SELECT * FROM `db`.`items` WHERE (`id` = ?) LIMIT 1" || len(args) != 1 || args[0].Value != "1" {
			return nil, nil, fmt.Errorf("unexpected row fetch: %s %#v", query, args)
		}
		return []string{"id", "label", "quantity", "note", "raw_data"}, [][]driver.Value{{int64(1), []byte("Alpha"), int64(10), nil, []byte{0xff, 0x00}}}, nil
	})
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	path := "/api/row?database=db&table=items&key=" + url.QueryEscape(`[["id","1"]]`)
	rec := b.request(s, http.MethodGet, path, "")
	var result struct {
		Columns   []columnInfo  `json:"columns"`
		Values    []interface{} `json:"values"`
		Binary    []bool        `json:"binary"`
		Truncated []bool        `json:"truncated"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
		t.Fatalf("row fetch failed: %d %s", rec.Code, rec.Body.String())
	}
	if len(result.Columns) != 5 || result.Columns[0].Name != "id" || result.Columns[4].Name != "raw_data" {
		t.Fatalf("row fetch columns wrong: %#v", result.Columns)
	}
	if len(result.Values) != 5 || result.Values[1] != "Alpha" || result.Values[3] != nil || result.Values[4] != "0xff00" {
		t.Fatalf("row fetch values wrong: %#v", result.Values)
	}
	if len(result.Binary) != 5 || result.Binary[0] || !result.Binary[4] {
		t.Fatalf("row fetch binary flags wrong: %#v", result.Binary)
	}
	if len(result.Truncated) != 5 || result.Truncated[4] {
		t.Fatalf("row fetch truncated flags wrong: %#v", result.Truncated)
	}
}

func TestRowFetchRejectsBadKeysAndMissingRows(t *testing.T) {
	newCase := func(table string) (*fakeEngine, *server, testBrowser) {
		engine := metaEngine(t, table, func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
			return []string{"id"}, nil, nil
		})
		s := newServer()
		return engine, s, connectFakeDatabase(t, s, engine)
	}
	cases := []struct {
		name    string
		table   string
		key     string
		status  int
		message string
	}{
		{"missing key", "items", "", http.StatusBadRequest, "a primary key is required"},
		{"malformed key json", "items", `[["id"]`, http.StatusBadRequest, "a primary key is required"},
		{"empty key array", "items", `[]`, http.StatusBadRequest, "a primary key is required"},
		{"key without pk", "nokey", `[["value","x"]]`, http.StatusBadRequest, "a primary key is required"},
		{"wrong key column", "items", `[["label","Alpha"]]`, http.StatusBadRequest, "a primary key is required"},
		{"partial key", "items", `[["id","1"]]`, http.StatusOK, ""}, // placeholder; overwritten below
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			engine, s, b := newCase(test.table)
			_ = engine
			path := "/api/row?database=db&table=" + test.table + "&key=" + url.QueryEscape(test.key)
			rec := b.request(s, http.MethodGet, path, "")
			if test.name == "partial key" {
				// items has a one-column primary key, so this key is complete:
				// the lookup runs but the table has no row.
				if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "row not found") {
					t.Fatalf("expected 404 row not found, got %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != test.status || !strings.Contains(rec.Body.String(), test.message) {
				t.Fatalf("expected %d %q, got %d %s", test.status, test.message, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRowFetchBinaryPrimaryKeyUsesHex(t *testing.T) {
	engine := metaEngine(t, "binkey", func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if query != "SELECT * FROM `db`.`binkey` WHERE (`id` = UNHEX(?)) LIMIT 1" || len(args) != 1 || args[0].Value != "0a0b" {
			return nil, nil, fmt.Errorf("unexpected binary row fetch: %s %#v", query, args)
		}
		return []string{"id", "data"}, [][]driver.Value{{[]byte{0x0a, 0x0b}, []byte{0xff}}}, nil
	})
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	path := "/api/row?database=db&table=binkey&key=" + url.QueryEscape(`[["id","0a0b"]]`)
	rec := b.request(s, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("binary row fetch failed: %d %s", rec.Code, rec.Body.String())
	}
	badPath := "/api/row?database=db&table=binkey&key=" + url.QueryEscape(`[["id","zz"]]`)
	if rec := b.request(s, http.MethodGet, badPath, ""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid hex value") {
		t.Fatalf("expected 400 invalid hex value, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestIndexesListingGroupsColumns(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if !strings.Contains(query, "INFORMATION_SCHEMA.STATISTICS") || len(args) != 2 || args[0].Value != "db" || args[1].Value != "items" {
			return nil, nil, fmt.Errorf("unexpected indexes lookup: %s %#v", query, args)
		}
		return []string{"INDEX_NAME", "NON_UNIQUE", "SEQ_IN_INDEX", "COLUMN_NAME", "INDEX_TYPE"}, [][]driver.Value{
			{"PRIMARY", int64(0), int64(1), "id", "BTREE"},
			{"idx_label", int64(1), int64(1), "label", "BTREE"},
			{"idx_label", int64(1), int64(2), "name", "BTREE"},
		}, nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	if rec := b.request(s, http.MethodGet, "/api/indexes?database=db&table=items", ""); rec.Code != http.StatusOK {
		t.Fatalf("indexes listing failed: %d %s", rec.Code, rec.Body.String())
	} else if body := rec.Body.String(); !strings.Contains(body, `"name":"PRIMARY"`) ||
		!strings.Contains(body, `"columns":["id"]`) || !strings.Contains(body, `"unique":true`) ||
		!strings.Contains(body, `"columns":["label","name"]`) || !strings.Contains(body, `"unique":false`) ||
		!strings.Contains(body, `"type":"BTREE"`) {
		t.Fatalf("indexes payload wrong: %s", body)
	}
}

func TestRoutinesTriggersEventsAndRoutineShow(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.Contains(query, "INFORMATION_SCHEMA.ROUTINES"):
			if len(args) != 1 || args[0].Value != "db" {
				return nil, nil, errors.New("unexpected routines arguments")
			}
			return []string{"ROUTINE_NAME", "ROUTINE_TYPE", "CREATED", "LAST_ALTERED"}, [][]driver.Value{
				{"dbkid_one_result", "PROCEDURE", "2026-11-04 22:43:45", "2026-11-04 22:43:45"},
			}, nil
		case strings.Contains(query, "INFORMATION_SCHEMA.TRIGGERS"):
			return []string{"TRIGGER_NAME", "EVENT_MANIPULATION", "EVENT_OBJECT_TABLE", "ACTION_TIMING"}, [][]driver.Value{
				{"t1", "INSERT", "items", "BEFORE"},
			}, nil
		case strings.Contains(query, "INFORMATION_SCHEMA.EVENTS"):
			return []string{"EVENT_NAME", "STATUS", "EVENT_TYPE", "INTERVAL_VALUE", "INTERVAL_FIELD"}, nil, nil
		case strings.HasPrefix(query, "SHOW CREATE Procedure `db`.`dbkid_one_result`"):
			return []string{"Procedure", "sql_mode", "Create Procedure", "character_set_client"}, [][]driver.Value{
				{"dbkid_one_result", "STRICT", "CREATE DEFINER=`root`@`localhost` PROCEDURE `dbkid_one_result`()\nBEGIN\n  SELECT 1;\nEND", "utf8mb4"},
			}, nil
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)

	rec := b.request(s, http.MethodGet, "/api/routines?database=db", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"routines":[{"name":"dbkid_one_result","type":"PROCEDURE","created":"2026-11-04 22:43:45","last_altered":"2026-11-04 22:43:45"}]`) {
		t.Fatalf("routines payload wrong: %d %s", rec.Code, rec.Body.String())
	}
	rec = b.request(s, http.MethodGet, "/api/triggers?database=db", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"triggers":[{"name":"t1","event":"INSERT","table":"items","timing":"BEFORE"}]`) {
		t.Fatalf("triggers payload wrong: %d %s", rec.Code, rec.Body.String())
	}
	rec = b.request(s, http.MethodGet, "/api/events?database=db", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Fatalf("events payload wrong: %d %s", rec.Code, rec.Body.String())
	}
	rec = b.request(s, http.MethodGet, "/api/routine?database=db&name=dbkid_one_result&kind=procedure", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"dbkid_one_result"`) ||
		!strings.Contains(rec.Body.String(), `"definition":"CREATE DEFINER=`) || strings.Contains(rec.Body.String(), `"truncated"`) {
		t.Fatalf("routine show payload wrong: %d %s", rec.Code, rec.Body.String())
	}
	if rec := b.request(s, http.MethodGet, "/api/routine?database=db&name=x&kind=trigger", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid kind must 400, got %d", rec.Code)
	}
	if rec := b.request(s, http.MethodPost, "/api/routines?database=db", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("routines POST must 405, got %d", rec.Code)
	}
}

func TestRoutineShowTruncatesLongDefinitions(t *testing.T) {
	definition := strings.Repeat("x", maxCellDisplayBytes+100)
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.HasPrefix(query, "SHOW CREATE Procedure") {
			return []string{"Procedure", "sql_mode", "Create Procedure"}, [][]driver.Value{{"p", "STRICT", definition}}, nil
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/routine?database=db&name=p&kind=procedure", "")
	var result struct {
		Definition string `json:"definition"`
		Truncated  bool   `json:"truncated"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
		t.Fatalf("routine show failed: %d %s", rec.Code, rec.Body.String())
	}
	if !result.Truncated || len(result.Definition) > maxCellDisplayBytes+len(truncationMarker) ||
		!strings.HasSuffix(result.Definition, truncationMarker) {
		t.Fatalf("long definition was not capped: truncated=%v len=%d", result.Truncated, len(result.Definition))
	}
}

func TestBrowseHandlerRejectsWrongMethodAndMissingNames(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	if rec := b.request(s, http.MethodPost, "/api/rows?database=db&table=items", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("rows POST must 405, got %d", rec.Code)
	}
	if rec := b.request(s, http.MethodGet, "/api/rows?database=&table=items", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing database must 400, got %d", rec.Code)
	}
	if rec := b.request(s, http.MethodGet, "/api/indexes?database=db&table=", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing table must 400, got %d", rec.Code)
	}
}
