package main

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

var pngMagic = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
var jpegMagic = []byte{0xFF, 0xD8, 0xFF, 0xE0}
var gifMagic = []byte("GIF89a")
var webpMagic = append([]byte("RIFF\x24\x00\x00\x00"), []byte("WEBPVP8 ")...)
var pdfMagic = []byte("%PDF-1.7\n%dbkid")

func TestSniffBlob(t *testing.T) {
	cases := []struct {
		name        string
		value       []byte
		contentType string
		disposition string
		filename    string
	}{
		{"png", pngMagic, "image/png", "inline", "blob.png"},
		{"jpeg", jpegMagic, "image/jpeg", "inline", "blob.jpg"},
		{"gif", gifMagic, "image/gif", "inline", "blob.gif"},
		{"webp", webpMagic, "image/webp", "inline", "blob.webp"},
		{"pdf", pdfMagic, "application/pdf", "inline", "blob.pdf"},
		{"utf8 text", []byte("just some text"), "text/plain; charset=utf-8", "inline", "blob.txt"},
		{"empty", []byte{}, "text/plain; charset=utf-8", "inline", "blob.txt"},
		{"binary", []byte{0x00, 0xFF, 0x01, 0x02}, "application/octet-stream", "attachment", "blob.bin"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			contentType, disposition, filename := sniffBlob(test.value)
			if contentType != test.contentType || disposition != test.disposition || filename != test.filename {
				t.Fatalf("sniff(%s) = (%q, %q, %q), want (%q, %q, %q)",
					test.name, contentType, disposition, filename,
					test.contentType, test.disposition, test.filename)
			}
		})
	}
	// WebP requires both the RIFF tag and the WEBP marker at offset 8; the
	// junk bytes are still valid UTF-8, so they fall through to text/plain.
	if contentType, disposition, _ := sniffBlob([]byte("RIFF\x00\x00\x00\x00JUNKJUNK")); disposition != "inline" || contentType != "text/plain; charset=utf-8" {
		t.Fatalf("RIFF without WEBP marker must not be treated as an image: %q %q", contentType, disposition)
	}
	// A truncated magic that is not valid UTF-8 must download as an attachment,
	// never render inline.
	if contentType, disposition, _ := sniffBlob(pngMagic[:3]); disposition != "attachment" || contentType != "application/octet-stream" {
		t.Fatalf("truncated invalid-UTF-8 magic must be an attachment, got (%q, %q)", contentType, disposition)
	}
}

