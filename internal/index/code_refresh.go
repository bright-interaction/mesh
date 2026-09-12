// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/index/code"
	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/vault"
)

// OpCodeRefresh requests a full refresh of the OWNER's configured source roots.
// Requests never carry filesystem paths or commands supplied by a reader.
const OpCodeRefresh = "code_refresh"

var errCodeRefreshConfigChanged = errors.New("code configuration changed since refresh was requested; request again")

// The scheduler normally runs every five minutes. During an operational failure,
// the owner may still receive many note events; cap source retries at one/minute
// per unchanged configuration. A new owner gets one immediate recovery attempt.
func (s *Store) deferCodeRefreshRetry(config string, err error) {
	s.codeRefreshRetryConfig = config
	s.codeRefreshRetryErr = err
	s.codeRefreshRetryAfter = time.Now().Add(time.Minute)
}

func codeConfigHash(cfg meshcfg.Code) string {
	b, _ := json.Marshal(cfg)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (s *Store) checkCodeRefreshConfig(expected string) error {
	cfg, err := meshcfg.LoadConfig(s.dir)
	if err != nil {
		return err
	}
	if codeConfigHash(cfg.Code) != expected {
		return errCodeRefreshConfigChanged
	}
	return nil
}

func codeRefreshID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (s *Store) codeRefreshReceipt(id string) (string, error) {
	if !codeRefreshID(id) {
		return "", fmt.Errorf("invalid code refresh request ID")
	}
	return filepath.Join(s.dir, "code-refresh-receipts", id), nil
}

// EnqueueCodeRefresh is legal on a read-only store. The bounded op queue provides
// durability; a positive receipt, not disappearance of the op, proves completion.
// Older owners discard unknown op kinds, so absence alone is NOT an acknowledgement.
func (s *Store) EnqueueCodeRefresh(expectedRoot ...string) (string, error) {
	cfg, err := meshcfg.LoadConfig(s.dir)
	if err != nil {
		return "", err
	}
	if len(expectedRoot) > 0 && expectedRoot[0] != "" {
		if len(cfg.Code.Roots) != 1 {
			return "", fmt.Errorf("--expect-root requires exactly one configured code root")
		}
		want, err := filepath.EvalSymlinks(expectedRoot[0])
		if err != nil {
			return "", err
		}
		got, err := filepath.EvalSymlinks(cfg.Code.Roots[0])
		if err != nil {
			return "", err
		}
		want, _ = filepath.Abs(want)
		got, _ = filepath.Abs(got)
		if got != want {
			return "", fmt.Errorf("configured code root does not match --expect-root; refusing to acknowledge another checkout")
		}
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(token[:])
	_, err = s.EnqueueOp(Op{Kind: OpCodeRefresh, ID: id, CodeConfig: codeConfigHash(cfg.Code)})
	return id, err
}

func (s *Store) AwaitCodeRefresh(ctx context.Context, id string, timeout time.Duration) error {
	path, err := s.codeRefreshReceipt(id)
	if err != nil {
		return err
	}
	err = s.awaitOwner(ctx, timeout, func(ctx context.Context) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		// One byte beyond the expected receipt detects trailing garbage while
		// bounding allocation even for an oversized/corrupt acknowledgement file.
		b, err := vault.ReadFileHeadContext(ctx, path, len(id)+2)
		if os.IsNotExist(err) {
			return false, nil
		}
		return string(b) == id+"\n", err
	})
	if err != nil {
		return fmt.Errorf("code refresh %s has no completion receipt (queued work may still finish; ensure the owner supports --through-owner): %w", id, err)
	}
	// A receipt belongs to this one caller, not SQLite. Abandoned receipts expire
	// during a future successful refresh; no caller removes another's receipt.
	_ = os.Remove(path)
	return nil
}

func (s *Store) applyCodeRefresh(ctx context.Context, id string) error {
	if _, err := s.codeRefreshReceipt(id); err != nil {
		return err
	}
	if err := s.runCodeRefresh(ctx); err != nil {
		return err
	}
	return s.ackCodeRefresh(ctx, id)
}

func (s *Store) runCodeRefresh(ctx context.Context, configHash ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := meshcfg.LoadConfig(s.dir)
	if err != nil {
		return err
	}
	if len(configHash) > 0 && configHash[0] != codeConfigHash(cfg.Code) {
		return errCodeRefreshConfigChanged
	}
	if !cfg.Code.Index || len(cfg.Code.Roots) == 0 {
		return fmt.Errorf("owner-routed code refresh requires [code] index=true and configured roots")
	}
	// Refuse unavailable roots before any publication.
	for _, root := range cfg.Code.Roots {
		info, err := os.Lstat(root)
		if err != nil {
			return fmt.Errorf("code root unavailable: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("code root must be a real directory, not a symlink: %s", root)
		}
	}
	var langs map[string]bool
	if len(cfg.Code.Languages) > 0 {
		langs = make(map[string]bool, len(cfg.Code.Languages))
	}
	for _, lang := range cfg.Code.Languages {
		langs[strings.ToLower(strings.TrimSpace(lang))] = true
	}
	// Git can replace a file twice in one second; the incremental code index uses
	// second-resolution mtimes. Explicit refreshes must not retain the old symbols.
	paths, err := code.WalkCode(cfg.Code.Roots, langs)
	if err != nil {
		return err
	}
	refs := make([]code.FileRef, 0, len(paths))
	for _, path := range paths {
		if rel, ok := relToRoots(cfg.Code.Roots, path); ok {
			refs = append(refs, code.FileRef{Abs: path, Rel: rel})
		}
	}
	files, failures := code.ParseCodeFiles(refs, 0)
	if len(failures) > 0 {
		return fmt.Errorf("code refresh refused partial parse (%d failures): %s: %w", len(failures), failures[0].Path, failures[0].Err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.IndexCodeFull(files); err != nil {
		return err
	}
	if err := s.setCodeRoots(cfg.Code.Roots); err != nil {
		return err
	}
	if _, err := s.LinkNotesToCodeContext(ctx, filepath.Dir(s.dir)); err != nil {
		return err
	}
	return nil
}

func (s *Store) ackCodeRefresh(ctx context.Context, id string) error {
	path, err := s.codeRefreshReceipt(id)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(id + "\n")
	// Private derived queue metadata, not a shared note. Keep this explicit so
	// atomic-writer mode audits cannot mistake it for an omitted permission policy.
	merr := tmp.Chmod(0o600)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := firstOpErr(werr, merr, serr, cerr); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	// Only derived acknowledgement files with our exact ID format are eligible.
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if !entry.IsDir() && codeRefreshID(entry.Name()) {
			if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	return nil
}
