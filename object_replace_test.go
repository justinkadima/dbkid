package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestObjectReplaceRejectsInvalidTargetBeforeDB(t *testing.T) {
	engine := &fakeEngine{}
	s := newServer()
	b := connectFakeDatabase(t, s, engine)
	body := `{"database":"db","name":"p","kind":"procedure","sql":"CREATE PROCEDURE q() SELECT 1","confirmed":true}`
	rec := b.mutation(s, "/api/object/replace", body)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not_started") || recordCount(engine) != 0 {
		t.Fatalf("status=%d body=%s calls=%d", rec.Code, rec.Body.String(), recordCount(engine))
	}
}
