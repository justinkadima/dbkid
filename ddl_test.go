package main

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestTableCreateExactSQL(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{
			"primary, unique, engine",
			`{"database":"db","name":"items","engine":"InnoDB","columns":[` +
				`{"name":"id","type":"int","nullable":false,"auto_increment":true,"primary":true},` +
				`{"name":"label","type":"varchar","length":"255","nullable":false,"unique":true},` +
				`{"name":"note","type":"text","nullable":true}` +
				`],"confirmed":true}`,
			"CREATE TABLE `db`.`items` (`id` INT NOT NULL AUTO_INCREMENT, `label` VARCHAR(255) NOT NULL, `note` TEXT NULL, PRIMARY KEY (`id`), UNIQUE KEY `u_label` (`label`)) ENGINE=InnoDB",
		},
		{
			"composite primary key without engine",
			`{"database":"db","name":"t","engine":"","columns":[` +
				`{"name":"a","type":"bigint","nullable":false,"primary":true},` +
				`{"name":"b","type":"int","nullable":false,"primary":true}` +
				`],"confirmed":true}`,
			"CREATE TABLE `db`.`t` (`a` BIGINT NOT NULL, `b` INT NOT NULL, PRIMARY KEY (`a`,`b`))",
		},
		{
			"decimal with negative numeric default",
			`{"database":"db","name":"t","columns":[` +
				`{"name":"rate","type":"decimal","length":"10","decimals":"2","nullable":true,"default":"-0.5"}` +
				`],"confirmed":true}`,
			"CREATE TABLE `db`.`t` (`rate` DECIMAL(10,2) NULL DEFAULT -0.5)",
		},
		{
			"timestamp current-timestamp default",
			`{"database":"db","name":"t","columns":[` +
				`{"name":"ts","type":"timestamp","nullable":true,"default":"CURRENT_TIMESTAMP"}` +
				`],"confirmed":true}`,
			"CREATE TABLE `db`.`t` (`ts` TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP)",
		},
		{
			"string default and DEFAULT NULL",
			`{"database":"db","name":"t","columns":[` +
				`{"name":"status","type":"varchar","length":"16","nullable":true,"default":"ready"},` +
				`{"name":"gone","type":"int","nullable":true,"default_null":true}` +
				`],"confirmed":true}`,
			"CREATE TABLE `db`.`t` (`status` VARCHAR(16) NULL DEFAULT 'ready', `gone` INT NULL DEFAULT NULL)",
		},
		{
			"engine case-insensitive",
			`{"database":"db","name":"t","engine":"aria","columns":[` +
				`{"name":"c","type":"bool","nullable":false}` +
				`],"confirmed":true}`,
			"CREATE TABLE `db`.`t` (`c` BOOL NOT NULL) ENGINE=ARIA",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			engine := &fakeEngine{}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.mutation(s, "/api/table/create", tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
			}
			var response affectedRowsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("response: %v", err)
			}
			if count := recordCount(engine); count != 1 || !engine.records[0].exec {
				t.Fatalf("expected one executed statement: %+v", engine.records)
			}
			if engine.records[0].query != tt.want {
				t.Fatalf("create SQL =\n %q\nwant\n %q", engine.records[0].query, tt.want)
			}
		})
	}
}

