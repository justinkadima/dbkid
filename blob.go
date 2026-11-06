package main

import (
	"bytes"
	"context"
	"net/http"
	"time"
	"unicode/utf8"
)

// Blob downloads through single-use, short-lived tokens.
// Implemented per docs/plans/v2-features.md section B.

const maxBlobBytes = 2 << 20

type blobTokenRequest struct {
	Database  string      `json:"database"`
	Table     string      `json:"table"`
	Column    string      `json:"column"`
	Key       [][2]string `json:"key"`
	Confirmed bool        `json:"confirmed,omitempty"` // tolerated, ignored: read-only
}

func handleBlobToken(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request blobTokenRequest
	if !decodeJSON(w, r, &request, false) {
		return
	}
	if !validIdentifier(request.Database) || !validIdentifier(request.Table) || !validIdentifier(request.Column) {
		writeError(w, http.StatusBadRequest, "database, table and column names are required")
		return
	}
	if len(request.Key) == 0 {
		writeError(w, http.StatusBadRequest, "a primary key is required")
		return
	}
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	meta, err := tableColumnMeta(ctx, conn, request.Database, request.Table)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	if len(meta) == 0 {
		writeError(w, http.StatusBadRequest, "unknown table")
		return
	}
	var target *columnMeta
	for i := range meta {
		if meta[i].Name == request.Column {
			target = &meta[i]
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusBadRequest, "unknown column")
		return
	}
	if !isBinaryDataType(target.DataType) {
		writeError(w, http.StatusBadRequest, "a binary column is required")
		return
	}
	if _, _, ok, message := matchPrimaryKey(tablePrimaryKeys(meta), request.Key); !ok {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	token, err := current.issueBlobToken(blobRequest{
		Database: request.Database, Table: request.Table, Column: request.Column, Key: request.Key,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unable to create download token")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		URL string `json:"url"`
	}{"/api/blob?token=" + token})
}

func handleBlob(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	token := r.URL.Query().Get("token")
	db, connected := sessionDatabase(current)
	if !connected {
		writeError(w, http.StatusConflict, "not connected")
		return
	}
	request, ok := current.consumeBlobToken(token)
	if !ok {
		writeError(w, http.StatusForbidden, "invalid or expired download token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conn, budget, err := requestConn(ctx, db)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	defer closeRequestConn(conn)
	meta, err := tableColumnMeta(ctx, conn, request.Database, request.Table)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	whereSQL, args, ok, message := matchPrimaryKey(tablePrimaryKeys(meta), request.Key)
	if !ok {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	query := "SELECT " + quoteIdentifier(request.Column) + " FROM " +
		quoteIdentifier(request.Database) + "." + quoteIdentifier(request.Table) + " WHERE " + whereSQL + " LIMIT 1"
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			budget.abort()
			_ = rows.Close()
			writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
			return
		}
		budget.abort() // a full primary key cannot repeat; never drain past it
		_ = rows.Close()
		writeError(w, http.StatusNotFound, "row not found")
		return
	}
	var value []byte
	if err := rows.Scan(&value); err != nil {
		budget.abort()
		_ = rows.Close()
		writeError(w, http.StatusBadGateway, databaseErrorMessage(err))
		return
	}
	budget.abort() // never read past the single fetched blob
	_ = rows.Close()
	if len(value) > maxBlobBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "blob exceeds the size limit")
		return
	}
	contentType, disposition, filename := sniffBlob(value)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", disposition+`; filename="`+sanitizeFilename(filename)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(value)
}

// sniffBlob classifies a blob for inline display: known image/document magics
// keep their content type and render inline; valid UTF-8 renders as text;
// anything else downloads as an octet-stream attachment.
func sniffBlob(value []byte) (string, string, string) {
	has := func(offset int, magic []byte) bool {
		return len(value) >= offset+len(magic) && bytes.Equal(value[offset:offset+len(magic)], magic)
	}
	switch {
	case has(0, []byte{0x89, 0x50, 0x4E, 0x47}):
		return "image/png", "inline", "blob.png"
	case has(0, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg", "inline", "blob.jpg"
	case has(0, []byte("GIF")):
		return "image/gif", "inline", "blob.gif"
	case has(0, []byte("RIFF")) && has(8, []byte("WEBP")):
		return "image/webp", "inline", "blob.webp"
	case has(0, []byte("%PDF")):
		return "application/pdf", "inline", "blob.pdf"
	case utf8.Valid(value):
		return "text/plain; charset=utf-8", "inline", "blob.txt"
	default:
		return "application/octet-stream", "attachment", "blob.bin"
	}
}

// sanitizeFilename strips header-unsafe bytes from a download filename.
func sanitizeFilename(name string) string {
	var clean bytes.Buffer
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f || c == '"' || c == '\\' {
			continue
		}
		clean.WriteByte(c)
	}
	return clean.String()
}
