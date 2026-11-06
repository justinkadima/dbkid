package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// This read-only integration check runs only with an explicitly supplied,
// disposable loopback server and never creates or modifies database objects.
func TestOptInMySQLReadOnlyIntegration(t *testing.T) {
	dsn := os.Getenv("DBKID_TEST_DSN")
	if dsn == "" {
		t.Skip("set DBKID_TEST_DSN to an isolated loopback test database to opt in")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil || config.Net != "tcp" || config.DBName == "" {
		t.Fatal("invalid opt-in test database configuration")
	}
	host, portText, err := net.SplitHostPort(config.Addr)
	if err != nil {
		t.Fatal("invalid opt-in test database address")
	}
	_, local, valid := normalizeDatabaseHost(host)
	port, portErr := strconv.Atoi(portText)
	if !valid || !local || portErr != nil || port < 1 || port > 65535 {
		t.Fatal("integration test requires a loopback database")
	}
	s := newServer()
	browser := bootstrapTestBrowser(t, s)
	payload, _ := json.Marshal(connectRequest{Host: host, Port: port, User: config.User, Password: config.Passwd, TLS: "disable"})
	connected := browser.request(s, http.MethodPost, "/api/connect", string(payload))
	if connected.Code != http.StatusOK {
		t.Fatalf("could not connect to the opt-in test database: HTTP %d", connected.Code)
	}
	defer browser.request(s, http.MethodPost, "/api/disconnect", "{}")

	submit := func(statement string, confirmed bool) (*struct {
		Columns      []string        `json:"columns"`
		Rows         [][]interface{} `json:"rows"`
		AffectedRows *int64          `json:"affected_rows"`
		Truncated    bool            `json:"truncated"`
	}, int) {
		t.Helper()
		body, _ := json.Marshal(queryRequest{Database: config.DBName, SQL: statement, Confirmed: confirmed})
		rec := browser.request(s, http.MethodPost, "/api/query", string(body))
		var result struct {
			Columns      []string        `json:"columns"`
			Rows         [][]interface{} `json:"rows"`
			AffectedRows *int64          `json:"affected_rows"`
			Truncated    bool            `json:"truncated"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &result) != nil {
			t.Fatal("query returned invalid JSON")
		}
		return &result, rec.Code
	}
	if _, status := submit("SELECT 1", false); status != http.StatusBadRequest {
		t.Fatalf("unconfirmed SQL was not rejected: HTTP %d", status)
	}
	result, status := submit("SELECT NULL AS n, X'FF00' AS b, '' AS e", true)
	if status != 200 || len(result.Columns) != 3 || len(result.Rows) != 1 || result.Rows[0][0] != nil || result.Rows[0][1] != "0xff00" || result.Rows[0][2] != "" || result.AffectedRows != nil {
		t.Fatalf("unexpected read-only SQL result: HTTP %d", status)
	}
	result, status = submit("WITH RECURSIVE seq AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM seq WHERE n < 501) SELECT n FROM seq", true)
	if status != 200 || len(result.Rows) != 500 || !result.Truncated {
		t.Fatalf("real-driver result cap failed: HTTP %d", status)
	}
	result, status = submit("DO 1", true)
	if status != 200 || result.AffectedRows == nil || *result.AffectedRows != 0 || len(result.Columns) != 0 || len(result.Rows) != 0 {
		t.Fatalf("unexpected no-column SQL result: HTTP %d", status)
	}
	if _, status := submit("SELECT 1; SELECT 2", true); status == http.StatusOK {
		t.Fatal("multiple SQL statements were accepted")
	}

	endpoint := "/api/tables?database=" + url.QueryEscape(config.DBName)
	tableResponse := browser.request(s, http.MethodGet, endpoint, "")
	var tables struct {
		Tables []struct {
			Name string `json:"name"`
		} `json:"tables"`
	}
	if tableResponse.Code != 200 || json.Unmarshal(tableResponse.Body.Bytes(), &tables) != nil || tables.Tables == nil {
		t.Fatalf("fixture table listing failed: HTTP %d", tableResponse.Code)
	}
	if len(tables.Tables) > 0 {
		params := "?database=" + url.QueryEscape(config.DBName) + "&table=" + url.QueryEscape(tables.Tables[0].Name)
		structure := browser.request(s, http.MethodGet, "/api/structure"+params, "")
		if structure.Code != 200 {
			t.Fatalf("fixture structure query failed: HTTP %d", structure.Code)
		}
		rows := browser.request(s, http.MethodGet, "/api/rows"+params+"&page_size=1", "")
		if rows.Code != 200 {
			t.Fatalf("fixture row query failed: HTTP %d", rows.Code)
		}
	}
}
