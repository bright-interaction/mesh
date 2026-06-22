package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWebMemberScoping proves the per-member web mode scopes the graph + note + search
// surfaces to the signed-in member: a sales member never sees a dev note, a dev member
// (unrestricted) sees both, and an unauthenticated request is refused.
func TestWebMemberScoping(t *testing.T) {
	dir := t.TempDir()
	must := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("devsecret.md", "---\nid: devsecret\ntype: note\ntitle: DevSecret\nscope: dev\n---\n# dev only\n")
	must("salesplan.md", "---\nid: salesplan\ntype: note\ntitle: SalesPlan\nscope: sales\n---\n# sales only\n")

	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// Stub the hub-backed funcs: devtok = unrestricted (nil), salestok = {sales}.
	srv.SetMemberAuth(
		func(tok string) (int64, string, bool) {
			switch tok {
			case "devtok":
				return 1, "dev", true
			case "salestok":
				return 2, "sales", true
			}
			return 0, "", false
		},
		func(id int64) map[string]bool {
			if id == 2 {
				return map[string]bool{"sales": true}
			}
			return nil // dev: unrestricted
		},
	)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path, tok string) (int, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Unauthenticated -> the graph is refused (only the shell/assets/login are open).
	if st, _ := get("/graph.json", ""); st != http.StatusUnauthorized {
		t.Errorf("unauth graph = %d, want 401", st)
	}
	// Sales member -> graph omits the dev note, includes the sales note.
	st, body := get("/graph.json", "salestok")
	if st != 200 {
		t.Fatalf("sales graph = %d", st)
	}
	if strings.Contains(body, "DevSecret") {
		t.Errorf("sales graph must omit the dev note; got: %s", body)
	}
	if !strings.Contains(body, "SalesPlan") {
		t.Errorf("sales graph should include the sales note; got: %s", body)
	}
	// Dev member (unrestricted) -> sees both.
	if _, db := get("/graph.json", "devtok"); !strings.Contains(db, "DevSecret") || !strings.Contains(db, "SalesPlan") {
		t.Errorf("dev graph should include both notes; got: %s", db)
	}
	// Note fetch: sales blocked from dev (opaque 404), allowed for sales; dev allowed.
	if st, _ := get("/api/note/devsecret", "salestok"); st != http.StatusNotFound {
		t.Errorf("sales fetch of dev note = %d, want 404", st)
	}
	if st, _ := get("/api/note/salesplan", "salestok"); st != 200 {
		t.Errorf("sales fetch of sales note = %d, want 200", st)
	}
	if st, _ := get("/api/note/devsecret", "devtok"); st != 200 {
		t.Errorf("dev fetch of dev note = %d, want 200", st)
	}
	// Search: sales results never include the dev note.
	if _, sb := get("/api/search?q=only", "salestok"); strings.Contains(sb, "DevSecret") {
		t.Errorf("sales search must omit the dev note; got: %s", sb)
	}
}
