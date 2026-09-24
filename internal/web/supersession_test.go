// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

const supersessionHistoryID = "historical-procedure"
const supersessionReplacementID = "current-procedure"
const supersessionSearchURL = "/api/search?q=archiveneedle&limit=10&budget=1200"

// The fixture exercises the real authenticated HTTP handlers without a listener,
// a model process, or an embedding endpoint. Keep replacement-specific strings
// out of the original note so a fenced identifier leak cannot hide in its prose.
func supersessionServer(t *testing.T, replacementScope string) (*Server, string) {
	t.Helper()
	t.Setenv("MESH_RERANK_AGENT", "http")
	for _, key := range []string{"MESH_RERANK_ENDPOINT", "MESH_RERANK_MODEL", "MESH_EMBED_ENDPOINT", "MESH_EMBED_MODEL"} {
		t.Setenv(key, "")
	}
	t.Setenv("MESH_WEIGHT_FTS", "1")
	t.Setenv("MESH_WEIGHT_GRAPH", "0")
	t.Setenv("MESH_WEIGHT_VEC", "0")
	t.Setenv("MESH_FRESHNESS_HALFLIFE_DAYS", "0")

	dir := t.TempDir()
	writeNote(t, dir, "public/history.md", "---\nid: "+supersessionHistoryID+"\ntitle: Historical procedure\ntype: note\nscope: public\n---\n# Historical procedure\narchiveneedle original historical prose\n")
	writeNote(t, dir, "corrections/replacement.md", supersessionReplacement(replacementScope))
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	rt, err := s.retrieverContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rt.RerankActive() || rt.VectorsActive() {
		t.Fatal("supersession HTTP fixture must not enable model providers")
	}
	return s, dir
}

func supersessionReplacement(scope string) string {
	return "---\nid: " + supersessionReplacementID + "\ntitle: Replacement classifiedtitle\ntype: note\nscope: " + scope + "\nsupersedes: [" + supersessionHistoryID + "]\n---\n# Replacement classifiedtitle\nreplacementbodymarker current guidance\n"
}

func supersessionMemberAuth(s *Server, scopes func() map[string]bool, paths func() func(string) bool) {
	s.SetMemberAuth(
		func(token string) (int64, string, bool) {
			switch token {
			case "test-admin":
				return 1, "admin", true
			case "test-member":
				return 2, "viewer", true
			default:
				return 0, "", false
			}
		},
		func(id int64) map[string]bool {
			if id == 2 {
				return scopes()
			}
			return nil
		},
		func(id int64) func(string) bool {
			if id == 2 {
				return paths()
			}
			return nil
		},
		func(id int64) (string, int64, bool) {
			if id == 1 {
				return "admin", 1000, true
			}
			if id == 2 {
				return "viewer", 2000, true
			}
			return "", 0, false
		},
	)
}

func supersessionGet(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func supersessionHistoryCard(t *testing.T, w *httptest.ResponseRecorder) retrieve.Card {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("search = %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Cards []retrieve.Card `json:"cards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, card := range result.Cards {
		if card.NoteID == supersessionHistoryID {
			return card
		}
	}
	t.Fatalf("readable historical card must remain in search: %s", w.Body.String())
	return retrieve.Card{}
}

func TestSearchSupersessionRespectsReplacementAccess(t *testing.T) {
	for _, tc := range []struct {
		name, replacementScope string
		fenceFolder            bool
		wantReplacement        string
	}{
		{"readable", "public", false, supersessionReplacementID},
		{"scope fenced", "secret", false, ""},
		{"folder fenced without scope filtering", "public", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := supersessionServer(t, tc.replacementScope)
			supersessionMemberAuth(s, func() map[string]bool {
				if tc.fenceFolder {
					return nil // folder ACLs must work independently of scopes
				}
				return map[string]bool{"public": true}
			}, func() func(string) bool {
				if tc.fenceFolder {
					return func(path string) bool { return !strings.HasPrefix(path, "corrections/") }
				}
				return nil
			})
			h := s.Handler()
			// Same fixture, unrestricted identity: prove the relation is present.
			admin := supersessionHistoryCard(t, supersessionGet(h, supersessionSearchURL, "test-admin"))
			if admin.SupersededBy != supersessionReplacementID {
				t.Fatalf("admin control lost replacement: %+v", admin)
			}
			response := supersessionGet(h, supersessionSearchURL, "test-member")
			member := supersessionHistoryCard(t, response)
			if member.SupersededBy != tc.wantReplacement {
				t.Fatalf("member replacement = %q, want %q", member.SupersededBy, tc.wantReplacement)
			}
			if tc.wantReplacement == "" {
				for _, marker := range []string{supersessionReplacementID, "classifiedtitle", "corrections/replacement.md", "replacementbodymarker"} {
					if strings.Contains(response.Body.String(), marker) {
						t.Errorf("fenced replacement leaked %q: %s", marker, response.Body.String())
					}
				}
			}
			history := supersessionGet(h, "/api/note/"+supersessionHistoryID, "test-member")
			if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), "original historical prose") {
				t.Fatalf("historical note should remain readable: %d %s", history.Code, history.Body.String())
			}
		})
	}
}

