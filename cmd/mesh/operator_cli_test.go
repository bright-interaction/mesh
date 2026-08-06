// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/spf13/cobra"
)

// runCLI executes one command and captures BOTH streams. The operator judges these
// commands by what they print and by the exit code, so a test that reads only one of the
// two would miss exactly the defects here (a lie on stdout with exit 0, a warning that was
// never written to stderr).
func runCLI(t *testing.T, c *cobra.Command, args ...string) (string, error) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	c.SetArgs(args)
	c.SilenceErrors = true
	c.SilenceUsage = true
	runErr := c.Execute()
	_ = w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	b, _ := io.ReadAll(r)
	_ = r.Close()
	return string(b), runErr
}

func writeNote(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodNote = "---\nid: good-note\ntype: note\ntitle: A good note\nwhen: \"2026-08-06\"\n---\n# A good note\n\nBody text.\n"

// unparseableNote has valid fences and broken YAML inside them, which is what ParseFile
// rejects. The note is then invisible to search and to the graph.
const unparseableNote = "---\nid: bad\ntype: [unclosed\n  : : :\n---\n# bad\n"

// TestDoctorReportsUnparseableNotes is Finding B. Observed before the fix, on a vault
// whose only note would not parse:
//
//	notes 0  nodes 0  edges 0 / lint: 0 problems / status: healthy   (exit 0)
//
// while `mesh lint` on the same vault exited 1 with "1 errors". Doctor discarded the
// []FileError from index.ParseFiles, so the notes that are invisible to search
// contributed nothing to its verdict. Doctor is the command named for diagnosis; it is
// the one command that must not lie.
func TestDoctorReportsUnparseableNotes(t *testing.T) {
	tests := []struct {
		name       string
		notes      map[string]string
		wantErr    bool
		wantOut    string
		notWantOut string
	}{
		{
			name:       "a vault of only unparseable notes is BROKEN, not healthy",
			notes:      map[string]string{"bad.md": unparseableNote},
			wantErr:    true,
			wantOut:    "status: BROKEN",
			notWantOut: "status: healthy",
		},
		{
			name:       "one bad note among good ones still breaks the verdict",
			notes:      map[string]string{"good.md": goodNote, "bad.md": unparseableNote},
			wantErr:    true,
			wantOut:    "status: BROKEN",
			notWantOut: "status: healthy",
		},
		{
			name:       "a clean vault is still reported as fine",
			notes:      map[string]string{"good.md": goodNote},
			wantErr:    false,
			wantOut:    "status:",
			notWantOut: "BROKEN",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.notes {
				writeNote(t, dir, name, body)
			}
			if _, err := runCLI(t, indexCmd(), dir); err != nil {
				t.Fatalf("mesh index: %v", err)
			}

			out, err := runCLI(t, doctorCmd(), dir)
			if tc.wantErr && err == nil {
				t.Errorf("doctor exited 0 on a vault with notes invisible to search\n%s", out)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("doctor failed on a clean vault: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("doctor output missing %q:\n%s", tc.wantOut, out)
			}
			if strings.Contains(out, tc.notWantOut) {
				t.Errorf("doctor output must not contain %q:\n%s", tc.notWantOut, out)
			}
		})
	}
}

// TestBulkRewritersAreDryRunByDefault: both rewrite every note in the vault in place with
// no backup, so `mesh migrate my-vault` typed once at the wrong directory used to rewrite
// the whole thing. `mesh structure --fill-bodies` already chose the safe shape (dry run by
// default, --apply to write); these two now match it, with --dry-run kept as an accepted
// no-op so existing scripts still parse.
func TestBulkRewritersAreDryRunByDefault(t *testing.T) {
	legacy := "---\ntype: note\ntitle: Legacy\n---\n# Legacy\n\nBody.\n"
	cmds := []struct {
		name string
		make func() *cobra.Command
	}{
		{"migrate", migrateCmd},
		{"scope backfill", scopeBackfillCmd},
	}
	for _, cd := range cmds {
		t.Run(cd.name, func(t *testing.T) {
			// The last row is the compat shim's sharp edge: --dry-run is hidden, so
			// whoever still passes it cannot read its help, and a flag that says "do not
			// write" must not be the reason something wrote. It beats --apply.
			for _, args := range [][]string{nil, {"--dry-run"}, {"--apply", "--dry-run"}} {
				dir := t.TempDir()
				p := writeNote(t, dir, "legacy.md", legacy)
				if _, err := runCLI(t, cd.make(), append([]string{dir}, args...)...); err != nil {
					t.Fatalf("%s %v: %v", cd.name, args, err)
				}
				got, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != legacy {
					t.Errorf("%s %v wrote a note it was asked not to:\n%s", cd.name, args, got)
				}
			}

			dir := t.TempDir()
			p := writeNote(t, dir, "legacy.md", legacy)
			if _, err := runCLI(t, cd.make(), dir, "--apply"); err != nil {
				t.Fatalf("%s --apply: %v", cd.name, err)
			}
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) == legacy {
				t.Errorf("%s --apply wrote nothing, so the flag inversion broke the command", cd.name)
			}
		})
	}
}