func TestTableCreateValidationBranches(t *testing.T) {
	var wide []string
	for index := 0; index < maxTableColumns+1; index++ {
		wide = append(wide, fmt.Sprintf(`{"name":"c%d","type":"int"}`, index))
	}
	cases := []struct{ name, body, message string }{
		{"unknown type", `{"database":"db","name":"t","columns":[{"name":"c","type":"geometry"}],"confirmed":true}`, "unsupported column type"},
		{"type case is not normalized", `{"database":"db","name":"t","columns":[{"name":"c","type":"INT"}],"confirmed":true}`, "unsupported column type"},
		{"length too wide", `{"database":"db","name":"t","columns":[{"name":"c","type":"varchar","length":"1234"}],"confirmed":true}`, "invalid column length"},
		{"length not numeric", `{"database":"db","name":"t","columns":[{"name":"c","type":"varchar","length":"1a"}],"confirmed":true}`, "invalid column length"},
		{"decimals not numeric", `{"database":"db","name":"t","columns":[{"name":"c","type":"decimal","length":"10","decimals":"12x"}],"confirmed":true}`, "invalid column decimals"},
		{"decimals without length", `{"database":"db","name":"t","columns":[{"name":"c","type":"decimal","decimals":"2"}],"confirmed":true}`, "a length is required with decimals"},
		{"quoted default", `{"database":"db","name":"t","columns":[{"name":"c","type":"varchar","length":"5","default":"O'Brien"}],"confirmed":true}`, "unsupported default value"},
		{"backslash default", `{"database":"db","name":"t","columns":[{"name":"c","type":"varchar","length":"5","default":"a\\b"}],"confirmed":true}`, "unsupported default value"},
		{"current timestamp on int", `{"database":"db","name":"t","columns":[{"name":"c","type":"int","default":"CURRENT_TIMESTAMP"}],"confirmed":true}`, "unsupported default value"},
		{"default null on required column", `{"database":"db","name":"t","columns":[{"name":"c","type":"int","default_null":true}],"confirmed":true}`, "unsupported default value"},
		{"auto increment without key", `{"database":"db","name":"t","columns":[{"name":"c","type":"int","auto_increment":true}],"confirmed":true}`, "auto-increment requires a key"},
		{"unknown engine", `{"database":"db","name":"t","engine":"blackhole","columns":[{"name":"c","type":"int"}],"confirmed":true}`, "unsupported storage engine"},
		{"empty column name", `{"database":"db","name":"t","columns":[{"name":"","type":"int"}],"confirmed":true}`, "a valid column name is required"},
		{"no columns", `{"database":"db","name":"t","columns":[],"confirmed":true}`, "a column definition is required"},
		{"missing columns", `{"database":"db","name":"t","confirmed":true}`, "a column definition is required"},
		{"missing database", `{"database":"","name":"t","columns":[{"name":"c","type":"int"}],"confirmed":true}`, "database and table names are required"},
		{"too many columns", `{"database":"db","name":"t","columns":[` + strings.Join(wide, ",") + `],"confirmed":true}`, "too many columns"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			engine := &fakeEngine{}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.mutation(s, "/api/table/create", tt.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.message) {
				t.Fatalf("response = %d %s, want 400 containing %q", rec.Code, rec.Body.String(), tt.message)
			}
			if count := recordCount(engine); count != 0 {
				t.Fatalf("rejected create reached the database %d times", count)
			}
		})
	}
}

