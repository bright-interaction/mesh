// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package updatecheck performs a small, cached check for a newer public Mesh
// release. It asks the Go module proxy because @latest is the artifact external
// go-install users actually receive; public repository main may be newer without
// a corresponding release tag.
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

const (
	DefaultEndpoint = "https://proxy.golang.org/github.com/bright-interaction/mesh/@latest"
	cacheMaxAge     = 24 * time.Hour
	staleMaxAge     = 7 * 24 * time.Hour
	maxReplyBytes   = 4096
	modulePath      = "github.com/bright-interaction/mesh/cmd/mesh"
	releaseBaseURL  = "https://github.com/bright-interaction/mesh/tree/"
)

// Notice is safe to render directly after using textContent (web) or ordinary
// terminal text (TUI). Latest is accepted only when it is a valid Go semver.
type Notice struct {
	Available bool   `json:"available"`
	Current   string `json:"current,omitempty"`
	Latest    string `json:"latest,omitempty"`
	Command   string `json:"command,omitempty"`
	URL       string `json:"url,omitempty"`
}

type cacheEntry struct {
	CheckedAt int64  `json:"checked_at"`
	Latest    string `json:"latest"`
}

// Checker serializes refreshes within one process and shares the result between
// web viewers. The on-disk cache prevents a TUI launch from phoning home more than
// once a day across processes.
type Checker struct {
	Client    *http.Client
	Endpoint  string
	CachePath string
	MaxAge    time.Duration
	Now       func() time.Time

	mu sync.Mutex
}

// New returns the production checker. Redirects are refused so the fixed public
// endpoint cannot silently turn an update check into a request to another host.
func New() *Checker {
	cachePath := ""
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		cachePath = filepath.Join(dir, "mesh", "update-check.json")
	}
	return &Checker{
		Client: &http.Client{
			Timeout: 3 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("update check redirect refused")
			},
		},
		Endpoint:  DefaultEndpoint,
		CachePath: cachePath,
		MaxAge:    cacheMaxAge,
		Now:       time.Now,
	}
}

// Default is concurrency-safe and shared by all surfaces in one process.
var Default = New()

// Check returns a newer release or an empty notice. Developer/SHA builds skip the
// network because they cannot be meaningfully ordered against a release version.
func (c *Checker) Check(ctx context.Context, current string) (Notice, error) {
	current = normalizeVersion(current)
	if updateCheckDisabled() || !semver.IsValid(current) {
		return Notice{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	cached, cacheErr := c.readCache()
	if cacheErr == nil && cacheFresh(cached, now, c.maxAge()) {
		return notice(current, cached.Latest), nil
	}

	latest, err := c.fetch(ctx, current)
	if err != nil {
		// A recent validated cache is still useful during a transient outage. Never
		// keep a release warning alive indefinitely after the source goes away.
		if cacheErr == nil && cacheFresh(cached, now, staleMaxAge) {
			return notice(current, cached.Latest), nil
		}
		return Notice{}, err
	}
	_ = c.writeCache(cacheEntry{CheckedAt: now.Unix(), Latest: latest})
	return notice(current, latest), nil
}

func (c *Checker) fetch(ctx context.Context, current string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mesh/"+current+" update-check")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update endpoint: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxReplyBytes {
		return "", errors.New("update endpoint response too large")
	}
	var reply struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(b, &reply); err != nil {
		return "", err
	}
	latest := normalizeVersion(reply.Version)
	if !semver.IsValid(latest) {
		return "", fmt.Errorf("update endpoint returned invalid version %q", reply.Version)
	}
	return latest, nil
}

func notice(current, latest string) Notice {
	if !semver.IsValid(current) || !semver.IsValid(latest) || semver.Compare(latest, current) <= 0 {
		return Notice{Current: current, Latest: latest}
	}
	return Notice{
		Available: true,
		Current:   current,
		Latest:    latest,
		Command:   "go install " + modulePath + "@" + latest,
		URL:       releaseBaseURL + latest,
	}
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v != "" && v[0] >= '0' && v[0] <= '9' {
		v = "v" + v
	}
	return v
}

func updateCheckDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MESH_NO_UPDATE_CHECK"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) maxAge() time.Duration {
	if c.MaxAge > 0 {
		return c.MaxAge
	}
	return cacheMaxAge
}

func cacheFresh(entry cacheEntry, now time.Time, maxAge time.Duration) bool {
	if !semver.IsValid(normalizeVersion(entry.Latest)) || entry.CheckedAt <= 0 {
		return false
	}
	checked := time.Unix(entry.CheckedAt, 0)
	age := now.Sub(checked)
	return age >= 0 && age <= maxAge
}

func (c *Checker) readCache() (cacheEntry, error) {
	var entry cacheEntry
	if c.CachePath == "" {
		return entry, os.ErrNotExist
	}
	f, err := os.Open(c.CachePath)
	if err != nil {
		return entry, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxReplyBytes+1))
	if err != nil {
		return entry, err
	}
	if len(b) > maxReplyBytes {
		return entry, errors.New("update cache too large")
	}
	if err := json.Unmarshal(b, &entry); err != nil {
		return entry, err
	}
	entry.Latest = normalizeVersion(entry.Latest)
	return entry, nil
}

func (c *Checker) writeCache(entry cacheEntry) error {
	if c.CachePath == "" {
		return nil
	}
	dir := filepath.Dir(c.CachePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".update-check-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := json.NewEncoder(tmp).Encode(entry); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, c.CachePath); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes the atomic rename durable. Directory handles are not supported
// on every platform, so the cache treats this as best effort after its data has
// already been fsynced.
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		slog.Debug("update check: cannot open cache directory to fsync it", "dir", dir, "err", err)
		return
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		slog.Debug("update check: directory fsync not supported here", "dir", dir, "err", err)
	}
}
