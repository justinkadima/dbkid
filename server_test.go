package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test the real executable without contacting any database server.
func TestSessionBootstrap(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "dbkid-test")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("session bootstrap requires a runnable backend: %v\n%s", err, output)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()
	process := exec.Command(binary, "-addr", address)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	client := &http.Client{Timeout: 300 * time.Millisecond}
	var response *http.Response
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		response, err = client.Get("http://" + address + "/api/session")
		if err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("session endpoint never became ready: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session bootstrap status = %d, want 200", response.StatusCode)
	}
	var body struct {
		CSRFToken     string `json:"csrf_token"`
		Connected     bool   `json:"connected"`
		Host          string `json:"host"`
		Port          int    `json:"port"`
		User          string `json:"user"`
		ServerVersion string `json:"server_version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("invalid session JSON: %v", err)
	}
	if len(body.CSRFToken) < 32 || body.Connected || body.Host != "" || body.Port != 0 || body.User != "" || body.ServerVersion != "" {
		t.Fatal("unexpected initial session metadata")
	}
	cookies := response.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" || cookies[0].Value == body.CSRFToken {
		t.Fatal("session cookie is missing or insecure")
	}
}

// Exercise the actual HTTP cookie jar across two loopback ports with a fake DB.
func TestSessionCookieReplayAcrossLoopbackPorts(t *testing.T) {
	s := newServer()
	engine := &fakeEngine{query: func(_ string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return []string{"SCHEMA_NAME"}, [][]driver.Value{{"victim_schema"}}, nil
	}}
	db := sql.OpenDB(fakeConnector{engine})
	t.Cleanup(func() { _ = db.Close() })
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) { return db, "8.mock", nil })
	app := httptest.NewServer(s)
	defer app.Close()

	captured := make(chan *http.Cookie, 1)
	otherPort := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Error(w, "cookie not sent", http.StatusBadRequest)
			return
		}
		captured <- cookie
		w.WriteHeader(http.StatusNoContent)
	}))
	defer otherPort.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	victim := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	response, err := victim.Get(app.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	var initial struct {
		CSRF      string `json:"csrf_token"`
		Connected bool   `json:"connected"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&initial)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || initial.CSRF == "" || initial.Connected {
		t.Fatal("initial HTTP session was not disconnected")
	}
	appURL, err := url.Parse(app.URL)
	if err != nil || len(jar.Cookies(appURL)) != 1 {
		t.Fatal("victim's cookie jar did not store the session")
	}
	originalCookie := jar.Cookies(appURL)[0]

	connect, err := http.NewRequest(http.MethodPost, app.URL+"/api/connect", strings.NewReader(`{"host":"127.0.0.1","port":3306,"user":"test","tls":"disable"}`))
	if err != nil {
		t.Fatal(err)
	}
	connect.Header.Set("Content-Type", "application/json")
	connect.Header.Set("X-CSRF-Token", initial.CSRF)
	response, err = victim.Do(connect)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fake database connect failed: status %d", response.StatusCode)
	}

	response, err = victim.Get(otherPort.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("the browser did not send its session cookie to the other loopback port")
	}
	stolen := <-captured
	if stolen.Value != originalCookie.Value {
		t.Fatal("the other port received a different cookie")
	}

	attacker := &http.Client{Timeout: 3 * time.Second}
	replay, err := http.NewRequest(http.MethodGet, app.URL+"/api/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	replay.AddCookie(stolen)
	response, err = attacker.Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	var fresh struct {
		CSRF          string `json:"csrf_token"`
		Connected     bool   `json:"connected"`
		Host          string `json:"host"`
		Port          int    `json:"port"`
		User          string `json:"user"`
		ServerVersion string `json:"server_version"`
	}
	decodeErr = json.NewDecoder(response.Body).Decode(&fresh)
	cookies := response.Cookies()
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || fresh.Connected || fresh.CSRF == "" || fresh.CSRF == initial.CSRF || fresh.Host != "" || fresh.Port != 0 || fresh.User != "" || fresh.ServerVersion != "" || len(cookies) != 1 || cookies[0].Value == stolen.Value {
		t.Fatal("cross-port replay recovered the connected session")
	}

	databases, err := http.NewRequest(http.MethodGet, app.URL+"/api/databases", nil)
	if err != nil {
		t.Fatal(err)
	}
	databases.Header.Set("X-CSRF-Token", initial.CSRF)
	response, err = victim.Do(databases)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || readErr != nil || !strings.Contains(string(body), `"victim_schema"`) {
		t.Fatal("cross-port replay damaged the original connection")
	}

	// A browser startup/reload has no token in memory, so it must reconnect.
	response, err = victim.Get(app.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	var reloaded struct {
		CSRF      string `json:"csrf_token"`
		Connected bool   `json:"connected"`
	}
	decodeErr = json.NewDecoder(response.Body).Decode(&reloaded)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || reloaded.Connected || reloaded.CSRF == "" || reloaded.CSRF == initial.CSRF || len(jar.Cookies(appURL)) != 1 || jar.Cookies(appURL)[0].Value == originalCookie.Value {
		t.Fatal("headerless browser reload did not start disconnected with new credentials")
	}
	reloadCookie := jar.Cookies(appURL)[0]
	staleTab, err := http.NewRequest(http.MethodGet, app.URL+"/api/databases", nil)
	if err != nil {
		t.Fatal(err)
	}
	staleTab.Header.Set("X-CSRF-Token", initial.CSRF)
	response, err = victim.Do(staleTab)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatal("a stale tab's token was accepted with the replacement browser cookie")
	}
	newDatabases, err := http.NewRequest(http.MethodGet, app.URL+"/api/databases", nil)
	if err != nil {
		t.Fatal(err)
	}
	newDatabases.Header.Set("X-CSRF-Token", reloaded.CSRF)
	response, err = victim.Do(newDatabases)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatal("browser reload unexpectedly retained the old database connection")
	}

	oldDatabases, err := http.NewRequest(http.MethodGet, app.URL+"/api/databases", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldDatabases.AddCookie(originalCookie)
	oldDatabases.Header.Set("X-CSRF-Token", initial.CSRF)
	response, err = attacker.Do(oldDatabases)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("browser reload damaged the original session")
	}
	oldDisconnect, err := http.NewRequest(http.MethodPost, app.URL+"/api/disconnect", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	oldDisconnect.AddCookie(originalCookie)
	oldDisconnect.Header.Set("Content-Type", "application/json")
	oldDisconnect.Header.Set("X-CSRF-Token", initial.CSRF)
	response, err = attacker.Do(oldDisconnect)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || db.Ping() == nil {
		t.Fatal("original session could not disconnect after reload")
	}

	disconnect, err := http.NewRequest(http.MethodPost, app.URL+"/api/disconnect", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	disconnect.Header.Set("Content-Type", "application/json")
	disconnect.Header.Set("X-CSRF-Token", reloaded.CSRF)
	response, err = victim.Do(disconnect)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(jar.Cookies(appURL)) != 0 {
		t.Fatal("HTTP disconnect did not expire the cookie")
	}
	response, err = victim.Get(app.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	var afterDisconnect struct {
		CSRF      string `json:"csrf_token"`
		Connected bool   `json:"connected"`
	}
	decodeErr = json.NewDecoder(response.Body).Decode(&afterDisconnect)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || afterDisconnect.Connected || afterDisconnect.CSRF == "" || afterDisconnect.CSRF == initial.CSRF || afterDisconnect.CSRF == reloaded.CSRF || len(jar.Cookies(appURL)) != 1 || jar.Cookies(appURL)[0].Value == reloadCookie.Value || jar.Cookies(appURL)[0].Value == originalCookie.Value {
		t.Fatal("HTTP bootstrap after disconnect was not a fresh session")
	}
}

func TestSessionCookieAloneCannotRecoverConnectedSession(t *testing.T) {
	s := newServer()
	victim := connectFakeDatabase(t, s, &fakeEngine{})
	t.Cleanup(func() { victim.request(s, http.MethodPost, "/api/disconnect", "{}") })

	req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil)
	req.AddCookie(victim.cookie)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	var body struct {
		CSRF          string `json:"csrf_token"`
		Connected     bool   `json:"connected"`
		Host          string `json:"host"`
		Port          int    `json:"port"`
		User          string `json:"user"`
		ServerVersion string `json:"server_version"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("cookie-only bootstrap: status %d, body %s", rec.Code, rec.Body.String())
	}
	if body.Connected || body.CSRF == "" || body.CSRF == victim.token || body.Host != "" || body.Port != 0 || body.User != "" || body.ServerVersion != "" {
		t.Fatalf("cookie-only replay exposed the connected session or its CSRF token")
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value == "" || cookies[0].Value == victim.cookie.Value || cookies[0].Value == body.CSRF {
		t.Fatal("cookie-only replay did not receive a new session cookie")
	}
	attacker := testBrowser{cookie: cookies[0], token: body.CSRF}
	if rec := attacker.request(s, http.MethodGet, "/api/databases", ""); rec.Code != http.StatusConflict {
		t.Fatalf("new session should be disconnected: status %d", rec.Code)
	}
	if rec := victim.request(s, http.MethodGet, "/api/session", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"connected":true`) || !strings.Contains(rec.Body.String(), victim.token) {
		t.Fatal("cookie-only replay damaged the original session")
	}
}

func TestSessionWrongTokenCannotRecoverConnectedSession(t *testing.T) {
	for _, tt := range []struct {
		name      string
		duplicate bool
	}{
		{"another session's token", false},
		{"multiple token headers", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer()
			engine := &fakeEngine{query: func(_ string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
				return []string{"SCHEMA_NAME"}, [][]driver.Value{{"victim_schema"}}, nil
			}}
			victim := connectFakeDatabase(t, s, engine)
			other := bootstrapTestBrowser(t, s)
			t.Cleanup(func() { victim.request(s, http.MethodPost, "/api/disconnect", "{}") })

			req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil)
			req.AddCookie(victim.cookie)
			if tt.duplicate {
				req.Header.Add("X-CSRF-Token", victim.token)
			}
			req.Header.Add("X-CSRF-Token", other.token)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			var body struct {
				CSRF          string `json:"csrf_token"`
				Connected     bool   `json:"connected"`
				Host          string `json:"host"`
				Port          int    `json:"port"`
				User          string `json:"user"`
				ServerVersion string `json:"server_version"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Connected || body.CSRF == "" || body.CSRF == victim.token || body.CSRF == other.token || body.Host != "" || body.Port != 0 || body.User != "" || body.ServerVersion != "" {
				t.Fatalf("wrong-token replay exposed the original session: status %d", rec.Code)
			}
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Value == "" || cookies[0].Value == victim.cookie.Value || cookies[0].Value == other.cookie.Value || cookies[0].Value == body.CSRF {
				t.Fatal("wrong-token replay did not receive a new independent cookie")
			}
			attacker := testBrowser{cookie: cookies[0], token: body.CSRF}
			if rec := attacker.request(s, http.MethodGet, "/api/databases", ""); rec.Code != http.StatusConflict {
				t.Fatalf("new session should be disconnected: status %d", rec.Code)
			}
			for _, mismatched := range []testBrowser{
				{cookie: victim.cookie, token: body.CSRF},
				{cookie: cookies[0], token: victim.token},
			} {
				if rec := mismatched.request(s, http.MethodGet, "/api/databases", ""); rec.Code != http.StatusForbidden {
					t.Fatalf("mixed credentials were accepted: status %d", rec.Code)
				}
			}

			reused := victim.request(s, http.MethodGet, "/api/session", "")
			var original struct {
				CSRF          string `json:"csrf_token"`
				Connected     bool   `json:"connected"`
				Host          string `json:"host"`
				Port          int    `json:"port"`
				User          string `json:"user"`
				ServerVersion string `json:"server_version"`
			}
			if reused.Code != http.StatusOK || json.Unmarshal(reused.Body.Bytes(), &original) != nil || original.CSRF != victim.token || !original.Connected || original.Host != "127.0.0.1" || original.Port != 3306 || original.User != "test" || original.ServerVersion != "8.mock" || len(reused.Result().Cookies()) != 0 {
				t.Fatal("correct credentials did not reuse the original connected session")
			}
			if rec := victim.request(s, http.MethodGet, "/api/databases", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"victim_schema"`) {
				t.Fatalf("wrong-token replay damaged the original connection: status %d", rec.Code)
			}
		})
	}
}

