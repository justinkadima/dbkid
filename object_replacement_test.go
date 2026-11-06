package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func replacementFixture(t *testing.T, kind string) (*server, testBrowser, *fakeEngine, objectSnapshot, string) {
	t.Helper()
	_, column := objectColumn(kind)
	body := "CREATE DEFINER=`user`@`host` " + strings.ToUpper(kind) + " `p`"
	switch kind {
	case "procedure":
		body += "() BEGIN SELECT 1; SELECT 2; END"
	case "trigger":
		body += " BEFORE INSERT ON t FOR EACH ROW SET @a=1"
	case "event":
		body += " ON SCHEDULE EVERY 1 DAY DO SELECT 1"
	}
	engine := &fakeEngine{}
	engine.query = func(q string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.HasPrefix(q, "SHOW CREATE ") {
			cols := []string{kind, "sql_mode", column, "character_set_client", "collation_connection", "Database Collation"}
			vals := []driver.Value{"p", "", body, "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci"}
			if kind == "event" {
				cols = append(cols, "time_zone")
				vals = append(vals, "SYSTEM")
			}
			return cols, [][]driver.Value{vals}, nil
		}
		if strings.HasPrefix(q, "SELECT @@SESSION") {
			return []string{"mode", "charset", "collation", "zone", "dbcollation"}, [][]driver.Value{{"", "utf8mb4", "utf8mb4_general_ci", "SYSTEM", "utf8mb4_general_ci"}}, nil
		}
		return nil, nil, fmt.Errorf("unexpected query %s", q)
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/object/definition?database=db&name=p&kind="+kind, "")
	if rec.Code != 200 {
		t.Fatalf("definition %d: %s", rec.Code, rec.Body.String())
	}
	var original objectSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	return s, b, engine, original, body
}
func replaceBody(t *testing.T, original objectSnapshot, sql string) string {
	t.Helper()
	b, err := json.Marshal(objectReplaceRequest{Original: original, SQL: sql, Confirmed: true})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func assertNoDrop(t *testing.T, e *fakeEngine) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.records {
		if strings.HasPrefix(r.query, "DROP ") {
			t.Fatalf("unexpected DROP: %+v", r)
		}
	}
}

func TestObjectReplacementPinnedConnectionAndReplay(t *testing.T) {
	for _, kind := range []string{"procedure", "trigger", "event"} {
		t.Run(kind, func(t *testing.T) {
			s, b, e, original, sql := replacementFixture(t, kind)
			id := "0123456789abcdef0123456789abcdef"
			body := replaceBody(t, original, sql)
			rec := b.mutationWithID(s, "/api/object/replace", body, id)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stage":"replaced"`) {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			e.mu.Lock()
			records := append([]fakeRecord(nil), e.records...)
			e.mu.Unlock()
			if len(records) != 7 {
				t.Fatalf("records=%+v", records)
			}
			want := []string{"USE `db`", "SELECT @@SESSION.sql_mode, @@SESSION.character_set_client, @@SESSION.collation_connection, @@SESSION.time_zone, @@collation_database", "SHOW CREATE " + strings.ToUpper(kind) + " `db`.`p`", "DROP " + strings.ToUpper(kind) + " `db`.`p`", sql, "SHOW CREATE " + strings.ToUpper(kind) + " `db`.`p`"}
			for i, q := range want {
				if records[i+1].query != q || records[i+1].conn != records[1].conn {
					t.Fatalf("records=%+v", records)
				}
			}
			replay := b.mutationWithID(s, "/api/object/replace", body, id)
			if replay.Code != 409 || !strings.Contains(replay.Body.String(), "replay_unknown") || recordCount(e) != 7 {
				t.Fatalf("replay %d %s", replay.Code, replay.Body.String())
			}
		})
	}
}

func TestReplacementReplayAfterDropDoesNotClaimNotStarted(t *testing.T) {
	s, b, e, original, sqlText := replacementFixture(t, "procedure")
	e.exec = func(q string, _ []driver.NamedValue) (driver.Result, error) {
		if strings.HasPrefix(q, "CREATE") {
			return nil, &mysql.MySQLError{Number: 1064, Message: "syntax"}
		}
		return driver.RowsAffected(0), nil
	}
	id := "0123456789abcdef0123456789abcdef"
	body := replaceBody(t, original, sqlText)
	first := b.mutationWithID(s, "/api/object/replace", body, id)
	if first.Code != 502 || !strings.Contains(first.Body.String(), "dropped_create_failed") {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	count := recordCount(e)
	replay := b.mutationWithID(s, "/api/object/replace", `{`, id)
	if replay.Code != 409 || !strings.Contains(replay.Body.String(), `"stage":"replay_unknown"`) || strings.Contains(replay.Body.String(), "not_started") || recordCount(e) != count {
		t.Fatalf("replay: %d %s, records before=%d after=%d", replay.Code, replay.Body.String(), count, recordCount(e))
	}
}

func TestConcurrentMalformedReplayCannotClaimNotStarted(t *testing.T) {
	s, b, e, original, sqlText := replacementFixture(t, "procedure")
	entered, release := make(chan struct{}), make(chan struct{})
	e.exec = func(q string, _ []driver.NamedValue) (driver.Result, error) {
		if strings.HasPrefix(q, "CREATE") {
			close(entered)
			<-release
			return nil, &mysql.MySQLError{Number: 1064, Message: "syntax"}
		}
		return driver.RowsAffected(0), nil
	}
	id := "0123456789abcdef0123456789abcdef"
	body := replaceBody(t, original, sqlText)
	done := make(chan int, 1)
	go func() { done <- b.mutationWithID(s, "/api/object/replace", body, id).Code }()
	<-entered
	count := recordCount(e)
	replay := b.mutationWithID(s, "/api/object/replace", `{`, id)
	close(release)
	status := <-done
	if status != 502 || replay.Code != 409 || !strings.Contains(replay.Body.String(), `"stage":"replay_unknown"`) || recordCount(e) != count {
		t.Fatalf("first=%d replay=%d %s records before=%d after=%d", status, replay.Code, replay.Body.String(), count, recordCount(e))
	}
}

func TestReplacementReadbackMustVerifyCreatedObject(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		status      int
	}{
		{"present", "replaced", 200},
		{"absent", "created_unverified", 502},
		{"error", "created_unverified", 502},
		{"invalid", "created_unverified", 502},
		{"wrong_definition_target", "created_unverified", 502},
		{"different_same_target", "created_unverified", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, b, e, original, sqlText := replacementFixture(t, "event")
			previous := e.query
			shows := 0
			e.query = func(q string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
				cols, rows, err := previous(q, args)
				if strings.HasPrefix(q, "SHOW CREATE") {
					shows++
					if shows == 2 {
						switch tc.name {
						case "absent":
							return cols, nil, nil
						case "error":
							return nil, nil, context.DeadlineExceeded
						case "invalid":
							rows[0][0] = "other"
						case "wrong_definition_target":
							rows[0][2] = "CREATE EVENT other ON SCHEDULE EVERY 1 DAY DO SELECT 1"
						case "different_same_target":
							rows[0][2] = "CREATE EVENT p ON SCHEDULE EVERY 1 DAY DO SELECT 999"
						}
					}
				}
				return cols, rows, err
			}
			rec := b.mutation(s, "/api/object/replace", replaceBody(t, original, sqlText))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"stage":"`+tc.stage+`"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if shows != 2 {
				t.Fatalf("SHOW count=%d", shows)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if len(e.records) != 7 || e.records[6].conn != e.records[4].conn || !strings.HasPrefix(e.records[6].query, "SHOW CREATE") {
				t.Fatalf("records=%+v", e.records)
			}
		})
	}
}

func TestReplacementRejectsExecutableCommentMarkersInStringLiterals(t *testing.T) {
	for _, marker := range []string{"/*!", "/*M!"} {
		t.Run(marker, func(t *testing.T) {
			s, b, e, original, _ := replacementFixture(t, "procedure")
			rec := b.mutation(s, "/api/object/replace", replaceBody(t, original, "CREATE PROCEDURE p() SELECT '"+marker+"'"))
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"stage":"not_started"`) {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			assertNoDrop(t, e)
		})
	}
}

func TestObjectReplacementRejectsChangedContextAndGeneration(t *testing.T) {
	for _, which := range []string{"generation", "definition", "metadata", "context", "show_metadata", "target", "versioned", "oversize", "missing"} {
		t.Run(which, func(t *testing.T) {
			s, b, e, original, sql := replacementFixture(t, "procedure")
			switch which {
			case "generation":
				original.Generation++
			case "definition":
				original.Definition += " "
				original.Metadata["create procedure"] = original.Definition
			case "metadata":
				original.Metadata["sql_mode"] = "ANSI_QUOTES"
			case "context":
				original.Metadata["database collation"] = "utf8mb4_bin"
			case "show_metadata":
				previous := e.query
				e.query = func(q string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
					cols, rows, err := previous(q, args)
					if strings.HasPrefix(q, "SHOW CREATE") {
						rows[0][0] = "other"
					}
					return cols, rows, err
				}
			case "versioned":
				sql += " /*!50000 ; DROP TABLE t */"
			case "target":
				sql = "CREATE PROCEDURE q() SELECT 1"
			case "oversize":
				sql += strings.Repeat("x", maxJSONBodyBytes)
			case "missing":
				delete(original.Metadata, "collation_connection")
			}
			rec := b.mutation(s, "/api/object/replace", replaceBody(t, original, sql))
			if rec.Code == 200 || !strings.Contains(rec.Body.String(), "not_started") {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			assertNoDrop(t, e)
		})
	}
}

func TestObjectReplacementRemainsOnCapturedDatabaseAcrossReconnect(t *testing.T) {
	s, b, old, original, sqlText := replacementFixture(t, "procedure")
	entered, release := make(chan struct{}), make(chan struct{})
	originalQuery := old.query
	old.query = func(q string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.HasPrefix(q, "SELECT @@SESSION") {
			close(entered)
			<-release
		}
		return originalQuery(q, args)
	}
	newer := &fakeEngine{}
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) {
		db := sql.OpenDB(fakeConnector{newer})
		db.SetMaxIdleConns(0)
		return db, "8.new", nil
	})
	done := make(chan int, 1)
	go func() { done <- b.mutation(s, "/api/object/replace", replaceBody(t, original, sqlText)).Code }()
	<-entered
	rec := b.request(s, "POST", "/api/connect", `{"host":"127.0.0.1","port":3306,"user":"other","tls":"disable"}`)
	if rec.Code != 200 {
		close(release)
		<-done
		t.Fatalf("reconnect %d %s", rec.Code, rec.Body.String())
	}
	close(release)
	if status := <-done; status != 200 {
		t.Fatalf("replacement status %d", status)
	}
	if recordCount(newer) != 0 {
		t.Fatalf("replacement switched database: %+v", newer.records)
	}
	old.mu.Lock()
	defer old.mu.Unlock()
	for _, record := range old.records[1:] {
		if record.conn != old.records[1].conn {
			t.Fatalf("switched connection: %+v", old.records)
		}
	}
}

func TestObjectDefinitionCRDownloadableButReplacementRejected(t *testing.T) {
	s, b, e, _, sqlText := replacementFixture(t, "procedure")
	previous := e.query
	e.query = func(q string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		cols, rows, err := previous(q, args)
		if strings.HasPrefix(q, "SHOW CREATE") {
			rows[0][2] = strings.Replace(sqlText, " BEGIN", "\rBEGIN", 1)
		}
		return cols, rows, err
	}
	rec := b.request(s, http.MethodGet, "/api/object/definition?database=db&name=p&kind=procedure", "")
	var snapshot objectSnapshot
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &snapshot) != nil || !strings.Contains(snapshot.Definition, "\r") {
		t.Fatalf("GET %d %s", rec.Code, rec.Body.String())
	}
	rec = b.mutation(s, "/api/object/replace", replaceBody(t, snapshot, sqlText))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "not_started") {
		t.Fatalf("original CR: %d %s", rec.Code, rec.Body.String())
	}
	assertNoDrop(t, e)
	// Even with a CR-free original, a proposed CR must be rejected.
	s2, b2, e2, original, sql2 := replacementFixture(t, "procedure")
	rec = b2.mutation(s2, "/api/object/replace", replaceBody(t, original, sql2+"\r"))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "not_started") {
		t.Fatalf("proposed CR: %d %s", rec.Code, rec.Body.String())
	}
	assertNoDrop(t, e2)
}

func TestObjectDefinitionRejectsIncompleteMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		columns []string
		values  []driver.Value
	}{
		{"missing collation", []string{"Create Procedure", "sql_mode", "character_set_client", "collation_connection"}, []driver.Value{"CREATE PROCEDURE p() SELECT 1", "", "utf8mb4", "utf8mb4_general_ci"}},
		{"null mode", []string{"Create Procedure", "sql_mode", "character_set_client", "collation_connection", "Database Collation"}, []driver.Value{"CREATE PROCEDURE p() SELECT 1", nil, "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci"}},
		{"unknown metadata", []string{"Procedure", "Create Procedure", "sql_mode", "character_set_client", "collation_connection", "Database Collation", "unknown_creation_context"}, []driver.Value{"p", "CREATE PROCEDURE p() SELECT 1", "", "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &fakeEngine{query: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
				return tc.columns, [][]driver.Value{tc.values}, nil
			}}
			s := newServer()
			b := connectFakeDatabase(t, s, e)
			rec := b.request(s, http.MethodGet, "/api/object/definition?database=db&name=p&kind=procedure", "")
			if rec.Code != 502 || strings.Contains(rec.Body.String(), `"definition"`) {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestEventReplacementRequiresMatchingTimeZone(t *testing.T) {
	s, b, e, original, sqlText := replacementFixture(t, "event")
	original.Metadata["time_zone"] = "UTC"
	rec := b.mutation(s, "/api/object/replace", replaceBody(t, original, sqlText))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "not_started") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	assertNoDrop(t, e)
}

func TestObjectReplacementRequiresConfirmationAndRequestID(t *testing.T) {
	s, b, e, original, sqlText := replacementFixture(t, "procedure")
	body := replaceBody(t, original, sqlText)
	missingID := b.mutationWithID(s, "/api/object/replace", body, "")
	if missingID.Code != 400 || !strings.Contains(missingID.Body.String(), "unique SQL request ID") {
		t.Fatalf("%d %s", missingID.Code, missingID.Body.String())
	}
	unconfirmed := strings.Replace(body, `"confirmed":true`, `"confirmed":false`, 1)
	rec := b.mutation(s, "/api/object/replace", unconfirmed)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "confirmation") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	assertNoDrop(t, e)
}

func TestObjectReplacementUncertainOutcomes(t *testing.T) {
	for _, tc := range []struct {
		when, stage string
		err         error
	}{
		{"DROP", "drop_unknown", errors.New("connection lost")},
		{"CREATE", "create_unknown", errors.New("connection lost")},
		{"CREATE", "dropped_create_failed", &mysql.MySQLError{Number: 1064, Message: "syntax"}},
		{"CREATE", "create_unknown", &mysql.MySQLError{Number: 1317, Message: "interrupted"}},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			s, b, e, original, sql := replacementFixture(t, "procedure")
			e.exec = func(q string, _ []driver.NamedValue) (driver.Result, error) {
				if strings.HasPrefix(q, tc.when) {
					return nil, tc.err
				}
				return driver.RowsAffected(0), nil
			}
			rec := b.mutation(s, "/api/object/replace", replaceBody(t, original, sql))
			if rec.Code != 502 || !strings.Contains(rec.Body.String(), `"stage":"`+tc.stage+`"`) {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
