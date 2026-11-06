package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func createBodyForSession(s *server, b testBrowser, body string) string {
	s.mu.Lock()
	current := s.sessions[b.cookie.Value]
	s.mu.Unlock()
	return strings.TrimSuffix(body, "}") + `,"generation":` + strconv.FormatUint(current.info().Generation, 10) + `}`
}

type blockingCreateConnector struct {
	engine           *fakeEngine
	entered, release chan struct{}
}

func (c blockingCreateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	close(c.entered)
	select {
	case <-c.release:
		return (fakeConnector{c.engine}).Connect(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c blockingCreateConnector) Driver() driver.Driver { return fakeDriver{c.engine} }

func TestDefinitionJSONFitsConservativelyAccountsControlEscapes(t *testing.T) {
	for _, value := range []string{"\b", "\f"} {
		if definitionJSONFits([]byte(value), maxResultJSONBytes-7) {
			t.Fatalf("must reserve six escaped bytes plus two quotes for %q", value)
		}
		if !definitionJSONFits([]byte(value), maxResultJSONBytes-8) {
			t.Fatalf("rejected exact conservative boundary for %q", value)
		}
	}
}

func createReadbackFixture(q string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
	kind, name, definition := "procedure", "p", "CREATE PROCEDURE p() SELECT 1"
	if strings.Contains(q, "`p_test`") {
		name = "p_test"
		definition = "CREATE PROCEDURE p_test() SELECT 1"
	}
	if strings.Contains(q, "SHOW CREATE EVENT") {
		kind = "event"
		name = "IF"
		definition = "CREATE EVENT `IF` ON SCHEDULE EVERY 1 DAY DO SELECT 1"
	}
	_, column := objectColumn(kind)
	cols := []string{kind, "sql_mode", column, "character_set_client", "collation_connection", "Database Collation"}
	vals := []driver.Value{name, "", definition, "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci"}
	if kind == "event" {
		cols = append(cols, "time_zone")
		vals = append(vals, "SYSTEM")
	}
	return cols, [][]driver.Value{vals}, nil
}

func TestObjectCreateReadbackOnPinnedConnection(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		status      int
	}{
		{"present", "created", 200}, {"absent", "created_unverified", 502}, {"error", "created_unverified", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement := "CREATE EVENT p ON SCHEDULE EVERY 1 DAY DO SELECT 1"
			e := &fakeEngine{query: func(q string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
				if tc.name == "error" {
					return nil, nil, context.DeadlineExceeded
				}
				cols := []string{"Event", "sql_mode", "time_zone", "Create Event", "character_set_client", "collation_connection", "Database Collation"}
				if tc.name == "absent" {
					return cols, nil, nil
				}
				return cols, [][]driver.Value{{"p", "", "SYSTEM", statement, "utf8mb4", "utf8mb4_general_ci", "utf8mb4_general_ci"}}, nil
			}}
			s := newServer()
			b := connectFakeDatabase(t, s, e)
			body := createBodyForSession(s, b, `{"database":"db","name":"p","kind":"event","sql":`+strconv.Quote(statement)+`,"confirmed":true}`)
			rec := b.mutation(s, "/api/object/create", body)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"stage":"`+tc.stage+`"`) {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if len(e.records) != 3 || e.records[2].query != "SHOW CREATE EVENT `db`.`p`" || e.records[0].conn != e.records[1].conn || e.records[1].conn != e.records[2].conn {
				t.Fatalf("records=%+v", e.records)
			}
		})
	}
}

func TestObjectCreateAcceptsMatchingDefinitionOnOneConnection(t *testing.T) {
	engine := &fakeEngine{query: createReadbackFixture}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	sql := "CREATE DEFINER=`dbkid_ddl`@`127.0.0.1` PROCEDURE `p_test`() BEGIN SELECT 20; END"
	body := createBodyForSession(s, b, `{"database":"db","name":"p_test","kind":"procedure","sql":`+strconv.Quote(sql)+`,"confirmed":true}`)
	rec := b.mutation(s, "/api/object/create", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.records) != 3 || engine.records[0].query != "USE `db`" || engine.records[1].query != sql || engine.records[2].query != "SHOW CREATE PROCEDURE `db`.`p_test`" || engine.records[0].conn != engine.records[1].conn || engine.records[1].conn != engine.records[2].conn {
		t.Fatalf("records: %#v", engine.records)
	}
	if len(engine.closed) != 1 || engine.closed[0] != engine.records[0].conn {
		t.Fatalf("request connection was not discarded: closed=%v", engine.closed)
	}
}

func TestObjectCreateRequiresGeneration(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := `{"database":"db","name":"p","kind":"procedure","sql":"CREATE PROCEDURE p() SELECT 1","confirmed":true}`
	rec := b.mutation(s, "/api/object/create", body)
	if rec.Code != http.StatusBadRequest || recordCount(engine) != 0 {
		t.Fatalf("missing generation: status=%d body=%s calls=%d", rec.Code, rec.Body.String(), recordCount(engine))
	}
}

func TestConnectionGenerationExposedAndChangesOnConnect(t *testing.T) {
	s := newServer()
	engine := &fakeEngine{}
	b := connectFakeDatabase(t, s, engine)
	getGeneration := func(path, method, body string) uint64 {
		t.Helper()
		rec := b.request(s, method, path, body)
		var result struct {
			Generation *uint64 `json:"generation"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Generation == nil {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		return *result.Generation
	}
	before := getGeneration("/api/session", http.MethodGet, "")
	after := getGeneration("/api/connect", http.MethodPost, `{"host":"127.0.0.2","port":3306,"user":"test","tls":"disable"}`)
	if after == before || getGeneration("/api/session", http.MethodGet, "") != after {
		t.Fatalf("generation before=%d after=%d", before, after)
	}
}

func TestPendingReconnectGenerationCannotAuthorizeNewDatabase(t *testing.T) {
	s := newServer()
	oldEngine, newEngine := &fakeEngine{}, &fakeEngine{}
	b := connectFakeDatabase(t, s, oldEngine)
	entered, release := make(chan struct{}), make(chan struct{})
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) {
		close(entered)
		<-release
		return sql.OpenDB(fakeConnector{newEngine}), "8.new", nil
	})
	done := make(chan int, 1)
	go func() {
		done <- b.request(s, http.MethodPost, "/api/connect", `{"host":"127.0.0.2","port":3306,"user":"test","tls":"disable"}`).Code
	}()
	<-entered
	rec := b.request(s, http.MethodGet, "/api/session", "")
	var state struct {
		Generation *uint64 `json:"generation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil || state.Generation == nil {
		close(release)
		<-done
		t.Fatalf("session status=%d body=%s", rec.Code, rec.Body.String())
	}
	close(release)
	if status := <-done; status != http.StatusOK {
		t.Fatalf("connect status=%d", status)
	}
	body := `{"database":"db","name":"p","kind":"procedure","sql":"CREATE PROCEDURE p() SELECT 1","confirmed":true,"generation":` + strconv.FormatUint(*state.Generation, 10) + `}`
	rec = b.mutation(s, "/api/object/create", body)
	if rec.Code != http.StatusConflict || recordCount(oldEngine) != 0 || recordCount(newEngine) != 0 {
		t.Fatalf("status=%d body=%s old=%d new=%d", rec.Code, rec.Body.String(), recordCount(oldEngine), recordCount(newEngine))
	}
}

func TestObjectCreateStaleAndZeroGeneration(t *testing.T) {
	s := newServer()
	oldEngine, newEngine := &fakeEngine{}, &fakeEngine{query: createReadbackFixture}
	b := connectFakeDatabase(t, s, oldEngine)
	base := `{"database":"db","name":"p","kind":"procedure","sql":"CREATE PROCEDURE p() SELECT 1","confirmed":true}`
	stale := createBodyForSession(s, b, base)
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) {
		return sql.OpenDB(fakeConnector{newEngine}), "8.new", nil
	})
	if rec := b.request(s, http.MethodPost, "/api/connect", `{"host":"127.0.0.2","port":3306,"user":"test","tls":"disable"}`); rec.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"stale", stale, http.StatusConflict},
		{"null", strings.TrimSuffix(base, "}") + `,"generation":null}`, http.StatusBadRequest},
		{"zero is present but stale", strings.TrimSuffix(base, "}") + `,"generation":0}`, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := b.mutation(s, "/api/object/create", tc.body)
			if rec.Code != tc.status || recordCount(oldEngine) != 0 || recordCount(newEngine) != 0 {
				t.Fatalf("status=%d body=%s old=%d new=%d", rec.Code, rec.Body.String(), recordCount(oldEngine), recordCount(newEngine))
			}
		})
	}
	// Zero is a valid present value, not the same as an absent field.
	s.mu.Lock()
	current := s.sessions[b.cookie.Value]
	s.mu.Unlock()
	current.mu.Lock()
	current.generation = 0
	current.mu.Unlock()
	rec := b.mutation(s, "/api/object/create", createBodyForSession(s, b, base))
	if rec.Code != http.StatusOK || recordCount(newEngine) != 3 {
		t.Fatalf("zero generation status=%d body=%s calls=%d", rec.Code, rec.Body.String(), recordCount(newEngine))
	}
}

func TestObjectCreateReconnectDuringConnectionAcquisition(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	oldEngine, newEngine := &fakeEngine{}, &fakeEngine{}
	s := newServer()
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) {
		return sql.OpenDB(blockingCreateConnector{oldEngine, entered, release}), "8.old", nil
	})
	b := bootstrapTestBrowser(t, s)
	connectBody := `{"host":"127.0.0.1","port":3306,"user":"test","tls":"disable"}`
	if rec := b.request(s, http.MethodPost, "/api/connect", connectBody); rec.Code != http.StatusOK {
		t.Fatalf("initial connect status=%d", rec.Code)
	}
	body := createBodyForSession(s, b, `{"database":"db","name":"p","kind":"procedure","sql":"CREATE PROCEDURE p() SELECT 1","confirmed":true}`)
	done := make(chan int, 1)
	go func() { done <- b.mutation(s, "/api/object/create", body).Code }()
	<-entered
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) {
		return sql.OpenDB(fakeConnector{newEngine}), "8.new", nil
	})
	rec := b.request(s, http.MethodPost, "/api/connect", connectBody)
	close(release)
	status := <-done
	if rec.Code != http.StatusOK || recordCount(newEngine) != 0 || (status != http.StatusOK && status != http.StatusBadGateway) {
		t.Fatalf("connect=%d create=%d old=%d new=%d", rec.Code, status, recordCount(oldEngine), recordCount(newEngine))
	}
	if status == http.StatusOK && recordCount(oldEngine) != 2 {
		t.Fatalf("successful create did not execute on old database: %d calls", recordCount(oldEngine))
	}
}

func TestObjectCreateReconnectAfterCaptureUsesOldDatabase(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	oldEngine := &fakeEngine{query: createReadbackFixture, exec: func(query string, _ []driver.NamedValue) (driver.Result, error) {
		if strings.HasPrefix(query, "USE ") {
			close(entered)
			<-release
		}
		return driver.RowsAffected(0), nil
	}}
	newEngine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, oldEngine)
	body := createBodyForSession(s, b, `{"database":"db","name":"p","kind":"procedure","sql":"CREATE PROCEDURE p() SELECT 1","confirmed":true}`)
	done := make(chan int, 1)
	go func() { done <- b.mutation(s, "/api/object/create", body).Code }()
	<-entered
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) {
		return sql.OpenDB(fakeConnector{newEngine}), "8.new", nil
	})
	rec := b.request(s, http.MethodPost, "/api/connect", `{"host":"127.0.0.2","port":3306,"user":"test","tls":"disable"}`)
	close(release)
	status := <-done
	if rec.Code != http.StatusOK || status != http.StatusOK || recordCount(oldEngine) != 3 || recordCount(newEngine) != 0 {
		t.Fatalf("connect=%d create=%d old=%d new=%d", rec.Code, status, recordCount(oldEngine), recordCount(newEngine))
	}
	oldEngine.mu.Lock()
	defer oldEngine.mu.Unlock()
	if oldEngine.records[0].conn != oldEngine.records[1].conn || oldEngine.records[1].query != "CREATE PROCEDURE p() SELECT 1" {
		t.Fatalf("old records=%#v", oldEngine.records)
	}
}

func TestObjectCreateRejectsAmbiguousDatabaseQuote(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := createBodyForSession(s, b, `{"database":"db\\\\`+"`"+`x","name":"p","kind":"procedure","sql":"CREATE PROCEDURE p() SELECT 1","confirmed":true}`)
	rec := b.mutation(s, "/api/object/create", body)
	if rec.Code != http.StatusBadRequest || recordCount(engine) != 0 {
		t.Fatalf("status=%d body=%s calls=%d", rec.Code, rec.Body.String(), recordCount(engine))
	}
}

func TestObjectCreateRejectsIfNotExistsBypass(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := createBodyForSession(s, b, `{"database":"db","name":"IF","kind":"event","sql":"CREATE EVENT IF NOT EXISTS q ON SCHEDULE EVERY 1 DAY DO SELECT 1","confirmed":true}`)
	rec := b.mutation(s, "/api/object/create", body)
	if rec.Code != http.StatusBadRequest || recordCount(engine) != 0 {
		t.Fatalf("status=%d body=%s calls=%d", rec.Code, rec.Body.String(), recordCount(engine))
	}
}

func TestObjectCreateQuotedIF(t *testing.T) {
	engine := &fakeEngine{query: createReadbackFixture}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	statement := "CREATE EVENT `IF` ON SCHEDULE EVERY 1 DAY DO SELECT 1"
	body := createBodyForSession(s, b, `{"database":"db","name":"IF","kind":"event","sql":`+strconv.Quote(statement)+`,"confirmed":true}`)
	rec := b.mutation(s, "/api/object/create", body)
	if rec.Code != http.StatusOK || recordCount(engine) != 3 || lastRecordQuery(engine) != "SHOW CREATE EVENT `db`.`IF`" {
		t.Fatalf("status=%d body=%s calls=%d", rec.Code, rec.Body.String(), recordCount(engine))
	}
}

func TestObjectCreateHeaderVariants(t *testing.T) {
	cases := []struct {
		sql, db, name, kind string
		accept              bool
	}{
		{"CREATE DEFINER=`dbkid_ddl`@`127.0.0.1` PROCEDURE `p_test`() BEGIN SELECT 20; END", "db", "p_test", "procedure", true},
		{"CREATE DEFINER=`dbkid_ddl`@`127.0.0.1` TRIGGER `p_test` BEFORE INSERT ON t FOR EACH ROW SET @a=1", "db", "p_test", "trigger", true},
		{"CREATE DEFINER=`dbkid_ddl`@`127.0.0.1` EVENT `p_test` ON SCHEDULE EVERY 1 DAY DO SELECT 1", "db", "p_test", "event", true},
		{"CREATE PROCEDURE `d``b`.`p``x`() BEGIN SELECT 1; SELECT 2; END", "d`b", "p`x", "procedure", true},
		{"CREATE PROCEDURE `IF`() SELECT 1", "db", "IF", "procedure", true},
		{"CREATE EVENT `IF` ON SCHEDULE EVERY 1 DAY DO SELECT 1", "db", "IF", "event", true},
		{"CREATE TRIGGER `IF` BEFORE INSERT ON t FOR EACH ROW SET @a=1", "db", "IF", "trigger", true},
		{"CREATE PROCEDURE `IF`.`p`() SELECT 1", "IF", "p", "procedure", true},
		{"CREATE PROCEDURE `db`.`IF`() SELECT 1", "db", "IF", "procedure", true},
		{"CREATE EVENT IF NOT EXISTS q ON SCHEDULE EVERY 1 DAY DO SELECT 1", "db", "IF", "event", false},
		{"CREATE OR REPLACE PROCEDURE p() SELECT 1", "db", "p", "procedure", false},
		{"CREATE /*!50000 DEFINER=`x`@`y` */ PROCEDURE p() SELECT 1", "db", "p", "procedure", false},
		{"CREATE PROCEDURE other.p() SELECT 1", "db", "p", "procedure", false},
		{"CREATE PROCEDURE p /*!50000 .q*/ () SELECT 1", "db", "p", "procedure", false},
		{"CREATE TRIGGER p /*!50000 .q*/ BEFORE INSERT ON t FOR EACH ROW SET @a=1", "db", "p", "trigger", false},
		{"CREATE EVENT p /*!50000 .q*/ ON SCHEDULE EVERY 1 DAY DO SELECT 1", "db", "p", "event", false},
		{"CREATE PROCEDURE p2() SELECT 1", "db", "p", "procedure", false},
		{"CREATE PROCEDURE `p\\\\`x`() SELECT 1", "db", "p`x", "procedure", false},
	}
	for _, tc := range cases {
		if got := objectCreateMatches(tc.sql, tc.db, tc.name, tc.kind); got != tc.accept {
			t.Errorf("%q got %v", tc.sql, got)
		}
	}
}

func TestObjectCreateReplayAndBodyLimit(t *testing.T) {
	engine := &fakeEngine{query: createReadbackFixture}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := createBodyForSession(s, b, `{"database":"db","name":"p","kind":"procedure","sql":"CREATE PROCEDURE p() SELECT 1","confirmed":true}`)
	id := "0123456789abcdef0123456789abcdef"
	first := b.mutationWithID(s, "/api/object/create", body, id)
	second := b.mutationWithID(s, "/api/object/create", body, id)
	if first.Code != 200 || second.Code != http.StatusConflict || recordCount(engine) != 3 {
		t.Fatalf("first=%d second=%d records=%d", first.Code, second.Code, recordCount(engine))
	}
	oversized := strings.Replace(body, "SELECT 1", "SELECT 1"+strings.Repeat(" ", maxJSONBodyBytes), 1)
	rec := b.mutation(s, "/api/object/create", oversized)
	if rec.Code != http.StatusRequestEntityTooLarge || recordCount(engine) != 3 {
		t.Fatalf("oversize status=%d records=%d", rec.Code, recordCount(engine))
	}
}

func TestObjectCreateValidatesBeforeConsumingRequestID(t *testing.T) {
	engine := &fakeEngine{query: createReadbackFixture}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	bad := createBodyForSession(s, b, `{"database":"db","name":"p","kind":"procedure","sql":"CREATE PROCEDURE q() SELECT 1","confirmed":true}`)
	good := createBodyForSession(s, b, `{"database":"db","name":"p","kind":"procedure","sql":"CREATE PROCEDURE p() SELECT 1","confirmed":true}`)
	id := "0123456789abcdef0123456789abcdef"
	first := b.mutationWithID(s, "/api/object/create", bad, id)
	second := b.mutationWithID(s, "/api/object/create", good, id)
	if first.Code != http.StatusBadRequest || second.Code != http.StatusOK || recordCount(engine) != 3 {
		t.Fatalf("invalid=%d valid=%d calls=%d", first.Code, second.Code, recordCount(engine))
	}
}

func TestObjectCreateRejectsMismatchedTargetBeforeDB(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	for _, sql := range []string{"SELECT 1", "CREATE PROCEDURE q() SELECT 1", "CREATE TRIGGER p BEFORE INSERT ON t FOR EACH ROW SET @a=1", "CREATE PROCEDURE other.p() SELECT 1"} {
		body := createBodyForSession(s, b, `{"database":"db","name":"p","kind":"procedure","sql":`+strconv.Quote(sql)+`,"confirmed":true}`)
		rec := b.mutation(s, "/api/object/create", body)
		if rec.Code != http.StatusBadRequest || recordCount(engine) != 0 {
			t.Fatalf("%q status=%d body=%s database calls=%d", sql, rec.Code, rec.Body.String(), recordCount(engine))
		}
	}
}