func TestDisconnectRetiresCredentialsAndBootstrapsFreshSession(t *testing.T) {
	s := newServer()
	victim := connectFakeDatabase(t, s, &fakeEngine{})
	other := bootstrapTestBrowser(t, s)
	original := s.sessions[victim.cookie.Value]
	db := original.db
	t.Cleanup(func() { _ = db.Close() })

	disconnected := victim.request(s, http.MethodPost, "/api/disconnect", "{}")
	if disconnected.Code != http.StatusOK || !strings.Contains(disconnected.Body.String(), `"connected":false`) {
		t.Fatalf("disconnect: status %d, body %s", disconnected.Code, disconnected.Body.String())
	}
	cookies := disconnected.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName || cookies[0].Value != "" || cookies[0].Path != "/" || cookies[0].MaxAge >= 0 {
		t.Fatal("disconnect did not expire the session cookie")
	}
	if _, ok := s.sessions[victim.cookie.Value]; ok {
		t.Fatal("disconnect left its session in the server")
	}
	if !original.retired || original.csrf != "" || original.db != nil || original.host != "" || original.port != 0 || original.user != "" || original.version != "" {
		t.Fatal("disconnect did not retire the session and clear its credentials and connection metadata")
	}
	if db.Ping() == nil {
		t.Fatal("disconnect left the database pool open")
	}
	if rec := victim.request(s, http.MethodGet, "/api/databases", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("old credentials still authorize requests: status %d", rec.Code)
	}

	// Even if the browser sends a stale cookie, a headerless bootstrap must be fresh.
	req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil)
	req.AddCookie(victim.cookie)
	fresh := httptest.NewRecorder()
	s.ServeHTTP(fresh, req)
	var body struct {
		CSRF      string `json:"csrf_token"`
		Connected bool   `json:"connected"`
	}
	freshCookies := fresh.Result().Cookies()
	if fresh.Code != http.StatusOK || json.Unmarshal(fresh.Body.Bytes(), &body) != nil || body.Connected || body.CSRF == "" || body.CSRF == victim.token || len(freshCookies) != 1 || freshCookies[0].Value == victim.cookie.Value {
		t.Fatalf("post-disconnect bootstrap was not a fresh session: status %d", fresh.Code)
	}
	// Old cookie and token together must also be unable to revive the old session.
	old := victim.request(s, http.MethodGet, "/api/session", "")
	var oldBody struct {
		CSRF string `json:"csrf_token"`
	}
	if old.Code != http.StatusOK || json.Unmarshal(old.Body.Bytes(), &oldBody) != nil || oldBody.CSRF == "" || oldBody.CSRF == victim.token || len(old.Result().Cookies()) != 1 {
		t.Fatal("disconnect allowed old credentials to revive the old session")
	}
	if rec := other.request(s, http.MethodGet, "/api/session", ""); rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 || !strings.Contains(rec.Body.String(), other.token) {
		t.Fatal("disconnect damaged an unrelated session")
	}
}

