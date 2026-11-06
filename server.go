package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const sessionCookieName = "dbkid_session"
const maxJSONBodyBytes = 1 << 20
const maxSessions = 1024
const maxQueryRequests = 4096
const sessionLifetime = 24 * time.Hour
const maxHistoryEntries = 100
const maxRecentConnections = 5
const maxPendingTokens = 32
const downloadTokenTTL = 60 * time.Second

//go:embed web/index.html web/style.css web/app.js
var embeddedWeb embed.FS

type session struct {
	mu           sync.RWMutex
	csrf         string // protected by server.mu
	db           *sql.DB
	host         string
	port         int
	user         string
	version      string
	generation   uint64
	queryIDs     map[string]struct{} // guarded by mu; prevents SQL request replay
	history      []historyEntry      // guarded by mu; successful SQL only
	blobTokens   map[string]blobRequest
	exportTokens map[string]exportRequest
	srv          *server // back-reference for process-wide recents
	retired      bool
	lastActive   time.Time // protected by server.mu
}

type server struct {
	mu           sync.Mutex
	sessions     map[string]*session
	recents      []recentConnection // guarded by mu; process-wide, never passwords
	openDatabase func(context.Context, connectRequest) (*sql.DB, string, error)
	now          func() time.Time
}

func newServer() *server {
	return &server{sessions: make(map[string]*session), openDatabase: openDatabase, now: time.Now}
}

func (s *server) setOpenDatabase(open func(context.Context, connectRequest) (*sql.DB, string, error)) {
	s.openDatabase = open
}

func (s *server) setClock(now func() time.Time) {
	s.now = now
}

// retire is called with server.mu held; it also invalidates in-flight connects.
func (current *session) retire() *sql.DB {
	current.mu.Lock()
	defer current.mu.Unlock()
	current.generation++
	current.retired = true
	current.csrf = ""
	current.queryIDs = nil
	current.history = nil
	current.blobTokens = nil
	current.exportTokens = nil
	db := current.db
	current.db, current.host, current.port, current.user, current.version = nil, "", 0, "", ""
	return db
}

