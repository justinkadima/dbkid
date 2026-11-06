package main

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestForeignKeyStatementsExactSQL(t *testing.T) {
	cases := []struct{ path, body, want string }{
		{"/api/foreign-key/add",
			`{"database":"db","table":"child","name":"fk_child_parent","columns":["parent_id"],` +
				`"referenced_database":"db","referenced_table":"parent","referenced_columns":["id"],` +
				`"on_delete":"CASCADE","on_update":"RESTRICT","confirmed":true}`,
			"ALTER TABLE `db`.`child` ADD CONSTRAINT `fk_child_parent` FOREIGN KEY (`parent_id`) " +
				"REFERENCES `db`.`parent` (`id`) ON DELETE CASCADE ON UPDATE RESTRICT"},
		{"/api/foreign-key/add",
			`{"database":"db","table":"child","name":"","columns":["a","b"],` +
				`"referenced_database":"ref","referenced_table":"parent","referenced_columns":["x","y"],` +
				`"on_delete":"","on_update":"","confirmed":true}`,
			"ALTER TABLE `db`.`child` ADD CONSTRAINT `fk_child_a` FOREIGN KEY (`a`,`b`) " +
				"REFERENCES `ref`.`parent` (`x`,`y`)"},
		{"/api/foreign-key/add",
			`{"database":"db","table":"child","name":"odd` + "`" + "`" + `fk","columns":["c"],` +
				`"referenced_database":"db","referenced_table":"parent","referenced_columns":["id"],` +
				`"on_delete":"SET NULL","on_update":"NO ACTION","confirmed":true}`,
			"ALTER TABLE `db`.`child` ADD CONSTRAINT `odd````fk` FOREIGN KEY (`c`) " +
				"REFERENCES `db`.`parent` (`id`) ON DELETE SET NULL ON UPDATE NO ACTION"},
		{"/api/foreign-key/drop",
			`{"database":"db","table":"child","name":"fk_child_parent","confirmed":true}`,
			"ALTER TABLE `db`.`child` DROP FOREIGN KEY `fk_child_parent`"},
	}
	for _, tt := range cases {
		t.Run(tt.path+" "+tt.want, func(t *testing.T) {
			engine := &fakeEngine{}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.mutation(s, tt.path, tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("mutation: %d %s", rec.Code, rec.Body.String())
			}
			if count := recordCount(engine); count != 1 || !engine.records[0].exec {
				t.Fatalf("expected one executed statement: %+v", engine.records)
			}
			if engine.records[0].query != tt.want {
				t.Fatalf("SQL = %q, want %q", engine.records[0].query, tt.want)
			}
			if len(engine.records[0].args) != 0 {
				t.Fatalf("DDL must not bind values: %+v", engine.records[0].args)
			}
		})
	}
}

func TestForeignKeyValidationBranches(t *testing.T) {
	cases := []struct{ path, body, message string }{
		{"/api/foreign-key/add", `{"database":"","table":"t","columns":["a"],"referenced_database":"d","referenced_table":"p","referenced_columns":["x"],"confirmed":true}`, "database and table names are required"},
		{"/api/foreign-key/add", `{"database":"d","table":"t","columns":["a"],"referenced_database":"","referenced_table":"p","referenced_columns":["x"],"confirmed":true}`, "database and table names are required"},
		{"/api/foreign-key/add", `{"database":"d","table":"t","name":"` + strings.Repeat("x", 65) + `","columns":["a"],"referenced_database":"d","referenced_table":"p","referenced_columns":["x"],"confirmed":true}`, "a valid foreign key name is required"},
		{"/api/foreign-key/add", `{"database":"d","table":"t","columns":[],"referenced_database":"d","referenced_table":"p","referenced_columns":[],"confirmed":true}`, "a foreign key needs 1 to 3 columns"},
		{"/api/foreign-key/add", `{"database":"d","table":"t","columns":["a"],"referenced_database":"d","referenced_table":"p","referenced_columns":[],"confirmed":true}`, "a foreign key needs the same number of local and referenced columns"},
		{"/api/foreign-key/add", `{"database":"d","table":"t","columns":["a","b"],"referenced_database":"d","referenced_table":"p","referenced_columns":["x"],"confirmed":true}`, "a foreign key needs the same number of local and referenced columns"},
		{"/api/foreign-key/add", `{"database":"d","table":"t","columns":["a"],"referenced_database":"d","referenced_table":"p","referenced_columns":[""],"confirmed":true}`, "a valid column name is required"},
		{"/api/foreign-key/add", `{"database":"d","table":"t","columns":["a"],"referenced_database":"d","referenced_table":"p","referenced_columns":["x"],"on_delete":"Nuke","confirmed":true}`, "unsupported ON DELETE action"},
		{"/api/foreign-key/add", `{"database":"d","table":"t","columns":["a"],"referenced_database":"d","referenced_table":"p","referenced_columns":["x"],"on_update":"Cascade","confirmed":true}`, "unsupported ON UPDATE action"},
		{"/api/foreign-key/drop", `{"database":"d","table":"","name":"fk","confirmed":true}`, "database and table names are required"},
		{"/api/foreign-key/drop", `{"database":"d","table":"t","name":"","confirmed":true}`, "a valid foreign key name is required"},
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

func TestForeignKeyListGroupsColumnsAndRules(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.Contains(query, "KEY_COLUMN_USAGE"):
			return []string{"CONSTRAINT_NAME", "COLUMN_NAME", "REFERENCED_TABLE_SCHEMA",
					"REFERENCED_TABLE_NAME", "REFERENCED_COLUMN_NAME", "ORDINAL_POSITION"},
				[][]driver.Value{
					{"fk_a", "left", "refdb", "parent", "pid", int64(1)},
					{"fk_a", "right", "refdb", "parent", "pid2", int64(2)},
					{"fk_b", "solo", "db", "other", "oid", int64(1)},
				}, nil
		case strings.Contains(query, "REFERENTIAL_CONSTRAINTS"):
			return []string{"CONSTRAINT_NAME", "DELETE_RULE", "UPDATE_RULE"},
				[][]driver.Value{{"fk_a", "CASCADE", "NO ACTION"}}, nil
		default:
			return nil, nil, errors.New("unexpected query: " + query)
		}
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/foreign-keys?database=db&table=child", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		ForeignKeys []foreignKeyInfo `json:"foreign_keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(payload.ForeignKeys) != 2 {
		t.Fatalf("constraints = %d, want 2 (%s)", len(payload.ForeignKeys), rec.Body.String())
	}
	first := payload.ForeignKeys[0]
	if first.Name != "fk_a" || len(first.Columns) != 2 || first.Columns[0] != "left" || first.Columns[1] != "right" {
		t.Fatalf("fk_a columns wrong: %+v", first)
	}
	if first.ReferencedDatabase != "refdb" || first.ReferencedTable != "parent" {
		t.Fatalf("fk_a reference wrong: %+v", first)
	}
	if first.ReferencedColumns[0] != "pid" || first.ReferencedColumns[1] != "pid2" {
		t.Fatalf("fk_a referenced columns wrong: %+v", first)
	}
	if first.OnDelete != "CASCADE" || first.OnUpdate != "NO ACTION" {
		t.Fatalf("fk_a rules wrong: %+v", first)
	}
	second := payload.ForeignKeys[1]
	if second.Name != "fk_b" || second.OnDelete != "RESTRICT" || second.OnUpdate != "RESTRICT" {
		t.Fatalf("fk_b must default to RESTRICT rules: %+v", second)
	}
}