func TestReplayDoesNotExtendOriginalSessionExpiry(t *testing.T) {
	for _, tt := range []struct {
		name           string
		withWrongToken bool
	}{
		{"cookie only", false},
		{"wrong token", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer()
			start := time.Unix(1000000, 0)
			now := start
			s.setClock(func() time.Time { return now })
			victim := connectFakeDatabase(t, s, &fakeEngine{})
			db := s.sessions[victim.cookie.Value].db
			t.Cleanup(func() { _ = db.Close() })

			now = start.Add(sessionLifetime - time.Second)
			req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil)
			req.AddCookie(victim.cookie)
			if tt.withWrongToken {
				req.Header.Set("X-CSRF-Token", victim.token[:len(victim.token)-1]+"!")
			}
			replay := httptest.NewRecorder()
			s.ServeHTTP(replay, req)
			if replay.Code != http.StatusOK || len(replay.Result().Cookies()) != 1 {
				t.Fatalf("replay bootstrap failed: status %d", replay.Code)
			}

			now = start.Add(sessionLifetime)
			expired := victim.request(s, http.MethodGet, "/api/session", "")
			var body struct {
				CSRF      string `json:"csrf_token"`
				Connected bool   `json:"connected"`
			}
			if expired.Code != http.StatusOK || json.Unmarshal(expired.Body.Bytes(), &body) != nil || body.Connected || body.CSRF == "" || body.CSRF == victim.token || len(expired.Result().Cookies()) != 1 {
				t.Fatal("unauthenticated replay extended the original session lifetime")
			}
			if db.Ping() == nil {
				t.Fatal("original database was not closed upon expiry")
			}
		})
	}
}

