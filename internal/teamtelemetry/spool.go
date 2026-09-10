// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package teamtelemetry is the private, content-free outbox that carries successful
// local fetches to a joined team hub. One file per event makes concurrent MCP writers
// and sync acknowledgements atomic without a shared append lock.
package teamtelemetry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

const MaxPerSync = 500

type credentialScope struct {
	VaultID string `json:"vault_id"`
}

func activeVaultID(vaultRoot string) string {
	b, err := os.ReadFile(filepath.Join(vaultRoot, ".mesh", "credentials"))
	if err != nil {
		return ""
	}
	var c credentialScope
	if json.Unmarshal(b, &c) != nil {
		return ""
	}
	return c.VaultID
}

func scopeDir(vaultRoot, vaultID string) string {
	sum := sha256.Sum256([]byte(vaultID))
	return filepath.Join(vaultRoot, ".mesh", "team-reuse-outbox", hex.EncodeToString(sum[:]))
}

func validEventID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func newEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// RecordForJoinedVault queues only note id + event id + time. Queries, snippets,
// bodies, usernames, emails and credentials never enter the spool.
func RecordForJoinedVault(vaultRoot, noteID string, fetchedAt time.Time) error {
	vaultID := activeVaultID(vaultRoot)
	if vaultID == "" || noteID == "" {
		return nil // solo/unjoined vault: the local flywheel remains the only metric
	}
	id, err := newEventID()
	if err != nil {
		return err
	}
	dir := scopeDir(vaultRoot, vaultID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	e := syncproto.ReuseEvent{EventID: id, NoteID: noteID, FetchedAt: fetchedAt.Unix()}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	p := filepath.Join(dir, id+".json")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		_ = os.Remove(p)
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func Pending(vaultRoot, vaultID string, limit int) ([]syncproto.ReuseEvent, error) {
	if vaultID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > MaxPerSync {
		limit = MaxPerSync
	}
	dir := scopeDir(vaultRoot, vaultID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := make([]syncproto.ReuseEvent, 0, min(limit, len(entries)))
	for _, ent := range entries {
		if len(out) >= limit || ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		id := ent.Name()[:len(ent.Name())-len(".json")]
		if !validEventID(id) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			return nil, err
		}
		var e syncproto.ReuseEvent
		if json.Unmarshal(b, &e) != nil || e.EventID != id || e.NoteID == "" || e.FetchedAt <= 0 {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Ack deletes only ids explicitly acknowledged by the hub, inside the active vault-id
// scope. A rejoin therefore cannot upload or acknowledge another team's pending ids.
func Ack(vaultRoot, vaultID string, ids []string) error {
	dir := scopeDir(vaultRoot, vaultID)
	for _, id := range ids {
		if !validEventID(id) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
