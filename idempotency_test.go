package main

import (
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReplayedSQLRequestIsExecutedAtMostOnce(t *testing.T) {
	var executions int32
	engine := &fakeEngine{query: func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch query {
		case "UPDATE fixture SET n=n+1":
			atomic.AddInt32(&executions, 1)
			return []string{}, nil, nil
		case "SELECT ROW_COUNT()":
			return []string{"ROW_COUNT()"}, [][]driver.Value{{int64(1)}}, nil
		default:
			return nil, nil, fmt.Errorf("unexpected SQL")
		}
	}}
	s := newServer()
	browser := connectFakeDatabase(t, s, engine)
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/query", strings.NewReader(`{"database":"fixture","sql":"UPDATE fixture SET n=n+1","confirmed":true}`))
		req.AddCookie(browser.cookie)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", browser.token)
		req.Header.Set("X-DBKid-Request-ID", "0123456789abcdef0123456789abcdef")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}
	first, duplicate := send(), send()
	if first.Code != http.StatusOK {
		t.Fatalf("first request returned HTTP %d", first.Code)
	}
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("replayed request returned HTTP %d, want 409", duplicate.Code)
	}
	if got := atomic.LoadInt32(&executions); got != 1 {
		t.Fatalf("SQL executed %d times, want once", got)
	}
}

func replayTestRequest(browser testBrowser, ids ...string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/query", strings.NewReader(`{"database":"fixture","sql":"UPDATE fixture SET n=n+1","confirmed":true}`))
	req.AddCookie(browser.cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", browser.token)
	for _, id := range ids {
		req.Header.Add("X-DBKid-Request-ID", id)
	}
	return req
}

func TestSQLRejectsMissingMalformedAndDuplicateRequestIDHeaders(t *testing.T) {
	s := newServer()
	browser := connectFakeDatabase(t, s, &fakeEngine{})
	cases := [][]string{
		nil,
		{"short"},
		{strings.Repeat("a", 22) + "!"},
		{strings.Repeat("a", 65)},
		{strings.Repeat("a", 32), strings.Repeat("b", 32)},
	}
	for _, ids := range cases {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, replayTestRequest(browser, ids...))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid ID headers returned HTTP %d, want 400", rec.Code)
		}
	}
}

func TestConcurrentSQLReplaysExecuteOnlyOnce(t *testing.T) {
	var executions int32
	engine := &fakeEngine{query: func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch query {
		case "UPDATE fixture SET n=n+1":
			atomic.AddInt32(&executions, 1)
			time.Sleep(30 * time.Millisecond)
			return []string{}, nil, nil
		case "SELECT ROW_COUNT()":
			return []string{"ROW_COUNT()"}, [][]driver.Value{{int64(1)}}, nil
		default:
			return nil, nil, fmt.Errorf("unexpected SQL")
		}
	}}
	s := newServer()
	browser := connectFakeDatabase(t, s, engine)
	const count = 12
	start := make(chan struct{})
	codes := make(chan int, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, replayTestRequest(browser, "0123456789abcdef0123456789abcdef"))
			codes <- rec.Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	accepted, rejected := 0, 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			accepted++
		case http.StatusConflict:
			rejected++
		default:
			t.Errorf("unexpected HTTP status %d", code)
		}
	}
	if accepted != 1 || rejected != count-1 || atomic.LoadInt32(&executions) != 1 {
		t.Fatalf("accepted=%d rejected=%d executions=%d, want 1/%d/1", accepted, rejected, executions, count-1)
	}
}
