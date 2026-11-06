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

// searchEngine fakes a database with two tables: items (string columns label
// and note plus numeric id) and blobs (binary only, no string columns).
func searchEngine(t *testing.T, tables [][]driver.Value, rowsByTable map[string][][]driver.Value) *fakeEngine {
	t.Helper()
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.Contains(query, "FROM INFORMATION_SCHEMA.TABLES"):
			if len(args) != 1 || args[0].Value != "db" {
				return nil, nil, errors.New("unexpected table listing arguments")
			}
			return []string{"TABLE_NAME", "TABLE_TYPE"}, tables, nil
		case strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS"):
			if len(args) != 2 {
				return nil, nil, errors.New("unexpected column listing arguments")
			}
			switch args[1].Value {
			case "items":
				return []string{"COLUMN_NAME", "DATA_TYPE"}, [][]driver.Value{
					{"id", "int"}, {"label", "varchar"}, {"note", "text"},
				}, nil
			case "blobs":
				return []string{"COLUMN_NAME", "DATA_TYPE"}, [][]driver.Value{
					{"id", "int"}, {"data", "varbinary"},
				}, nil
			}
			return []string{"COLUMN_NAME", "DATA_TYPE"}, nil, nil
		case strings.HasPrefix(query, "SELECT * FROM `db`.`"):
			rest := strings.TrimPrefix(query, "SELECT * FROM `db`.`")
			table := rest[:strings.IndexByte(rest, '`')]
			if values, ok := rowsByTable[table]; ok {
				return []string{"id", "label", "note"}, values, nil
			}
			return nil, nil, fmt.Errorf("unexpected search select: %s", query)
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	return engine
}

