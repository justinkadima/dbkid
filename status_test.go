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
	"time"
)

func TestStatusServesThreeBoundedResultSets(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if len(args) != 0 {
			return nil, nil, errors.New("status queries take no parameters")
		}
		switch query {
		case "SHOW GLOBAL STATUS":
			return []string{"Variable_name", "Value"}, [][]driver.Value{{"Uptime", "1024"}, {"Threads_connected", "1"}}, nil
		case "SHOW GLOBAL VARIABLES":
			return []string{"Variable_name", "Value"}, [][]driver.Value{{"max_allowed_packet", "67108864"}}, nil
		case "SHOW PROCESSLIST":
			return []string{"Id", "User", "Host", "Db", "Command", "Time", "State", "Info"}, [][]driver.Value{
				{int64(342), "dbkid_test", "127.0.0.1:60687", nil, "Query", int64(0), "init", []byte("SHOW PROCESSLIST")},
			}, nil
		}
		return nil, nil, fmt.Errorf("unexpected status query: %s", query)
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/status", "")
	var result struct {
		Status    [][]interface{} `json:"status"`
		Variables [][]interface{} `json:"variables"`
		Processes [][]interface{} `json:"processes"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
		t.Fatalf("status failed: %d %s", rec.Code, rec.Body.String())
	}
	if len(result.Status) != 2 || result.Status[0][0] != "Uptime" || result.Status[0][1] != "1024" {
		t.Fatalf("status rows wrong: %#v", result.Status)
	}
	if len(result.Variables) != 1 || result.Variables[0][0] != "max_allowed_packet" {
		t.Fatalf("variables rows wrong: %#v", result.Variables)
	}
	if len(result.Processes) != 1 || len(result.Processes[0]) != 8 || result.Processes[0][0] != "342" ||
		result.Processes[0][3] != nil || result.Processes[0][7] != "SHOW PROCESSLIST" {
		t.Fatalf("processes rows wrong: %#v", result.Processes)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("status response must not be cacheable")
	}
	if rec := b.request(s, http.MethodPost, "/api/status", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status POST must 405, got %d", rec.Code)
	}
}

func TestHistoryListsNewestFirstAndClears(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch query {
		case "SELECT 1 AS one":
			return []string{"one"}, [][]driver.Value{{int64(1)}}, nil
		case "SELECT 2 AS two":
			return []string{"two"}, [][]driver.Value{{int64(2)}}, nil
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	if rec := b.request(s, http.MethodPost, "/api/query", `{"database":"db","sql":"SELECT 1 AS one","confirmed":true}`); rec.Code != http.StatusOK {
		t.Fatalf("first query failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := b.request(s, http.MethodPost, "/api/query", `{"database":"db","sql":"SELECT 2 AS two","confirmed":true}`); rec.Code != http.StatusOK {
		t.Fatalf("second query failed: %d %s", rec.Code, rec.Body.String())
	}
	rec := b.request(s, http.MethodGet, "/api/history", "")
	var list struct {
		History []historyEntry `json:"history"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("history failed: %d %s", rec.Code, rec.Body.String())
	}
	if len(list.History) != 2 || list.History[0].SQL != "SELECT 2 AS two" || list.History[1].SQL != "SELECT 1 AS one" {
		t.Fatalf("history order wrong: %#v", list.History)
	}
	if list.History[0].Rows == nil || *list.History[0].Rows != 1 {
		t.Fatalf("history row counts wrong: %#v", list.History[0])
	}

	// The clear endpoint is served by handleHistory for the
	// /api/history/clear path; call the handler directly because the
	// dispatcher in server.go may not route that path yet.
	clearRequest := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/history/clear", strings.NewReader("{}"))
	clearRequest.AddCookie(b.cookie)
	clearRequest.Header.Set("X-CSRF-Token", b.token)
	clearRequest.Header.Set("Content-Type", "application/json")
	clearRecorder := httptest.NewRecorder()
	var current *session
	s.mu.Lock()
	for _, candidate := range s.sessions {
		if candidate.csrf == b.token {
			current = candidate
		}
	}
	s.mu.Unlock()
	if current == nil {
		t.Fatal("session not found")
	}
	handleHistory(clearRecorder, clearRequest, current)
	if clearRecorder.Code != http.StatusOK || !strings.Contains(clearRecorder.Body.String(), `"cleared":true`) {
		t.Fatalf("history clear failed: %d %s", clearRecorder.Code, clearRecorder.Body.String())
	}
	rec = b.request(s, http.MethodGet, "/api/history", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"history":[]`) {
		t.Fatalf("history should be empty after clear: %d %s", rec.Code, rec.Body.String())
	}
	if rec := b.request(s, http.MethodPost, "/api/history", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("history POST must 405, got %d", rec.Code)
	}
}

func TestRecentConnectionsAfterConnect(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodGet, "/api/recent", "")
	var result struct {
		Recent []recentConnection `json:"recent"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
		t.Fatalf("recent failed: %d %s", rec.Code, rec.Body.String())
	}
	if len(result.Recent) != 1 || result.Recent[0] != (recentConnection{Host: "127.0.0.1", Port: 3306, User: "test", TLS: "disable"}) {
		t.Fatalf("recent entries wrong: %#v", result.Recent)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatal("recent connections must never include credentials")
	}
	if rec := b.request(s, http.MethodPost, "/api/recent", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("recent POST must 405, got %d", rec.Code)
	}
}