func TestSupersessionClickRechecksReplacementAccess(t *testing.T) {
	for _, change := range []string{"member scope revoked", "folder access revoked", "file scope tightened before indexing"} {
		t.Run(change, func(t *testing.T) {
			s, dir := supersessionServer(t, "public")
			revoked := false
			supersessionMemberAuth(s, func() map[string]bool {
				if change == "member scope revoked" && revoked {
					return map[string]bool{"history": true}
				}
				return map[string]bool{"history": true, "public": true}
			}, func() func(string) bool {
				if change == "folder access revoked" && revoked {
					return func(path string) bool { return !strings.HasPrefix(path, "corrections/") }
				}
				return nil
			})
			// Give the historical note an independent scope so revoking the
			// replacement's scope does not accidentally hide the control note.
			writeNote(t, dir, "public/history.md", "---\nid: "+supersessionHistoryID+"\ntitle: Historical procedure\ntype: note\nscope: history\n---\n# Historical procedure\narchiveneedle original historical prose\n")
			seedIndex(t, dir)
			h := s.Handler()
			card := supersessionHistoryCard(t, supersessionGet(h, supersessionSearchURL, "test-member"))
			if card.SupersededBy != supersessionReplacementID {
				t.Fatalf("pre-revocation search did not offer the replacement: %+v", card)
			}
			before := supersessionGet(h, "/api/note/"+card.SupersededBy, "test-member")
			if before.Code != http.StatusOK || !strings.Contains(before.Body.String(), "replacementbodymarker") {
				t.Fatalf("replacement must be readable before revocation: %d %s", before.Code, before.Body.String())
			}
			revoked = true
			if change == "file scope tightened before indexing" {
				writeNote(t, dir, "corrections/replacement.md", supersessionReplacement("secret"))
			}
			denied := supersessionGet(h, "/api/note/"+card.SupersededBy, "test-member")
			missing := supersessionGet(h, "/api/note/no-such-note", "test-member")
			if denied.Code != http.StatusNotFound || denied.Code != missing.Code || denied.Body.String() != missing.Body.String() {
				t.Fatalf("replacement must receive the same opaque 404 as unknown id: denied=%d %q missing=%d %q", denied.Code, denied.Body.String(), missing.Code, missing.Body.String())
			}
			history := supersessionGet(h, "/api/note/"+supersessionHistoryID, "test-member")
			if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), "original historical prose") {
				t.Fatalf("revocation must not remove readable history: %d %s", history.Code, history.Body.String())
			}
		})
	}
}

func TestSearchSupersessionDropsDeletedReplacement(t *testing.T) {
	s, dir := supersessionServer(t, "public")
	supersessionMemberAuth(s, func() map[string]bool { return map[string]bool{"public": true} }, func() func(string) bool { return nil })
	h := s.Handler()
	before := supersessionHistoryCard(t, supersessionGet(h, supersessionSearchURL, "test-member"))
	if before.SupersededBy != supersessionReplacementID {
		t.Fatalf("pre-deletion relation missing: %+v", before)
	}
	// Delete only this disposable test note, then let the writer publish the
	// deletion. The reader's already-cached graph must not resurrect its pointer.
	if err := os.Remove(filepath.Join(dir, "corrections/replacement.md")); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	after := supersessionHistoryCard(t, supersessionGet(h, supersessionSearchURL, "test-member"))
	if after.SupersededBy != "" {
		t.Fatalf("deleted replacement remains in historical card: %+v", after)
	}
	deleted := supersessionGet(h, "/api/note/"+supersessionReplacementID, "test-member")
	missing := supersessionGet(h, "/api/note/no-such-note", "test-member")
	if deleted.Code != http.StatusNotFound || deleted.Code != missing.Code || deleted.Body.String() != missing.Body.String() {
		t.Fatalf("deleted replacement must look absent: deleted=%d %q missing=%d %q", deleted.Code, deleted.Body.String(), missing.Code, missing.Body.String())
	}
	history := supersessionGet(h, "/api/note/"+supersessionHistoryID, "test-member")
	if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), "original historical prose") {
		t.Fatalf("deleting replacement must not remove history: %d %s", history.Code, history.Body.String())
	}
}
