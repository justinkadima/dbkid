package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// objectHeaderLexer recognizes only a deliberately restricted CREATE header.
// It does not interpret the program body. MySQL multi-statements are disabled
// on every production connection; a stored-program body can contain semicolons.
type objectHeaderLexer struct {
	text string
	pos  int
}

func (p *objectHeaderLexer) space() {
	for p.pos < len(p.text) && strings.ContainsRune(" \t\r\n", rune(p.text[p.pos])) {
		p.pos++
	}
}
func (p *objectHeaderLexer) symbol(c byte) bool {
	p.space()
	if p.pos >= len(p.text) || p.text[p.pos] != c {
		return false
	}
	p.pos++
	return true
}
func (p *objectHeaderLexer) identifier() (string, bool) {
	p.space()
	if p.pos >= len(p.text) {
		return "", false
	}
	if p.text[p.pos] == '`' {
		p.pos++
		var b strings.Builder
		for p.pos < len(p.text) {
			c := p.text[p.pos]
			p.pos++
			if c == '\\' || c == 0 {
				return "", false
			}
			if c == '`' {
				if p.pos < len(p.text) && p.text[p.pos] == '`' {
					b.WriteByte('`')
					p.pos++
					continue
				}
				return b.String(), b.Len() > 0
			}
			b.WriteByte(c)
		}
		return "", false
	}
	start := p.pos
	for p.pos < len(p.text) {
		c := p.text[p.pos]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$') {
			break
		}
		p.pos++
	}
	return p.text[start:p.pos], p.pos > start
}
func (p *objectHeaderLexer) keyword(want string) bool {
	p.space()
	// Keywords may not be quoted.
	if p.pos < len(p.text) && p.text[p.pos] == '`' {
		return false
	}
	word, ok := p.identifier()
	return ok && strings.EqualFold(word, want)
}

func objectCreateMatches(sql, database, name, kind string) bool {
	if strings.ContainsRune(sql, 0) {
		return false
	}
	p := objectHeaderLexer{text: sql}
	if !p.keyword("CREATE") {
		return false
	}
	// Optional SHOW CREATE definer; executable comments and quoted strings
	// in this portion are intentionally rejected rather than guessed.
	p.space()
	saved := p.pos
	if p.keyword("DEFINER") {
		if !p.symbol('=') {
			return false
		}
		if _, ok := p.identifier(); !ok || !p.symbol('@') {
			return false
		}
		if _, ok := p.identifier(); !ok {
			return false
		}
	} else {
		p.pos = saved
	}
	if !p.keyword(kind) {
		return false
	}
	p.space()
	quotedFirst := p.pos < len(sql) && sql[p.pos] == '`'
	first, ok := p.identifier()
	// IF NOT EXISTS is permitted by MySQL/MariaDB for some kinds. Do
	// not confuse its IF with the target, even when the requested name is IF.
	if !ok || !quotedFirst && strings.EqualFold(first, "IF") {
		return false
	}
	afterFirst := p.pos
	p.space()
	target := first
	if p.pos < len(sql) && sql[p.pos] == '.' {
		p.pos++
		target, ok = p.identifier()
		if !ok || first != database {
			return false
		}
	} else {
		p.pos = afterFirst
	}
	if target != name {
		return false
	}
	// Require the next grammar token; executable comments can otherwise
	// insert a schema qualifier after what looked like the complete name.
	switch kind {
	case "procedure":
		return p.symbol('(')
	case "event":
		return p.keyword("ON")
	case "trigger":
		saved = p.pos
		if p.keyword("BEFORE") {
			return true
		}
		p.pos = saved
		return p.keyword("AFTER")
	}
	return false
}

type objectCreateRequest struct {
	Database   string  `json:"database"`
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	SQL        string  `json:"sql"`
	Confirmed  bool    `json:"confirmed"`
	Generation *uint64 `json:"generation"`
}

func handleObjectCreate(w http.ResponseWriter, r *http.Request, current *session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input objectCreateRequest
	if !decodeJSON(w, r, &input, false) {
		return
	}
	if !input.Confirmed {
		writeError(w, http.StatusBadRequest, "explicit SQL confirmation is required")
		return
	}
	if input.Generation == nil {
		writeError(w, http.StatusBadRequest, "connection generation is required")
		return
	}
	if !validIdentifier(input.Database) || strings.ContainsRune(input.Database, '\\') || !validIdentifier(input.Name) ||
		(input.Kind != "procedure" && input.Kind != "trigger" && input.Kind != "event") ||
		!objectCreateMatches(input.SQL, input.Database, input.Name, input.Kind) {
		writeError(w, http.StatusBadRequest, "SQL must be a supported CREATE targeting the intended database, kind and name")
		return
	}
	db, ok := writeGateCapture(w, r, current, input.Confirmed, input.Generation)
	if !ok {
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
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdentifier(input.Database)); err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	result, err := conn.ExecContext(ctx, input.SQL)
	if err != nil {
		writeError(w, http.StatusBadGateway, databaseErrorMessage(budget.resultError(err)))
		return
	}
	// A successful CREATE can immediately disappear (e.g. an expired event).
	// Verify existence on the same bounded, pinned connection; this does not
	// establish equivalence to the submitted SQL or exclude concurrent DDL.
	if _, err := readObjectSnapshot(ctx, conn, budget, input.Database, input.Name, input.Kind, *input.Generation); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"stage": "created_unverified", "error": "CREATE completed, but the resulting object could not be verified. Inspect the server before retrying; no automatic retry was attempted."})
		return
	}
	response := map[string]interface{}{"stage": "created", "warning": "The object exists; its definition was not compared with the submitted SQL and concurrent DDL may race."}
	if affected, err := result.RowsAffected(); err == nil {
		response["affected_rows"] = affected
	}
	writeJSON(w, http.StatusOK, response)
}