func TestAuthenticatedBootstrapExtendsSessionExpiry(t *testing.T) {
	s := newServer()
	start := time.Unix(2000000, 0)
	now := start
	s.setClock(func() time.Time { return now })
	victim := connectFakeDatabase(t, s, &fakeEngine{})
	t.Cleanup(func() { victim.request(s, http.MethodPost, "/api/disconnect", "{}") })

	now = start.Add(sessionLifetime - time.Second)
	if rec := victim.request(s, http.MethodGet, "/api/session", ""); rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
		t.Fatal("authenticated bootstrap did not reuse the live session")
	}
	now = start.Add(sessionLifetime + time.Second)
	if rec := victim.request(s, http.MethodGet, "/api/session", ""); rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 || !strings.Contains(rec.Body.String(), `"connected":true`) || !strings.Contains(rec.Body.String(), victim.token) {
		t.Fatal("authenticated bootstrap did not extend the session lifetime")
	}
}

func TestCookieReplayDoesNotSupersedeInFlightConnect(t *testing.T) {
	s := newServer()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseConnect := func() { once.Do(func() { close(release) }) }
	defer releaseConnect()
	db := sql.OpenDB(noopConnector{})
	t.Cleanup(func() { _ = db.Close() })
	s.setOpenDatabase(func(context.Context, connectRequest) (*sql.DB, string, error) {
		close(started)
		<-release
		return db, "8.mock", nil
	})
	victim := bootstrapTestBrowser(t, s)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- victim.request(s, http.MethodPost, "/api/connect", `{"host":"127.0.0.1","port":3306,"user":"test","tls":"disable"}`)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not start")
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil)
	req.AddCookie(victim.cookie)
	replay := httptest.NewRecorder()
	s.ServeHTTP(replay, req)
	var body struct {
		CSRF      string `json:"csrf_token"`
		Connected bool   `json:"connected"`
	}
	if replay.Code != http.StatusOK || json.Unmarshal(replay.Body.Bytes(), &body) != nil || body.Connected || body.CSRF == "" || body.CSRF == victim.token || len(replay.Result().Cookies()) != 1 {
		t.Fatal("cookie replay did not bootstrap an independent disconnected session")
	}
	releaseConnect()
	select {
	case result := <-done:
		if result.Code != http.StatusOK {
			t.Fatalf("cookie replay superseded the original connect: status %d", result.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not finish")
	}
	if rec := victim.request(s, http.MethodGet, "/api/session", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"connected":true`) || !strings.Contains(rec.Body.String(), victim.token) {
		t.Fatal("cookie replay damaged the in-flight connection")
	}
	if rec := victim.request(s, http.MethodPost, "/api/disconnect", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("disconnect failed: status %d", rec.Code)
	}
}

func TestConcurrentSessionBootstrapAndDisconnect(t *testing.T) {
	s := newServer()
	victim := connectFakeDatabase(t, s, &fakeEngine{})
	original := s.sessions[victim.cookie.Value]
	db := original.db
	t.Cleanup(func() { _ = db.Close() })

	const requests = 96
	start := make(chan struct{})
	errors := make(chan string, requests+1)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		withToken := i%2 == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil)
			req.AddCookie(victim.cookie)
			if withToken {
				req.Header.Set("X-CSRF-Token", victim.token)
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			var body struct {
				CSRF          string `json:"csrf_token"`
				Connected     bool   `json:"connected"`
				Host          string `json:"host"`
				Port          int    `json:"port"`
				User          string `json:"user"`
				ServerVersion string `json:"server_version"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.CSRF == "" {
				errors <- "concurrent bootstrap returned an invalid session"
				return
			}
			if !withToken {
				cookies := rec.Result().Cookies()
				if body.CSRF == victim.token || body.Connected || body.Host != "" || body.Port != 0 || body.User != "" || body.ServerVersion != "" || len(cookies) != 1 || cookies[0].Value == victim.cookie.Value {
					errors <- "concurrent cookie replay exposed the original session"
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if rec := victim.request(s, http.MethodPost, "/api/disconnect", "{}"); rec.Code != http.StatusOK {
			errors <- "concurrent disconnect failed"
		}
	}()
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if _, ok := s.sessions[victim.cookie.Value]; ok || !original.retired || original.csrf != "" || db.Ping() == nil {
		t.Fatal("concurrent disconnect did not retire and close the original session")
	}
}

func TestHostAndOriginValidation(t *testing.T) {
	tests := []struct {
		name, host, origin string
		want               int
	}{
		{"loopback without origin", "127.0.0.1:8080", "", http.StatusOK},
		{"matching origin", "localhost:8080", "http://localhost:8080", http.StatusOK},
		{"IPv6 loopback", "[::1]:8080", "http://[::1]:8080", http.StatusOK},
		{"DNS rebinding host", "attacker.example:8080", "", http.StatusForbidden},
		{"wildcard host", "0.0.0.0:8080", "", http.StatusForbidden},
		{"cross-origin port", "127.0.0.1:8080", "http://127.0.0.1:8081", http.StatusForbidden},
		{"cross-origin hostname", "127.0.0.1:8080", "http://evil.example", http.StatusForbidden},
		{"opaque origin", "127.0.0.1:8080", "null", http.StatusForbidden},
		{"origin with credentials", "127.0.0.1:8080", "http://attacker@127.0.0.1:8080", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/session", nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			response := httptest.NewRecorder()
			newServer().ServeHTTP(response, req)
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d", response.Code, tt.want)
			}
			if response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("CORS must not be enabled")
			}
		})
	}
}