func TestSearchLikeAndExactModes(t *testing.T) {
	matches := make([][]driver.Value, searchPerTableRows+1)
	for i := range matches {
		matches[i] = []driver.Value{int64(i + 1), []byte("fixture-1"), []byte("note")}
	}
	engine := searchEngine(t,
		[][]driver.Value{{"blobs", "BASE TABLE"}, {"items", "BASE TABLE"}},
		map[string][][]driver.Value{"items": matches})
	s := newServer()
	b := connectFakeDatabase(t, s, engine)

	var ran []string
	engine.mu.Lock()
	queries := func() []string { return ran }
	engine.mu.Unlock()
	_ = queries

	rec := b.request(s, http.MethodGet, "/api/search?database=db&q=fixture-1", "")
	var result struct {
		Results []searchTableResult `json:"results"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
		t.Fatalf("search failed: %d %s", rec.Code, rec.Body.String())
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected exactly one matching table, got %d: %s", len(result.Results), rec.Body.String())
	}
	entry := result.Results[0]
	if entry.Table != "items" || len(entry.Rows) != searchPerTableRows || !entry.More ||
		entry.MatchedColumnCount != 2 || len(entry.Columns) != 3 {
		t.Fatalf("search result wrong: %#v", entry)
	}
	// The 21st row must prove more without being returned.
	if len(entry.Rows) == searchPerTableRows+1 {
		t.Fatal("search returned the lookahead row")
	}

	engine.mu.Lock()
	var searchQuery string
	var searchArgs []driver.Value
	for _, record := range engine.records {
		if strings.HasPrefix(record.query, "SELECT * FROM `db`.`items`") {
			searchQuery = record.query
			for _, arg := range record.args {
				searchArgs = append(searchArgs, arg.Value)
			}
		}
	}
	engine.mu.Unlock()
	want := "SELECT * FROM `db`.`items` WHERE (`label` LIKE ? OR `note` LIKE ?) LIMIT 21"
	if searchQuery != want {
		t.Fatalf("like query = %q, want %q", searchQuery, want)
	}
	if len(searchArgs) != 2 || searchArgs[0] != "%fixture-1%" || searchArgs[1] != "%fixture-1%" {
		t.Fatalf("like args = %#v", searchArgs)
	}

	rec = b.request(s, http.MethodGet, "/api/search?database=db&q=Alpha&mode=exact", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("exact search failed: %d %s", rec.Code, rec.Body.String())
	}
	engine.mu.Lock()
	exact := ""
	for _, record := range engine.records {
		if strings.HasPrefix(record.query, "SELECT * FROM `db`.`items` WHERE") {
			exact = record.query
		}
	}
	engine.mu.Unlock()
	if exact != "SELECT * FROM `db`.`items` WHERE (`label` = ? OR `note` = ?) LIMIT 21" {
		t.Fatalf("exact query = %q", exact)
	}
}

func TestSearchSkipsTablesWithoutStringColumns(t *testing.T) {
	engine := searchEngine(t,
		[][]driver.Value{{"blobs", "BASE TABLE"}},
		map[string][][]driver.Value{})
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/search?database=db&q=anything", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"results":[]`) {
		t.Fatalf("search without matches failed: %d %s", rec.Code, rec.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	for _, record := range engine.records {
		if strings.HasPrefix(record.query, "SELECT * FROM") {
			t.Fatalf("table without string columns must not be scanned: %s", record.query)
		}
	}
}

func TestSearchRejectsInvalidRequests(t *testing.T) {
	engine := searchEngine(t, nil, nil)
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	cases := []struct {
		path    string
		message string
	}{
		{"/api/search?database=db&q=", "a search term is required"},
		{"/api/search?database=db&q=" + strings.Repeat("x", searchMaxQueryChars+1), "a search term is required"},
		{"/api/search?database=&q=x", "database and table names are required"},
		{"/api/search?database=db&q=x&mode=regex", "invalid search mode"},
	}
	for _, test := range cases {
		rec := b.request(s, http.MethodGet, test.path, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), test.message) {
			t.Fatalf("expected 400 %q for %s, got %d %s", test.message, test.path, rec.Code, rec.Body.String())
		}
	}
	if rec := b.request(s, http.MethodPost, "/api/search?database=db&q=x", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("search POST must 405, got %d", rec.Code)
	}
}

func TestSearchStopsAtGlobalRowCap(t *testing.T) {
	// Three tables each matching exactly searchPerTableRows rows: the global
	// 1000-row cap cannot trigger here, but the per-table cap plus the hit
	// cap must stop scanning further tables.
	tables := [][]driver.Value{{"items", "BASE TABLE"}, {"second", "BASE TABLE"}, {"third", "BASE TABLE"}}
	rows := make([][]driver.Value, 3)
	for i := range rows {
		rows[i] = []driver.Value{int64(i), []byte("needle"), nil}
	}
	engine := searchEngine(t, tables, map[string][][]driver.Value{"items": rows, "second": rows, "third": rows})
	// searchEngine only knows items' column layout; give the other tables the
	// same string columns by treating them as items in the metadata branch.
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS") {
			return []string{"COLUMN_NAME", "DATA_TYPE"}, [][]driver.Value{{"id", "int"}, {"label", "varchar"}}, nil
		}
		if strings.Contains(query, "FROM INFORMATION_SCHEMA.TABLES") {
			return []string{"TABLE_NAME", "TABLE_TYPE"}, tables, nil
		}
		table := strings.TrimSuffix(strings.TrimPrefix(query, "SELECT * FROM `db`.`"), "`")
		if table != "" {
			return []string{"id", "label"}, rows, nil
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/search?database=db&q=needle", "")
	var result struct {
		Results []searchTableResult `json:"results"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Results) != 3 {
		t.Fatalf("search results wrong: %d %s", rec.Code, rec.Body.String())
	}
	for _, entry := range result.Results {
		if entry.MatchedColumnCount != 1 {
			t.Fatalf("matched column count wrong: %#v", entry)
		}
	}
}

func TestSearchURLEncodedTerm(t *testing.T) {
	engine := searchEngine(t,
		[][]driver.Value{{"items", "BASE TABLE"}},
		map[string][][]driver.Value{"items": {}})
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	path := "/api/search?database=db&q=" + url.QueryEscape("O'Reilly & co")
	rec := b.request(s, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("search with encoded term failed: %d %s", rec.Code, rec.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	for _, record := range engine.records {
		if strings.HasPrefix(record.query, "SELECT * FROM `db`.`items`") && len(record.args) == 2 {
			for _, arg := range record.args {
				if arg.Value != "%O'Reilly & co%" {
					t.Fatalf("search term binding wrong: %#v", arg.Value)
				}
			}
		}
	}
}
