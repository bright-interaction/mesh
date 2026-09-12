// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/meshcfg"
)

func codeRefreshFixture(t *testing.T) (*Store, *Store, string) {
	t.Helper()
	root := opsVault(t)
	owner, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	reader, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	codeRoot := t.TempDir()
	config := meshcfg.Config{Code: meshcfg.Code{Index: true, Roots: []string{codeRoot}, Languages: []string{"go"}}}
	if err := meshcfg.SaveConfig(owner.MeshDir(), config); err != nil {
		t.Fatal(err)
	}
	return owner, reader, codeRoot
}

func codeRefreshRequest(t *testing.T, s *Store) string {
	t.Helper()
	id, err := s.EnqueueCodeRefresh()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("receipt id %q is not 32 lowercase hex characters", id)
	}
	return id
}

func TestCodeRefreshReaderOwnerHandoffRefreshesSameSecondAndLinks(t *testing.T) {
	owner, reader, codeRoot := codeRefreshFixture(t)
	// Caller environment must not expand the owner's configured indexing scope.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.go"), []byte("package outside\nfunc UnconfiguredSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_CODE_ROOTS", outside)
	path := filepath.Join(codeRoot, "sample.go")
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("package sample\nfunc "+name+"() {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	assertSymbol := func(name string, want int) {
		t.Helper()
		hits, err := reader.SearchCode(context.Background(), name, 10, nil)
		if err != nil || len(hits) != want {
			t.Fatalf("reader search %q: %+v, %v; want %d hits", name, hits, err, want)
		}
	}
	// Seed a mention before its symbol exists. Only the later refresh can link it.
	notePath := filepath.Join(filepath.Dir(owner.MeshDir()), "n.md")
	if err := os.WriteFile(notePath, []byte("---\nid: n\ntype: note\n---\n# N\n`AfterrSymbol`\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReindexFull(owner, filepath.Dir(owner.MeshDir())); err != nil {
		t.Fatal(err)
	}
	for _, symbol := range []string{"BeforeSymbol", "AfterrSymbol"} {
		write(symbol) // Identical mtime and file size: incremental mtime checks miss this.
		id := codeRefreshRequest(t, reader)
		if err := reader.AwaitCodeRefresh(context.Background(), id, 5*time.Millisecond); err == nil {
			t.Fatal("queued but unapplied refresh reported success")
		}
		if n, err := owner.DrainOps(); err != nil || n != 1 {
			t.Fatalf("owner drain = %d, %v; want one applied refresh", n, err)
		}
		st, err := os.Stat(filepath.Join(owner.MeshDir(), "code-refresh-receipts", id))
		if err != nil || !st.Mode().IsRegular() {
			t.Fatalf("successful refresh lacks a regular receipt: %v, %v", st, err)
		}
		if err := reader.AwaitCodeRefresh(context.Background(), id, time.Second); err != nil {
			t.Fatal(err)
		}
		assertSymbol(symbol, 1)
		assertSymbol("UnconfiguredSymbol", 0)
	}
	assertSymbol("BeforeSymbol", 0)
	// Receipt acknowledgement includes the note/code bridge, not only symbols.
	links := bridgeRows(t, reader)
	if len(links) != 1 || links[0].name != "AfterrSymbol" {
		t.Fatalf("acknowledged refresh did not publish note links: %+v", links)
	}
}

func TestCodeRefreshMissingRootNeverAcknowledges(t *testing.T) {
	owner, reader, codeRoot := codeRefreshFixture(t)
	if err := os.Remove(codeRoot); err != nil {
		t.Fatal(err)
	}
	id := codeRefreshRequest(t, reader)
	if err := owner.applyCodeRefresh(context.Background(), id); err == nil {
		t.Fatal("missing configured root reported success")
	}
	if _, err := os.Stat(filepath.Join(owner.MeshDir(), "code-refresh-receipts", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed refresh created a success receipt: %v", err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), id, 5*time.Millisecond); err == nil {
		t.Fatal("failed refresh was acknowledged")
	}
	files, err := os.ReadDir(OpsDir(owner.MeshDir()))
	if err != nil || len(files) != 1 {
		t.Fatalf("failed request must remain retryable: %v, %v", files, err)
	}
}

func TestCodeRefreshSymlinkRootPreservesSymbolsWithoutAcknowledgement(t *testing.T) {
	owner, reader, codeRoot := codeRefreshFixture(t)
	if err := os.WriteFile(filepath.Join(codeRoot, "sample.go"), []byte("package sample\nfunc RetainedAcrossSymlinkRoot() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedID := codeRefreshRequest(t, reader)
	if n, err := owner.DrainOps(); err != nil || n != 1 {
		t.Fatalf("seed refresh: %d, %v", n, err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), seedID, time.Second); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(t.TempDir(), "source-link")
	if err := os.Symlink(codeRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	config := meshcfg.Config{Code: meshcfg.Code{Index: true, Roots: []string{linkRoot}, Languages: []string{"go"}}}
	if err := meshcfg.SaveConfig(owner.MeshDir(), config); err != nil {
		t.Fatal(err)
	}
	// Expected-root identity alone accepts this alias. The owner must reject it
	// before a non-following filesystem walk can publish an empty source index.
	id, err := reader.EnqueueCodeRefresh(codeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := owner.DrainOps(); n != 0 || err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink root refresh: %d, %v; want rejection before publication", n, err)
	}
	if _, err := os.Stat(filepath.Join(owner.MeshDir(), "code-refresh-receipts", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink root received a success receipt: %v", err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), id, 5*time.Millisecond); err == nil {
		t.Fatal("symlink root refresh was acknowledged")
	}
	hits, err := reader.SearchCode(context.Background(), "RetainedAcrossSymlinkRoot", 5, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("rejected symlink root wiped existing symbols: %+v, %v", hits, err)
	}
	// A failed optional refresh remains deferred without suppressing newly
	// queued unrelated work during the cooldown.
	pending := PendingNote{Type: "gotcha", Title: "Source cooldown allows bookkeeping"}
	if _, err := reader.EnqueueOp(Op{Kind: OpAddPending, Pending: &pending}); err != nil {
		t.Fatal(err)
	}
	if n, err := owner.DrainOps(); n != 1 || err == nil {
		t.Fatalf("cooldown drain: %d, %v; want unrelated op plus deferred failure", n, err)
	}
	if _, err := owner.GetPending(PendingID(pending.Type, pending.Title)); err != nil {
		t.Fatalf("source cooldown blocked unrelated bookkeeping: %v", err)
	}
}

func TestCodeRefreshDrainCoalescesRequests(t *testing.T) {
	owner, reader, codeRoot := codeRefreshFixture(t)
	if err := os.WriteFile(filepath.Join(codeRoot, "sample.go"), []byte("package sample\nfunc CoalescedSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Count actual index publications, not queue removal or receipt issuance.
	if err := owner.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE refresh_test_publications (n INTEGER NOT NULL)`); err != nil {
			return err
		}
		_, err := tx.Exec(`CREATE TRIGGER refresh_test_publication AFTER INSERT ON code_symbols BEGIN INSERT INTO refresh_test_publications(n) VALUES(1); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ids := []string{codeRefreshRequest(t, reader), codeRefreshRequest(t, reader), codeRefreshRequest(t, reader)}
	if n, err := owner.DrainOps(); err != nil || n != len(ids) {
		t.Fatalf("coalesced drain: %d, %v; want %d acknowledgements", n, err, len(ids))
	}
	for _, id := range ids {
		if err := reader.AwaitCodeRefresh(context.Background(), id, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	var publications int
	if err := owner.readDB.QueryRow(`SELECT count(*) FROM refresh_test_publications`).Scan(&publications); err != nil {
		t.Fatal(err)
	}
	if publications != 1 {
		t.Fatalf("%d queued requests caused %d full code publications, want one", len(ids), publications)
	}
}

func TestCodeRefreshRemovedQueueIsNotAcknowledgement(t *testing.T) {
	owner, reader, _ := codeRefreshFixture(t)
	id := codeRefreshRequest(t, reader)
	files, err := os.ReadDir(OpsDir(owner.MeshDir()))
	if err != nil || len(files) != 1 {
		t.Fatalf("queued files: %v, %v", files, err)
	}
	path := filepath.Join(OpsDir(owner.MeshDir()), files[0].Name())
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var op Op
	if err := json.Unmarshal(body, &op); err != nil || op.Kind != "code_refresh" || op.ID != id {
		t.Fatalf("queued refresh payload: %+v, %v", op, err)
	}
	// An old owner drops unknown operations. Removal alone cannot be a receipt.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), id, 5*time.Millisecond); err == nil {
		t.Fatal("old owner's discarded unknown op was mistaken for successful refresh")
	}
}

func TestCodeRefreshRejectsInvalidReceiptIDs(t *testing.T) {
	owner, reader, _ := codeRefreshFixture(t)
	for _, id := range []string{"", "../outside", strings.Repeat("A", 32), strings.Repeat("a", 31), "/tmp/receipt", strings.Repeat("g", 32)} {
		t.Run(id, func(t *testing.T) {
			if err := owner.applyCodeRefresh(context.Background(), id); err == nil {
				t.Fatalf("apply accepted invalid receipt ID %q", id)
			}
			if err := reader.AwaitCodeRefresh(context.Background(), id, time.Millisecond); err == nil {
				t.Fatalf("wait accepted invalid receipt ID %q", id)
			}
		})
	}
}

func TestCodeRefreshWaitHonorsCancellation(t *testing.T) {
	_, reader, _ := codeRefreshFixture(t)
	id := codeRefreshRequest(t, reader)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reader.AwaitCodeRefresh(ctx, id, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v, want context.Canceled", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := reader.AwaitCodeRefresh(ctx, id, time.Minute); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline wait = %v, want context.DeadlineExceeded", err)
	}
}

func TestCodeRefreshMalformedReceiptDoesNotAcknowledge(t *testing.T) {
	_, reader, _ := codeRefreshFixture(t)
	id := codeRefreshRequest(t, reader)
	dir := filepath.Join(reader.MeshDir(), "code-refresh-receipts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "done\n", strings.Repeat("0", 32) + "\n", id} {
		if err := os.WriteFile(filepath.Join(dir, id), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := reader.AwaitCodeRefresh(context.Background(), id, 5*time.Millisecond); err == nil {
			t.Fatalf("malformed receipt %q was acknowledged", body)
		}
	}
}

func TestCodeRefreshParseFailurePreservesOldSymbolsAndOtherOpsProgress(t *testing.T) {
	owner, reader, codeRoot := codeRefreshFixture(t)
	path := filepath.Join(codeRoot, "sample.go")
	if err := os.WriteFile(path, []byte("package sample\nfunc PreviouslyIndexed() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := codeRefreshRequest(t, reader)
	if n, err := owner.DrainOps(); err != nil || n != 1 {
		t.Fatalf("seed refresh: %d, %v", n, err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), id, time.Second); err != nil {
		t.Fatal(err)
	}
	// Syntax tolerance is the existing source parser contract: this is not a
	// compiler. Use an actual read failure, which must not publish a partial index.
	broken := filepath.Join(codeRoot, "broken.go")
	if err := os.Symlink(filepath.Join(codeRoot, "missing.go"), broken); err != nil {
		t.Fatal(err)
	}
	failedID := codeRefreshRequest(t, reader)
	pending := PendingNote{Type: "gotcha", Title: "Unrelated durable operation"}
	if _, err := reader.EnqueueOp(Op{Kind: OpAddPending, Pending: &pending}); err != nil {
		t.Fatal(err)
	}
	if n, err := owner.DrainOps(); err == nil || n != 1 {
		t.Fatalf("partial drain must report failure but apply unrelated op: %d, %v", n, err)
	}
	if _, err := owner.GetPending(PendingID(pending.Type, pending.Title)); err != nil {
		t.Fatalf("failed refresh blocked unrelated queued operation: %v", err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), failedID, 5*time.Millisecond); err == nil {
		t.Fatal("partial parse received success acknowledgement")
	}
	hits, err := reader.SearchCode(context.Background(), "PreviouslyIndexed", 5, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("failed refresh replaced the previously valid index: %+v, %v", hits, err)
	}
	// The same queued request succeeds after the source is repaired.
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package sample\nfunc RepairedSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := owner.DrainOps(); err == nil || n != 0 {
		t.Fatalf("cooldown must defer expensive retry: %d, %v", n, err)
	}
	owner.codeRefreshRetryAfter = time.Time{} // advance past the retry cooldown
	if n, err := owner.DrainOps(); err != nil || n != 1 {
		t.Fatalf("repaired refresh: %d, %v", n, err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), failedID, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestCodeRefreshConfigDriftNeverAcknowledgesAnotherCheckout(t *testing.T) {
	owner, reader, originalRoot := codeRefreshFixture(t)
	if err := os.WriteFile(filepath.Join(originalRoot, "original.go"), []byte("package original\nfunc OriginalCheckoutSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	obsoleteID, err := reader.EnqueueCodeRefresh(originalRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Reconfigure after the caller's expected-root validation but before drain.
	newRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(newRoot, "current.go"), []byte("package current\nfunc ReconfiguredCheckoutSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := meshcfg.Config{Code: meshcfg.Code{Index: true, Roots: []string{newRoot}, Languages: []string{"go"}}}
	if err := meshcfg.SaveConfig(owner.MeshDir(), config); err != nil {
		t.Fatal(err)
	}
	currentID, err := reader.EnqueueCodeRefresh(newRoot)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := owner.DrainOps(); n != 1 || !errors.Is(err, errCodeRefreshConfigChanged) {
		t.Fatalf("config drift drain: %d, %v; want one valid refresh plus obsolete-config error", n, err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), obsoleteID, 5*time.Millisecond); err == nil {
		t.Fatal("original checkout request acknowledged a different configured checkout")
	}
	if err := reader.AwaitCodeRefresh(context.Background(), currentID, time.Second); err != nil {
		t.Fatalf("obsolete request blocked valid later refresh: %v", err)
	}
	hits, err := reader.SearchCode(context.Background(), "ReconfiguredCheckoutSymbol", 5, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("current config receipt did not publish the current checkout: %+v, %v", hits, err)
	}
	files, err := os.ReadDir(OpsDir(owner.MeshDir()))
	if err != nil || len(files) != 0 {
		t.Fatalf("obsolete request was not discarded: %v, %v", files, err)
	}
}

func TestCodeRefreshNewConfigurationBypassesFailedConfigurationCooldown(t *testing.T) {
	owner, reader, originalRoot := codeRefreshFixture(t)
	obsoleteID := codeRefreshRequest(t, reader)
	if err := os.Remove(originalRoot); err != nil {
		t.Fatal(err)
	}
	if n, err := owner.DrainOps(); n != 0 || err == nil {
		t.Fatalf("initial failed refresh: %d, %v", n, err)
	}
	if !time.Now().Before(owner.codeRefreshRetryAfter) {
		t.Fatal("failed refresh did not establish a future retry cooldown")
	}
	newRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(newRoot, "sample.go"), []byte("package sample\nfunc NewConfigurationBypassesCooldown() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := meshcfg.Config{Code: meshcfg.Code{Index: true, Roots: []string{newRoot}, Languages: []string{"go"}}}
	if err := meshcfg.SaveConfig(owner.MeshDir(), config); err != nil {
		t.Fatal(err)
	}
	currentID := codeRefreshRequest(t, reader)
	// Do not expire the old cooldown: an obsolete queued request cannot make a
	// fresh valid configuration wait behind a failure that no longer applies.
	if n, err := owner.DrainOps(); n != 1 || !errors.Is(err, errCodeRefreshConfigChanged) {
		t.Fatalf("changed config during cooldown: %d, %v; want valid refresh plus obsolete-config error", n, err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), currentID, time.Second); err != nil {
		t.Fatalf("new configuration waited behind obsolete cooldown: %v", err)
	}
	if err := reader.AwaitCodeRefresh(context.Background(), obsoleteID, 5*time.Millisecond); err == nil {
		t.Fatal("obsolete failed configuration received a success acknowledgement")
	}
	hits, err := reader.SearchCode(context.Background(), "NewConfigurationBypassesCooldown", 5, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("new configuration was not published: %+v, %v", hits, err)
	}
	files, err := os.ReadDir(OpsDir(owner.MeshDir()))
	if err != nil || len(files) != 0 {
		t.Fatalf("obsolete and acknowledged requests must leave the queue: %v, %v", files, err)
	}
}

func TestCodeRefreshReceiptIOFailureDoesNotStarveOtherOps(t *testing.T) {
	owner, reader, codeRoot := codeRefreshFixture(t)
	if err := os.WriteFile(filepath.Join(codeRoot, "sample.go"), []byte("package sample\nfunc ReceiptFailureSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	receiptDir := filepath.Join(owner.MeshDir(), "code-refresh-receipts")
	if err := os.WriteFile(receiptDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids := []string{codeRefreshRequest(t, reader), codeRefreshRequest(t, reader)}
	pending := PendingNote{Type: "gotcha", Title: "Receipt failure must not starve this"}
	if _, err := reader.EnqueueOp(Op{Kind: OpAddPending, Pending: &pending}); err != nil {
		t.Fatal(err)
	}
	if n, err := owner.DrainOps(); n != 1 || err == nil {
		t.Fatalf("receipt failure drain: %d, %v; want one unrelated op plus receipt error", n, err)
	}
	if _, err := owner.GetPending(PendingID(pending.Type, pending.Title)); err != nil {
		t.Fatalf("receipt failure starved unrelated operation: %v", err)
	}
	for _, id := range ids {
		if err := reader.AwaitCodeRefresh(context.Background(), id, 5*time.Millisecond); err == nil {
			t.Fatal("receipt I/O failure was acknowledged")
		}
	}
	files, err := os.ReadDir(OpsDir(owner.MeshDir()))
	if err != nil || len(files) != len(ids) {
		t.Fatalf("unacknowledged refreshes must remain retryable: %v, %v", files, err)
	}
	if err := os.Remove(receiptDir); err != nil {
		t.Fatal(err)
	}
	if n, err := owner.DrainOps(); n != 0 || err == nil {
		t.Fatalf("receipt cooldown must defer reparse: %d, %v", n, err)
	}
	owner.codeRefreshRetryAfter = time.Time{} // advance past the retry cooldown
	if n, err := owner.DrainOps(); n != len(ids) || err != nil {
		t.Fatalf("repaired receipt drain: %d, %v; want %d acknowledgements", n, err, len(ids))
	}
	for _, id := range ids {
		if err := reader.AwaitCodeRefresh(context.Background(), id, time.Second); err != nil {
			t.Fatal(err)
		}
	}
}