func TestSessionIsolationAndCSRF(t *testing.T) {
	s := newServer()
	bootstrap := func(cookie *http.Cookie, token string) (*http.Cookie, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/session", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		var body struct {
			CSRF string `json:"csrf_token"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.CSRF == "" {
			t.Fatalf("invalid session response: %d", rec.Code)
		}
		if cookie == nil {
			if len(rec.Result().Cookies()) != 1 {
				t.Fatal("session cookie missing")
			}
			cookie = rec.Result().Cookies()[0]
		} else if len(rec.Result().Cookies()) != 0 {
			t.Fatal("authenticated bootstrap unexpectedly replaced the session")
		}
		return cookie, body.CSRF
	}
	a, tokenA := bootstrap(nil, "")
	b, tokenB := bootstrap(nil, "")
	_, reused := bootstrap(a, tokenA)
	if a.Value == b.Value || tokenA == tokenB || reused != tokenA {
		t.Fatal("browser sessions are not isolated or stable")
	}
	request := func(path string, cookie *http.Cookie, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+path, bytes.NewBufferString("{}"))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/api/connect", "/api/disconnect", "/api/databases", "/api/tables", "/api/structure", "/api/rows", "/api/query"} {
		if got := request(path, a, ""); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), `"error"`) {
			t.Errorf("%s without CSRF: status %d, body %s", path, got.Code, got.Body.String())
		}
	}
	if got := request("/api/disconnect", a, tokenB); got.Code != http.StatusForbidden {
		t.Errorf("another session's token accepted: %d", got.Code)
	}
	if got := request("/api/disconnect", nil, tokenA); got.Code != http.StatusForbidden {
		t.Errorf("token without cookie accepted: %d", got.Code)
	}
	if got := request("/api/disconnect", a, tokenA); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"connected":false`) {
		t.Errorf("valid session rejected: %d %s", got.Code, got.Body.String())
	}
}