func TestOtherDDLStatementsExactSQL(t *testing.T) {
	cases := []struct{ path, body, want string }{
		{"/api/database/create", `{"name":"fresh` + "`" + `db","confirmed":true}`, "CREATE DATABASE `fresh``db`"},
		{"/api/database/drop", `{"database":"gone","confirmed":true}`, "DROP DATABASE `gone`"},
		{"/api/table/rename", `{"database":"d","table":"old","new_name":"new","confirmed":true}`, "RENAME TABLE `d`.`old` TO `d`.`new`"},
		{"/api/table/drop", `{"database":"d","table":"t","confirmed":true}`, "DROP TABLE `d`.`t`"},
		{"/api/column/add", `{"database":"d","table":"t","column":{"name":"age","type":"int","nullable":true},"position":"","confirmed":true}`, "ALTER TABLE `d`.`t` ADD COLUMN `age` INT NULL"},
		{"/api/column/add", `{"database":"d","table":"t","column":{"name":"age","type":"int","nullable":true},"position":"first","confirmed":true}`, "ALTER TABLE `d`.`t` ADD COLUMN `age` INT NULL FIRST"},
		{"/api/column/add", `{"database":"d","table":"t","column":{"name":"age","type":"int","nullable":true},"position":"after","after_column":"id","confirmed":true}`, "ALTER TABLE `d`.`t` ADD COLUMN `age` INT NULL AFTER `id`"},
		{"/api/column/modify", `{"database":"d","table":"t","old_name":"old","column":{"name":"renamed","type":"varchar","length":"10","nullable":true},"confirmed":true}`, "ALTER TABLE `d`.`t` CHANGE COLUMN `old` `renamed` VARCHAR(10) NULL"},
		{"/api/column/drop", `{"database":"d","table":"t","column":"old","confirmed":true}`, "ALTER TABLE `d`.`t` DROP COLUMN `old`"},
		{"/api/index/create", `{"database":"d","table":"t","name":"idx","columns":["a","b"],"unique":false,"confirmed":true}`, "CREATE INDEX `idx` ON `d`.`t` (`a`,`b`)"},
		{"/api/index/create", `{"database":"d","table":"t","name":"uniq","columns":["a"],"unique":true,"confirmed":true}`, "CREATE UNIQUE INDEX `uniq` ON `d`.`t` (`a`)"},
		{"/api/index/drop", `{"database":"d","table":"t","name":"idx","confirmed":true}`, "ALTER TABLE `d`.`t` DROP INDEX `idx`"},
		{"/api/routine/drop", `{"database":"d","name":"p","kind":"procedure","confirmed":true}`, "DROP PROCEDURE `d`.`p`"},
		{"/api/routine/drop", `{"database":"d","name":"f","kind":"function","confirmed":true}`, "DROP FUNCTION `d`.`f`"},
		{"/api/routine/drop", `{"database":"d","name":"tr","kind":"trigger","confirmed":true}`, "DROP TRIGGER `d`.`tr`"},
		{"/api/routine/drop", `{"database":"d","name":"ev","kind":"event","confirmed":true}`, "DROP EVENT `d`.`ev`"},
		{"/api/process/kill", `{"id":0,"confirmed":true}`, "KILL 0"},
		{"/api/process/kill", `{"id":123,"confirmed":true}`, "KILL 123"},
		{"/api/process/kill", `{"id":9223372036854775807,"confirmed":true}`, "KILL 9223372036854775807"},
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

func TestDDLValidationBranches(t *testing.T) {
	var seventeen []string
	for index := 0; index < 17; index++ {
		seventeen = append(seventeen, fmt.Sprintf(`"c%d"`, index))
	}
	cases := []struct{ path, body, message string }{
		{"/api/database/create", `{"name":"","confirmed":true}`, "a valid database name is required"},
		{"/api/database/drop", `{"database":"","confirmed":true}`, "a valid database name is required"},
		{"/api/table/rename", `{"database":"d","table":"t","new_name":"","confirmed":true}`, "database and table names are required"},
		{"/api/table/drop", `{"database":"d","table":"","confirmed":true}`, "database and table names are required"},
		{"/api/column/add", `{"database":"d","table":"t","column":{"name":"c","type":"int"},"position":"middle","confirmed":true}`, "invalid column position"},
		{"/api/column/add", `{"database":"d","table":"t","column":{"name":"c","type":"int"},"position":"after","after_column":"","confirmed":true}`, "a valid column name is required"},
		{"/api/column/add", `{"database":"d","table":"t","column":{"name":"c","type":"point"},"position":"","confirmed":true}`, "unsupported column type"},
		{"/api/column/modify", `{"database":"d","table":"t","old_name":"","column":{"name":"c","type":"int"},"confirmed":true}`, "a valid column name is required"},
		{"/api/column/drop", `{"database":"d","table":"t","column":"","confirmed":true}`, "database and table names are required"},
		{"/api/index/create", `{"database":"d","table":"t","name":"i","columns":[],"unique":false,"confirmed":true}`, "an index requires 1 to 16 columns"},
		{"/api/index/create", `{"database":"d","table":"t","name":"i","columns":[` + strings.Join(seventeen, ",") + `],"unique":false,"confirmed":true}`, "an index requires 1 to 16 columns"},
		{"/api/index/create", `{"database":"d","table":"t","name":"","columns":["a"],"unique":false,"confirmed":true}`, "a valid index name is required"},
		{"/api/index/create", `{"database":"d","table":"t","name":"i","columns":[""],"unique":false,"confirmed":true}`, "a valid column name is required"},
		{"/api/index/drop", `{"database":"d","table":"t","name":"","confirmed":true}`, "a valid index name is required"},
		{"/api/routine/drop", `{"database":"d","name":"n","kind":"view","confirmed":true}`, "unsupported routine kind"},
		{"/api/routine/drop", `{"database":"d","name":"","kind":"procedure","confirmed":true}`, "a valid database and routine name are required"},
		{"/api/process/kill", `{"id":-1,"confirmed":true}`, "invalid process id"},
		{"/api/process/kill", `{"id":9223372036854775808,"confirmed":true}`, "invalid process id"},
		{"/api/process/kill", `{"id":1.5,"confirmed":true}`, "invalid process id"},
		{"/api/process/kill", `{"confirmed":true}`, "invalid process id"},
		{"/api/process/kill", `{"id":18446744073709551616,"confirmed":true}`, "invalid process id"},
	}
	for _, tt := range cases {
		t.Run(tt.path+" "+tt.message, func(t *testing.T) {
			engine := &fakeEngine{}
			s := newServer()
			b := connectFakeDatabase(t, s, engine)
			rec := b.mutation(s, tt.path, tt.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.message) {
				t.Fatalf("response = %d %s, want 400 containing %q", rec.Code, rec.Body.String(), tt.message)
			}
			if count := recordCount(engine); count != 0 {
				t.Fatalf("rejected request reached the database %d times", count)
			}
		})
	}
}

func TestProcessKillMapsDatabaseErrors(t *testing.T) {
	engine := &fakeEngine{}
	engine.exec = func(string, []driver.NamedValue) (driver.Result, error) {
		return nil, &mysql.MySQLError{Number: 10904, Message: "Unknown thread id"}
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.mutation(s, "/api/process/kill", `{"id":999999999,"confirmed":true}`)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "database error 10904") {
		t.Fatalf("kill error mapping: %d %s", rec.Code, rec.Body.String())
	}
}