type connectionInfo struct {
	Connected     bool   `json:"connected"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	ServerVersion string `json:"server_version"`
	Generation    uint64 `json:"generation"`
}

type queryRequest struct {
	Database  string `json:"database"`
	SQL       string `json:"sql"`
	Confirmed bool   `json:"confirmed"`
}

type historyEntry struct {
	SQL        string `json:"sql"`
	At         int64  `json:"at"`
	DurationMS int64  `json:"duration_ms"`
	Rows       *int64 `json:"rows,omitempty"`
	Affected   *int64 `json:"affected,omitempty"`
}

type recentConnection struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	TLS  string `json:"tls"`
}

type blobRequest struct {
	Database string
	Table    string
	Column   string
	Key      [][2]string
	Expires  time.Time
}

type exportRequest struct {
	Database string
	Table    string
	Format   string
	What     string
	Sort     string
	Dir      string
	Filter   string
	Expires  time.Time
}

func historyFromQuery(sql string, result queryResult) historyEntry {
	entry := historyEntry{SQL: sql, At: time.Now().UnixMilli(), DurationMS: result.DurationMS, Affected: result.AffectedRows}
	if len(result.Columns) > 0 {
		count := int64(len(result.Rows))
		entry.Rows = &count
	}
	if len(entry.SQL) > 16<<10 {
		entry.SQL = entry.SQL[:16<<10] + " …[truncated]"
	}
	return entry
}

func (current *session) recordHistory(entry historyEntry) {
	current.mu.Lock()
	defer current.mu.Unlock()
	current.history = append(current.history, entry)
	if excess := len(current.history) - maxHistoryEntries; excess > 0 {
		current.history = current.history[excess:]
	}
}

func (current *session) clearHistory() {
	current.mu.Lock()
	current.history = nil
	current.mu.Unlock()
}

func (current *session) historySnapshot() []historyEntry {
	current.mu.RLock()
	defer current.mu.RUnlock()
	result := make([]historyEntry, len(current.history))
	copy(result, current.history)
	return result
}

func (s *server) recordRecent(recent recentConnection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, existing := range s.recents {
		if existing == recent {
			s.recents = append(s.recents[:index], s.recents[index+1:]...)
			break
		}
	}
	s.recents = append([]recentConnection{recent}, s.recents...)
	if len(s.recents) > maxRecentConnections {
		s.recents = s.recents[:maxRecentConnections]
	}
}

func (s *server) recentSnapshot() []recentConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]recentConnection, len(s.recents))
	copy(result, s.recents)
	return result
}

func (current *session) issueBlobToken(request blobRequest) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	current.mu.Lock()
	defer current.mu.Unlock()
	for id, existing := range current.blobTokens {
		if now.After(existing.Expires) {
			delete(current.blobTokens, id)
		}
	}
	if len(current.blobTokens) >= maxPendingTokens {
		oldestID, oldest := "", time.Time{}
		for id, existing := range current.blobTokens {
			if oldest.IsZero() || existing.Expires.Before(oldest) {
				oldestID, oldest = id, existing.Expires
			}
		}
		if oldestID != "" {
			delete(current.blobTokens, oldestID)
		}
	}
	request.Expires = now.Add(downloadTokenTTL)
	if current.blobTokens == nil {
		current.blobTokens = make(map[string]blobRequest)
	}
	current.blobTokens[token] = request
	return token, nil
}

func (current *session) consumeBlobToken(token string) (blobRequest, bool) {
	current.mu.Lock()
	defer current.mu.Unlock()
	request, ok := current.blobTokens[token]
	if ok {
		delete(current.blobTokens, token)
	}
	if !ok || time.Now().After(request.Expires) {
		return blobRequest{}, false
	}
	return request, true
}

func (current *session) issueExportToken(request exportRequest) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	current.mu.Lock()
	defer current.mu.Unlock()
	for id, existing := range current.exportTokens {
		if now.After(existing.Expires) {
			delete(current.exportTokens, id)
		}
	}
	if len(current.exportTokens) >= maxPendingTokens {
		oldestID, oldest := "", time.Time{}
		for id, existing := range current.exportTokens {
			if oldest.IsZero() || existing.Expires.Before(oldest) {
				oldestID, oldest = id, existing.Expires
			}
		}
		if oldestID != "" {
			delete(current.exportTokens, oldestID)
		}
	}
	request.Expires = now.Add(downloadTokenTTL)
	if current.exportTokens == nil {
		current.exportTokens = make(map[string]exportRequest)
	}
	current.exportTokens[token] = request
	return token, nil
}

func (current *session) consumeExportToken(token string) (exportRequest, bool) {
	current.mu.Lock()
	defer current.mu.Unlock()
	request, ok := current.exportTokens[token]
	if ok {
		delete(current.exportTokens, token)
	}
	if !ok || time.Now().After(request.Expires) {
		return exportRequest{}, false
	}
	return request, true
}

// writeGate enforces the same confirmation, replay-protection and liveness rules
// as /api/query for every v2 mutation. Call it after decoding the request body.
func writeGate(w http.ResponseWriter, r *http.Request, current *session, confirmed bool) bool {
	_, ok := writeGateCapture(w, r, current, confirmed, nil)
	return ok
}

// Use the same gate for replacement, and capture the database and validate its
// generation under the gate mutex, so reconnect cannot change the target.
func writeGateCapture(w http.ResponseWriter, r *http.Request, current *session, confirmed bool, generation *uint64) (*sql.DB, bool) {
	if !confirmed {
		writeError(w, http.StatusBadRequest, "explicit SQL confirmation is required")
		return nil, false
	}
	ids := r.Header.Values("X-DBKid-Request-ID")
	if len(ids) != 1 || !validQueryRequestID(ids[0]) {
		writeError(w, http.StatusBadRequest, "a unique SQL request ID is required")
		return nil, false
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if !writeGateLocked(w, current, ids[0]) {
		return nil, false
	}
	if generation != nil && current.generation != *generation {
		writeError(w, http.StatusConflict, "connection generation changed")
		return nil, false
	}
	return current.db, true
}

// Caller holds current.mu so generation validation and database capture can
// be performed in the same critical section as the replay gate.
func writeGateLocked(w http.ResponseWriter, current *session, id string) bool {
	if _, exists := current.queryIDs[id]; exists {
		writeJSON(w, http.StatusConflict, map[string]string{"stage": "replay_unknown", "error": "SQL request already submitted; its outcome may be unknown. Inspect data before retrying"})
		return false
	}
	if current.retired || current.db == nil {
		writeError(w, http.StatusConflict, "not connected")
		return false
	}
	if len(current.queryIDs) >= maxQueryRequests {
		writeError(w, http.StatusTooManyRequests, "session SQL request limit reached; disconnect and reconnect")
		return false
	}
	if current.queryIDs == nil {
		current.queryIDs = make(map[string]struct{})
	}
	current.queryIDs[id] = struct{}{}
	return true
}

func (current *session) info() connectionInfo {
	current.mu.RLock()
	defer current.mu.RUnlock()
	return connectionInfo{current.db != nil, current.host, current.port, current.user, current.version, current.generation}
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func validQueryRequestID(id string) bool {
	if len(id) < 22 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}, allowEmpty bool) bool {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		writeError(w, http.StatusUnsupportedMediaType, "application/json is required")
		return false
	}
	mediaType, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		writeError(w, http.StatusUnsupportedMediaType, "application/json is required")
		return false
	}
	if r.ContentLength > maxJSONBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	if err != nil {
		if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 && allowEmpty {
		return true
	}
	if len(body) == 0 || body[0] != '{' || !utf8.Valid(body) {
		writeError(w, http.StatusBadRequest, "a JSON object is required")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON object")
		return false
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON object")
		return false
	}
	return true
}

func loopbackAuthority(authority string) bool {
	if authority == "" || strings.ContainsAny(authority, " 	\r\n/@?#%") {
		return false
	}
	host, port := authority, ""
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return false
		}
		host = authority[1:end]
		if rest := authority[end+1:]; rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return false
			}
			port = rest[1:]
			if port == "" {
				return false
			}
		}
	} else if strings.Contains(authority, ":") {
		var err error
		host, port, err = net.SplitHostPort(authority)
		if err != nil || port == "" {
			return false
		}
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validRequestOrigin(r *http.Request) bool {
	if !loopbackAuthority(r.Host) {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	u, err := url.Parse(origins[0])
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if !validRequestOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid host or origin")
		return
	}
	if r.URL.Path == "/api/session" {
		s.handleSession(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		serveStatic(w, r)
		return
	}
	tokens := r.Header.Values("X-CSRF-Token")
	var current *session
	var sessionID string
	var expiredDB *sql.DB
	validToken := false
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.mu.Lock()
		current = s.sessions[cookie.Value]
		now := s.now()
		if current != nil && now.Sub(current.lastActive) >= sessionLifetime {
			delete(s.sessions, cookie.Value)
			expiredDB = current.retire()
			current = nil
		} else if current != nil && len(tokens) == 1 && subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(current.csrf)) == 1 {
			validToken = true
			sessionID = cookie.Value
			current.lastActive = now
		}
		s.mu.Unlock()
	}
	if expiredDB != nil {
		_ = expiredDB.Close()
	}
	// /api/blob and /api/export are the only CSRF-exempt paths: they are
	// same-origin downloads authorized by a single-use unguessable token that
	// was issued through a CSRF-protected POST. The token is the capability.
	downloadPath := r.Method == http.MethodGet && (r.URL.Path == "/api/blob" || r.URL.Path == "/api/export")
	if !validToken && !(downloadPath && current != nil) {
		writeError(w, http.StatusForbidden, "invalid session or CSRF token")
		return
	}
	if r.URL.Path == "/api/connect" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var request connectRequest
		if !decodeJSON(w, r, &request, false) {
			return
		}
		if message := validateConnect(&request); message != "" {
			writeError(w, http.StatusBadRequest, message)
			return
		}
		current.mu.Lock()
		if current.retired {
			current.mu.Unlock()
			writeError(w, http.StatusForbidden, "session expired")
			return
		}
		current.generation++
		generation := current.generation
		current.mu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		db, version, err := s.openDatabase(ctx, request)
		if err != nil || db == nil {
			if db != nil {
				_ = db.Close()
			}
			writeError(w, http.StatusBadGateway, "database connection failed")
			return
		}
		current.mu.Lock()
		if current.generation != generation || ctx.Err() != nil {
			current.mu.Unlock()
			_ = db.Close()
			writeError(w, http.StatusConflict, "connection request was superseded")
			return
		}
		old := current.db
		// A session read while this connect was opening may have seen the old
		// database with the attempt's generation. Give the published database a
		// distinct generation so that token cannot authorize the new target.
		current.generation++
		current.db, current.host, current.port, current.user, current.version = db, request.Host, request.Port, request.User, version
		info := connectionInfo{true, request.Host, request.Port, request.User, version, current.generation}
		current.mu.Unlock()
		s.recordRecent(recentConnection{Host: request.Host, Port: request.Port, User: request.User, TLS: request.TLS})
		if old != nil {
			_ = old.Close()
		}
		writeJSON(w, http.StatusOK, info)
		return
	}
	if r.URL.Path == "/api/disconnect" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !decodeJSON(w, r, &struct{}{}, true) {
			return
		}
		s.mu.Lock()
		if s.sessions[sessionID] != current {
			s.mu.Unlock()
			writeError(w, http.StatusForbidden, "invalid session or CSRF token")
			return
		}
		delete(s.sessions, sessionID)
		old := current.retire()
		s.mu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"connected": false})
		return
	}
	if r.URL.Path == "/api/databases" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		current.mu.RLock()
		db := current.db
		current.mu.RUnlock()
		if db == nil {
			writeError(w, http.StatusConflict, "not connected")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		databases, err := listDatabases(ctx, db)
		if err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Databases []string `json:"databases"`
		}{databases})
		return
	}
	if r.URL.Path == "/api/tables" || r.URL.Path == "/api/structure" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		database := r.URL.Query().Get("database")
		table := r.URL.Query().Get("table")
		if !validIdentifier(database) || (r.URL.Path == "/api/structure" && !validIdentifier(table)) {
			writeError(w, http.StatusBadRequest, "database and table names are required")
			return
		}
		current.mu.RLock()
		db := current.db
		current.mu.RUnlock()
		if db == nil {
			writeError(w, http.StatusConflict, "not connected")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if r.URL.Path == "/api/tables" {
			tables, err := listTables(ctx, db, database)
			if err != nil {
				writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
				return
			}
			writeJSON(w, http.StatusOK, struct {
				Tables []tableInfo `json:"tables"`
			}{tables})
			return
		}
		columns, err := listColumns(ctx, db, database, table)
		if err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Columns []columnInfo `json:"columns"`
		}{columns})
		return
	}
	if r.URL.Path == "/api/rows" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		values := r.URL.Query()
		database, table := values.Get("database"), values.Get("table")
		if !validIdentifier(database) || !validIdentifier(table) {
			writeError(w, http.StatusBadRequest, "database and table names are required")
			return
		}
		page, size := int64(1), int64(50)
		var err error
		if value := values.Get("page"); value != "" {
			page, err = strconv.ParseInt(value, 10, 64)
			if err != nil || page < 1 {
				writeError(w, http.StatusBadRequest, "invalid page")
				return
			}
		}
		if value := values.Get("page_size"); value != "" {
			size, err = strconv.ParseInt(value, 10, 64)
			if err != nil || size < 1 {
				writeError(w, http.StatusBadRequest, "invalid page size")
				return
			}
			if size > 200 {
				size = 200
			}
		}
		if page-1 > math.MaxInt64/size {
			writeError(w, http.StatusBadRequest, "page offset is too large")
			return
		}
		current.mu.RLock()
		db := current.db
		current.mu.RUnlock()
		if db == nil {
			writeError(w, http.StatusConflict, "not connected")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		result, err := browseRowsV2(ctx, db, database, table, page, size, (page-1)*size,
			r.URL.Query().Get("sort"), r.URL.Query().Get("dir"), r.URL.Query().Get("filter"))
		if errors.Is(err, errInvalidBrowse) {
			writeError(w, http.StatusBadRequest, "invalid sort or filter")
			return
		}
		if err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if r.URL.Path == "/api/query" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var request queryRequest
		if !decodeJSON(w, r, &request, false) {
			return
		}
		if !request.Confirmed {
			writeError(w, http.StatusBadRequest, "explicit SQL confirmation is required")
			return
		}
		if strings.TrimSpace(request.SQL) == "" || (request.Database != "" && !validIdentifier(request.Database)) {
			writeError(w, http.StatusBadRequest, "SQL and a valid database name are required")
			return
		}
		ids := r.Header.Values("X-DBKid-Request-ID")
		if len(ids) != 1 || !validQueryRequestID(ids[0]) {
			writeError(w, http.StatusBadRequest, "a unique SQL request ID is required")
			return
		}
		current.mu.Lock()
		if current.retired || current.db == nil {
			current.mu.Unlock()
			writeError(w, http.StatusConflict, "not connected")
			return
		}
		if _, exists := current.queryIDs[ids[0]]; exists {
			current.mu.Unlock()
			writeError(w, http.StatusConflict, "SQL request already submitted; its outcome may be unknown. Inspect data before retrying")
			return
		}
		if len(current.queryIDs) >= maxQueryRequests {
			current.mu.Unlock()
			writeError(w, http.StatusTooManyRequests, "session SQL request limit reached; disconnect and reconnect")
			return
		}
		if current.queryIDs == nil {
			current.queryIDs = make(map[string]struct{})
		}
		current.queryIDs[ids[0]] = struct{}{}
		db := current.db
		current.mu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		result, err := executeQuery(ctx, db, request.Database, request.SQL)
		if err != nil {
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		current.recordHistory(historyFromQuery(request.SQL, result))
		writeJSON(w, http.StatusOK, result)
		return
	}
	switch r.URL.Path {
	case "/api/database/create":
		handleDatabaseCreate(w, r, current)
		return
	case "/api/database/drop":
		handleDatabaseDrop(w, r, current)
		return
	case "/api/table/create":
		handleTableCreate(w, r, current)
		return
	case "/api/table/rename":
		handleTableRename(w, r, current)
		return
	case "/api/table/drop":
		handleTableDrop(w, r, current)
		return
	case "/api/column/add":
		handleColumnAdd(w, r, current)
		return
	case "/api/column/modify":
		handleColumnModify(w, r, current)
		return
	case "/api/column/drop":
		handleColumnDrop(w, r, current)
		return
	case "/api/index/create":
		handleIndexCreate(w, r, current)
		return
	case "/api/index/drop":
		handleIndexDrop(w, r, current)
		return
	case "/api/row/insert":
		handleRowInsert(w, r, current)
		return
	case "/api/row/update":
		handleRowUpdate(w, r, current)
		return
	case "/api/row/delete":
		handleRowDelete(w, r, current)
		return
	case "/api/routine/drop":
		handleRoutineDrop(w, r, current)
		return
	case "/api/process/kill":
		handleProcessKill(w, r, current)
		return
	case "/api/foreign-keys":
		handleForeignKeys(w, r, current)
		return
	case "/api/foreign-key/add":
		handleForeignKeyAdd(w, r, current)
		return
	case "/api/foreign-key/drop":
		handleForeignKeyDrop(w, r, current)
		return
	case "/api/users":
		handleUsers(w, r, current)
		return
	case "/api/grants":
		handleGrants(w, r, current)
		return
	case "/api/user/create":
		handleUserCreate(w, r, current)
		return
	case "/api/user/password":
		handleUserPassword(w, r, current)
		return
	case "/api/user/drop":
		handleUserDrop(w, r, current)
		return
	case "/api/grant":
		handleGrant(w, r, current)
		return
	case "/api/revoke":
		handleRevoke(w, r, current)
		return
	case "/api/import/sql":
		handleImportSQL(w, r, current)
		return
	case "/api/import/csv":
		handleImportCSV(w, r, current)
		return
	case "/api/row":
		handleRowFetch(w, r, current)
		return
	case "/api/indexes":
		handleIndexes(w, r, current)
		return
	case "/api/routines":
		handleRoutines(w, r, current)
		return
	case "/api/triggers":
		handleTriggers(w, r, current)
		return
	case "/api/events":
		handleEvents(w, r, current)
		return
	case "/api/routine":
		handleRoutineShow(w, r, current)
		return
	case "/api/object/create":
		handleObjectCreate(w, r, current)
		return
	case "/api/object/replace":
		handleObjectReplace(w, r, current)
		return
	case "/api/object/definition":
		handleObjectDefinition(w, r, current)
		return
	case "/api/search":
		handleSearch(w, r, current)
		return
	case "/api/status":
		handleStatus(w, r, current)
		return
	case "/api/history/clear":
		handleHistory(w, r, current)
		return
	case "/api/history":
		handleHistory(w, r, current)
		return
	case "/api/recent":
		handleRecent(w, r, current)
		return
	case "/api/blob/token":
		handleBlobToken(w, r, current)
		return
	case "/api/blob":
		handleBlob(w, r, current)
		return
	case "/api/export/token":
		handleExportToken(w, r, current)
		return
	case "/api/export":
		handleExport(w, r, current)
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

func serveStatic(w http.ResponseWriter, r *http.Request) {
	name, contentType := "", ""
	switch r.URL.Path {
	case "/":
		name, contentType = "web/index.html", "text/html; charset=utf-8"
	case "/style.css":
		name, contentType = "web/style.css", "text/css; charset=utf-8"
	case "/app.js":
		name, contentType = "web/app.js", "application/javascript; charset=utf-8"
	default:
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	content, err := embeddedWeb.ReadFile(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(content))
}

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.Lock()
	now := s.now()
	var expired []*sql.DB
	for id, candidate := range s.sessions {
		if now.Sub(candidate.lastActive) >= sessionLifetime {
			delete(s.sessions, id)
			if db := candidate.retire(); db != nil {
				expired = append(expired, db)
			}
		}
	}
	defer func() {
		for _, db := range expired {
			_ = db.Close()
		}
	}()
	var current *session
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		candidate := s.sessions[cookie.Value]
		tokens := r.Header.Values("X-CSRF-Token")
		// Cookies are shared across ports; the token proves access to this session.
		if candidate != nil && len(tokens) == 1 && subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(candidate.csrf)) == 1 {
			current = candidate
		}
	}
	if current == nil {
		if len(s.sessions) >= maxSessions {
			s.mu.Unlock()
			writeError(w, http.StatusServiceUnavailable, "too many active sessions")
			return
		}
		id, err := randomToken()
		if err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "unable to create session")
			return
		}
		csrf, err := randomToken()
		if err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "unable to create session")
			return
		}
		current = &session{csrf: csrf, lastActive: now, srv: s}
		s.sessions[id] = current
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	} else {
		current.lastActive = now
	}
	csrf := current.csrf
	s.mu.Unlock()
	info := current.info()
	writeJSON(w, http.StatusOK, struct {
		CSRFToken     string `json:"csrf_token"`
		Connected     bool   `json:"connected"`
		Host          string `json:"host"`
		Port          int    `json:"port"`
		User          string `json:"user"`
		ServerVersion string `json:"server_version"`
		Generation    uint64 `json:"generation"`
	}{csrf, info.Connected, info.Host, info.Port, info.User, info.ServerVersion, info.Generation})
}