func TestExecutableRejectsNonLoopbackListenAddress(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "dbkid-test")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// TEST-NET-1 is not a local interface; this test must never open a public listener.
	out, err := exec.CommandContext(ctx, binary, "-addr", "192.0.2.1:0").CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(strings.ToLower(string(out)), "loopback") {
		t.Fatalf("non-loopback address must be rejected before listen; output %q, error %v", out, err)
	}
}

func TestPostRequiresBoundedJSON(t *testing.T) {
	tests := []struct {
		name, contentType, body string
		want                    int
	}{
		{"form", "application/x-www-form-urlencoded", "a=b", http.StatusUnsupportedMediaType},
		{"missing content type", "", "{}", http.StatusUnsupportedMediaType},
		{"bodyless without content type", "", "", http.StatusUnsupportedMediaType},
		{"malformed", "application/json", "{", http.StatusBadRequest},
		{"multiple objects", "application/json", "{} {}", http.StatusBadRequest},
		{"oversized", "application/json", "{\"x\":\"" + strings.Repeat("x", 1<<20) + "\"}", http.StatusRequestEntityTooLarge},
		{"valid JSON", "application/json; charset=utf-8", "{}", http.StatusOK},
		{"empty disconnect", "application/json", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer()
			browser := bootstrapTestBrowser(t, s)
			req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/disconnect", strings.NewReader(tt.body))
			req.AddCookie(browser.cookie)
			req.Header.Set("X-CSRF-Token", browser.token)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want != http.StatusOK {
				stillValid := browser.request(s, http.MethodGet, "/api/session", "")
				if stillValid.Code != http.StatusOK || len(stillValid.Result().Cookies()) != 0 || !strings.Contains(stillValid.Body.String(), browser.token) {
					t.Fatal("invalid disconnect request retired the session")
				}
			}
		})
	}
}