func TestSessionTokensIssueConsumeExpiryAndEviction(t *testing.T) {
	current := &session{}

	// Single use.
	token, err := current.issueBlobToken(blobRequest{Database: "db", Table: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if request, ok := current.consumeBlobToken(token); !ok || request.Database != "db" {
		t.Fatalf("valid token was rejected: %#v", request)
	}
	if _, ok := current.consumeBlobToken(token); ok {
		t.Fatal("blob token was reusable after consumption")
	}
	if _, ok := current.consumeBlobToken("bogus"); ok {
		t.Fatal("unknown blob token was accepted")
	}

	// Expiry.
	current.mu.Lock()
	expired := "expired-token"
	current.blobTokens[expired] = blobRequest{Database: "db", Expires: time.Now().Add(-time.Second)}
	current.mu.Unlock()
	if _, ok := current.consumeBlobToken(expired); ok {
		t.Fatal("expired blob token was accepted")
	}

	// Export tokens behave identically.
	exportToken, err := current.issueExportToken(exportRequest{Database: "db", Format: "csv"})
	if err != nil {
		t.Fatal(err)
	}
	if request, ok := current.consumeExportToken(exportToken); !ok || request.Format != "csv" {
		t.Fatalf("valid export token rejected: %#v", request)
	}
	if _, ok := current.consumeExportToken(exportToken); ok {
		t.Fatal("export token was reusable after consumption")
	}

	// Eviction: issuing beyond the cap drops the oldest pending token first.
	first, err := current.issueBlobToken(blobRequest{Database: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := current.issueBlobToken(blobRequest{Database: "second"})
	if err != nil {
		t.Fatal(err)
	}
	// first + second + maxPendingTokens-1 fillers = maxPendingTokens+1
	// issued tokens: exactly one eviction, and it must be the oldest.
	for i := 0; i < maxPendingTokens-1; i++ {
		if _, err := current.issueBlobToken(blobRequest{Database: "filler"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := current.consumeBlobToken(first); ok {
		t.Fatal("oldest blob token should have been evicted")
	}
	if _, ok := current.consumeBlobToken(second); !ok {
		t.Fatal("second-oldest blob token should still be pending")
	}
	current.mu.RLock()
	pending := len(current.blobTokens)
	current.mu.RUnlock()
	if pending > maxPendingTokens {
		t.Fatalf("pending blob tokens exceed the cap: %d", pending)
	}

	// A retired session rejects everything.
	other := &session{}
	other.retire()
	if other.blobTokens != nil || other.exportTokens != nil {
		t.Fatal("retire must drop pending download tokens")
	}
}
