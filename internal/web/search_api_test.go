package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestSearchAndNote(t *testing.T) {
	s, _ := cfgServer(t) // a vault with one note id "n", body "# N\nbody"
	h := s.Handler()

	// search requires q
	if code, _ := doJSON(t, h, "GET", "/api/search", ""); code != http.StatusBadRequest {
		t.Errorf("search without q = %d, want 400", code)
	}
	// search returns cards including note:n
	code, got := doJSON(t, h, "GET", "/api/search?q=body", "")
	if code != 200 {
		t.Fatalf("search = %d", code)
	}
	cards, _ := got["cards"].([]any)
	found := false
	for _, ci := range cards {
		if ci.(map[string]any)["NoteID"] == "n" {
			found = true
		}
	}
	if !found {
		t.Fatalf("search did not return note n: %+v", cards)
	}
	// fetch the note's markdown by id
	code, note := doJSON(t, h, "GET", "/api/note/n", "")
	if code != 200 || !strings.Contains(note["markdown"].(string), "# N") {
		t.Errorf("note fetch = %d %+v", code, note)
	}
	// unknown id -> 404
	if code, _ := doJSON(t, h, "GET", "/api/note/nope", ""); code != http.StatusNotFound {
		t.Errorf("unknown note = %d, want 404", code)
	}
}