func TestConnectRejectsUnacknowledgedRemotePlaintext(t *testing.T) {
	s := newServer()
	boot := httptest.NewRecorder()
	s.ServeHTTP(boot, httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil))
	var body struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &body); err != nil || len(boot.Result().Cookies()) != 1 {
		t.Fatal("cannot create test session")
	}
	for _, input := range []string{
		`{"host":"192.0.2.1","port":3306,"user":"test","password":"","tls":"disable","allow_insecure":false}`,
		`{"host":"127.0.0.1","port":3306,"user":"test","password":"","tls":"skip-verify"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/connect", strings.NewReader(input))
		req.AddCookie(boot.Result().Cookies()[0])
		req.Header.Set("X-CSRF-Token", body.Token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("unsafe connect must be rejected before dialing: status %d, body %s", rec.Code, rec.Body.String())
		}
	}
}

func TestExecutableServesEmbeddedAssetsOutsideProject(t *testing.T) {
	workdir := t.TempDir()
	binary := filepath.Join(workdir, "dbkid-test")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()
	process := exec.Command(binary, "-addr", address)
	process.Dir = workdir
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	client := &http.Client{Timeout: 300 * time.Millisecond}
	var response *http.Response
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		response, err = client.Get("http://" + address + "/")
		if err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not start: %v", err)
	}
	response.Body.Close()
	for _, tt := range []struct{ path, mime string }{
		{"/", "text/html"}, {"/style.css", "text/css"}, {"/app.js", "javascript"},
	} {
		res, err := client.Get("http://" + address + tt.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || len(body) == 0 || !strings.Contains(res.Header.Get("Content-Type"), tt.mime) {
			t.Errorf("embedded %s: status %d, length %d, MIME %q", tt.path, res.StatusCode, len(body), res.Header.Get("Content-Type"))
		}
	}
	for _, path := range []string{"/placeholder.txt", "/app.test.js"} {
		res, err := client.Get("http://" + address + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("unexpected static file %s served", path)
		}
	}
}

func TestSessionCreationIsBounded(t *testing.T) {
	s := newServer()
	var firstCookie *http.Cookie
	var firstToken string
	for i := 0; i < 1024; i++ {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("session %d: status %d", i, rec.Code)
		}
		if i == 0 {
			firstCookie = rec.Result().Cookies()[0]
			var body struct {
				Token string `json:"csrf_token"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Token == "" {
				t.Fatal("first session has no CSRF token")
			}
			firstToken = body.Token
		}
	}
	refused := httptest.NewRecorder()
	s.ServeHTTP(refused, httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil))
	if refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("unbounded new sessions: status %d", refused.Code)
	}
	for _, token := range []string{"", firstToken[:len(firstToken)-1] + "!"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil)
		req.AddCookie(firstCookie)
		if token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || len(rec.Result().Cookies()) != 0 || strings.Contains(rec.Body.String(), firstToken) {
			t.Fatalf("at capacity, unauthenticated replay must not expose the session: %d", rec.Code)
		}
	}
	first := testBrowser{cookie: firstCookie, token: firstToken}
	reused := first.request(s, http.MethodGet, "/api/session", "")
	if reused.Code != http.StatusOK || len(reused.Result().Cookies()) != 0 || !strings.Contains(reused.Body.String(), firstToken) {
		t.Fatalf("authenticated existing session should remain usable at capacity: %d", reused.Code)
	}
	if disconnected := first.request(s, http.MethodPost, "/api/disconnect", "{}"); disconnected.Code != http.StatusOK {
		t.Fatalf("disconnect at capacity failed: %d", disconnected.Code)
	}
	replacement := httptest.NewRecorder()
	s.ServeHTTP(replacement, httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil))
	if replacement.Code != http.StatusOK || len(replacement.Result().Cookies()) != 1 {
		t.Fatalf("disconnect did not free a session slot: %d", replacement.Code)
	}
}

func TestSensitiveResponsesHaveSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	newServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/session", nil))
	header := rec.Header()
	if header.Get("X-Frame-Options") != "DENY" || !strings.Contains(header.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Referrer-Policy") != "no-referrer" || header.Get("Cache-Control") != "no-store" {
		t.Fatal("missing required security headers")
	}
}