// TestBulkRewritersFailWhenFilesError: the summary counted `errored` and returned nil, so
// a migrate that failed on 800 of 1150 files printed the failures and still exited 0.
// Every script wrapping it read that as success.
func TestBulkRewritersFailWhenFilesError(t *testing.T) {
	// An unterminated frontmatter block is refused by both writers (see MigrateFile), so
	// it is the real failure the operator hits, not a synthetic one.
	unterminated := "---\ntype: note\ntitle: Never closed\n\n# Body starts here\n"
	cmds := []struct {
		name string
		make func() *cobra.Command
	}{
		{"migrate", migrateCmd},
		{"scope backfill", scopeBackfillCmd},
	}
	for _, cd := range cmds {
		t.Run(cd.name, func(t *testing.T) {
			dir := t.TempDir()
			writeNote(t, dir, "good.md", goodNote)
			writeNote(t, dir, "broken.md", unterminated)

			out, err := runCLI(t, cd.make(), dir, "--apply")
			if err == nil {
				t.Fatalf("%s exited 0 despite a file it could not rewrite:\n%s", cd.name, out)
			}
			if !strings.Contains(out, "1 errored") {
				t.Errorf("%s did not report the failure count:\n%s", cd.name, out)
			}
		})
	}
}

type stubLinker struct {
	links int
	err   error
}

func (s stubLinker) LinkNotesToCode(string) (int, error) { return s.links, s.err }

// TestRefreshNoteCodeLinksWarnsOnFailure: `links, _ := store.LinkNotesToCode(root)` dropped
// the error and the zero went straight into the success line, so a dead note<->code bridge
// printed "0 note links" and was indistinguishable from a vault whose notes mention no code.
func TestRefreshNoteCodeLinksWarnsOnFailure(t *testing.T) {
	tests := []struct {
		name      string
		linker    stubLinker
		wantCount int
		wantWarn  bool
	}{
		{"a failed bridge is announced", stubLinker{links: 0, err: errors.New("no such table: note_code_links")}, 0, true},
		{"a real zero stays silent", stubLinker{links: 0}, 0, false},
		{"a working bridge keeps its count", stubLinker{links: 42}, 42, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			got := refreshNoteCodeLinks(&sb, tc.linker, "/vault")
			if got != tc.wantCount {
				t.Errorf("count = %d, want %d", got, tc.wantCount)
			}
			warned := strings.Contains(sb.String(), "warning:")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v (output %q)", warned, tc.wantWarn, sb.String())
			}
		})
	}
}

// TestIndexRebuildsCorruptDatabase is the operator half of Finding C. With junk in
// .mesh/mesh.db, search, doctor, health AND index all failed with the same
// "apply schema: file is not a database (26)", so the command that rebuilds the index was
// itself blocked by the index. `mesh index` now discards and rebuilds it.
func TestIndexRebuildsCorruptDatabase(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "good.md", goodNote)
	if _, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("first index: %v", err)
	}
	dbPath := filepath.Join(dir, ".mesh", "mesh.db")
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(p)
	}
	if err := os.WriteFile(dbPath, []byte(strings.Repeat("junk bytes, not sqlite. ", 200)), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, indexCmd(), dir)
	if err != nil {
		t.Fatalf("mesh index could not recover from a corrupt index: %v\n%s", err, out)
	}
	if !strings.Contains(out, "was corrupt and unreadable") {
		t.Errorf("index deleted the database without telling the operator:\n%s", out)
	}

	store, openErr := index.Open(dir)
	if openErr != nil {
		t.Fatalf("index is still unreadable after the rebuild: %v", openErr)
	}
	defer store.Close()
	n, err := store.Count("notes")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rebuilt index holds %d notes, want 1", n)
	}
}