func TestBlobTokenFlowIsSingleUseAndTypeSafe(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS") {
			return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, [][]driver.Value{
				{"id", "int", "int", "NO", "PRI", nil, ""},
				{"data", "varbinary", "varbinary(200)", "YES", "", nil, ""},
			}, nil
		}
		if query == "SELECT `data` FROM `db`.`blobs` WHERE (`id` = ?) LIMIT 1" {
			if len(args) != 1 || args[0].Value != "1" {
				return nil, nil, fmt.Errorf("unexpected blob fetch arguments: %#v", args)
			}
			return []string{"data"}, [][]driver.Value{{pngMagic}}, nil
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)

	// Token issuance validates the column family and the primary key.
	rec := b.request(s, http.MethodPost, "/api/blob/token", `{"database":"db","table":"blobs","column":"data","key":[["id","1"]]}`)
	var issued struct {
		URL string `json:"url"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &issued) != nil || !strings.HasPrefix(issued.URL, "/api/blob?token=") {
		t.Fatalf("blob token issuance failed: %d %s", rec.Code, rec.Body.String())
	}

	// The download works without a CSRF header: the token is the capability.
	req, _ := http.NewRequest(http.MethodGet, "http://localhost:8080"+issued.URL, nil)
	req.AddCookie(b.cookie)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("blob download failed: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("blob content type wrong: %q", recorder.Header().Get("Content-Type"))
	}
	if got := recorder.Header().Get("Content-Disposition"); got != `inline; filename="blob.png"` {
		t.Fatalf("blob disposition wrong: %q", got)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("blob download must not be cacheable")
	}
	if !bytes.Equal(recorder.Body.Bytes(), pngMagic) {
		t.Fatalf("blob bytes wrong: %x", recorder.Body.Bytes())
	}

	// Single use: the second download is rejected.
	req2, _ := http.NewRequest(http.MethodGet, "http://localhost:8080"+issued.URL, nil)
	req2.AddCookie(b.cookie)
	recorder2 := httptest.NewRecorder()
	s.ServeHTTP(recorder2, req2)
	if recorder2.Code != http.StatusForbidden || !strings.Contains(recorder2.Body.String(), "invalid or expired download token") {
		t.Fatalf("second download must 403, got %d %s", recorder2.Code, recorder2.Body.String())
	}

	// Without any token parameter the request is forbidden too.
	req3, _ := http.NewRequest(http.MethodGet, "http://localhost:8080/api/blob", nil)
	req3.AddCookie(b.cookie)
	recorder3 := httptest.NewRecorder()
	s.ServeHTTP(recorder3, req3)
	if recorder3.Code != http.StatusForbidden {
		t.Fatalf("tokenless download must 403, got %d", recorder3.Code)
	}
}

func TestBlobTokenRejectsInvalidRequests(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS") {
			if args[1].Value == "items" {
				return columnsFixture("items")
			}
			if args[1].Value == "blobs" {
				return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, [][]driver.Value{
					{"id", "int", "int", "NO", "PRI", nil, ""},
					{"data", "varbinary", "varbinary(200)", "YES", "", nil, ""},
				}, nil
			}
			return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected query: %s", query)
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{"unknown column", `{"database":"db","table":"blobs","column":"missing","key":[["id","1"]]}`, "unknown column"},
		{"not binary", `{"database":"db","table":"items","column":"label","key":[["id","1"]]}`, "a binary column is required"},
		{"missing key", `{"database":"db","table":"blobs","column":"data"}`, "a primary key is required"},
		{"wrong key column", `{"database":"db","table":"blobs","column":"data","key":[["data","ff"]]}`, "a primary key is required"},
		{"unknown table", `{"database":"db","table":"missing","column":"data","key":[["id","1"]]}`, "unknown table"},
		{"bad json", `{`, "invalid JSON object"},
		{"unknown field", `{"database":"db","table":"blobs","column":"data","key":[["id","1"]],"evil":1}`, "invalid JSON object"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rec := b.request(s, http.MethodPost, "/api/blob/token", test.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), test.message) {
				t.Fatalf("expected 400 %q, got %d %s", test.message, rec.Code, rec.Body.String())
			}
		})
	}
	if rec := b.request(s, http.MethodGet, "/api/blob/token?database=db", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("blob token GET must 405, got %d", rec.Code)
	}
}

func TestBlobDownloadRejectsMissingRows(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS") {
			return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, [][]driver.Value{
				{"id", "int", "int", "NO", "PRI", nil, ""},
				{"data", "varbinary", "varbinary(200)", "YES", "", nil, ""},
			}, nil
		}
		return []string{"data"}, nil, nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodPost, "/api/blob/token", `{"database":"db","table":"blobs","column":"data","key":[["id","9"]]}`)
	var issued struct {
		URL string `json:"url"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &issued) != nil {
		t.Fatalf("blob token issuance failed: %d %s", rec.Code, rec.Body.String())
	}
	req, _ := http.NewRequest(http.MethodGet, "http://localhost:8080"+issued.URL, nil)
	req.AddCookie(b.cookie)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "row not found") {
		t.Fatalf("missing blob row must 404, got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestBlobDownloadOversizedValueIsRejected(t *testing.T) {
	engine := &fakeEngine{}
	engine.query = func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.Contains(query, "FROM INFORMATION_SCHEMA.COLUMNS") {
			return []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA"}, [][]driver.Value{
				{"id", "int", "int", "NO", "PRI", nil, ""},
				{"data", "varbinary", "varbinary(200)", "YES", "", nil, ""},
			}, nil
		}
		return []string{"data"}, [][]driver.Value{{bytes.Repeat([]byte{0x41}, maxBlobBytes+1)}}, nil
	}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	rec := b.request(s, http.MethodPost, "/api/blob/token", `{"database":"db","table":"blobs","column":"data","key":[["id","1"]]}`)
	var issued struct {
		URL string `json:"url"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &issued) != nil {
		t.Fatalf("blob token issuance failed: %d %s", rec.Code, rec.Body.String())
	}
	req, _ := http.NewRequest(http.MethodGet, "http://localhost:8080"+issued.URL, nil)
	req.AddCookie(b.cookie)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "blob exceeds the size limit") {
		t.Fatalf("oversized blob must 413, got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestBlobURLTokenIsUnguessableBase64URL(t *testing.T) {
	current := &session{}
	token, err := current.issueBlobToken(blobRequest{Database: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 43 || strings.ContainsAny(token, "+/=") {
		t.Fatalf("token must be 32-byte base64url: %q", token)
	}
	if _, err := url.Parse("/api/blob?token=" + token); err != nil {
		t.Fatal(err)
	}
}
