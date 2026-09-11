// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	maxUpgradeMetadata = 64 << 10
	maxUpgradeBinary   = 256 << 20
)

// UpgradeOptions describes an explicit prebuilt-client upgrade. HubURL is optional
// for a joined vault; Executable is a test seam and normally comes from os.Executable.
type UpgradeOptions struct {
	VaultDir       string
	HubURL         string
	Executable     string
	CurrentRelease string
	CheckOnly      bool
	HTTPClient     *http.Client
}

// UpgradeResult is safe to show to the operator. It contains no credential or note data.
type UpgradeResult struct {
	Current   string `json:"current,omitempty"`
	Latest    string `json:"latest"`
	HubURL    string `json:"hub_url"`
	BinaryURL string `json:"binary_url,omitempty"`
	Path      string `json:"path,omitempty"`
	UpToDate  bool   `json:"up_to_date"`
	Changed   bool   `json:"changed"`
}

type upgradeAbout struct {
	ReleaseVersion string `json:"release_version"`
	SourceURL      string `json:"source_url"`
}

type binaryVersion struct {
	Name    string `json:"name"`
	Release string `json:"release"`
}

// UpgradePrebuilt checks a joined hub and, when requested, atomically replaces this
// platform's client with the hub-served binary after TLS, SHA-256 and embedded-release
// verification. It never runs for an update banner; the operator invokes mesh upgrade.
func UpgradePrebuilt(ctx context.Context, opts UpgradeOptions) (UpgradeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	hubURL, token, err := upgradeHub(opts)
	if err != nil {
		return UpgradeResult{}, err
	}
	base, err := validateUpgradeHubURL(hubURL)
	if err != nil {
		return UpgradeResult{}, err
	}
	client := opts.HTTPClient
	if client == nil {
		client = upgradeHTTPClient()
	}

	meta, err := upgradeGET(ctx, client, base+"/about", "", maxUpgradeMetadata)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("upgrade: read hub version: %w", err)
	}
	var about upgradeAbout
	if err := json.Unmarshal(meta, &about); err != nil {
		return UpgradeResult{}, fmt.Errorf("upgrade: decode hub version: %w", err)
	}
	latest := normalizeRelease(about.ReleaseVersion)
	if !semver.IsValid(latest) {
		latest = releaseFromSourceURL(about.SourceURL)
	}
	if !semver.IsValid(latest) {
		return UpgradeResult{}, errors.New("upgrade: this hub does not advertise a released client version")
	}
	current := normalizeRelease(opts.CurrentRelease)
	name, err := upgradeBinaryName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return UpgradeResult{}, err
	}
	result := UpgradeResult{
		Current: current, Latest: latest, HubURL: base,
		BinaryURL: base + "/download/" + name,
	}
	if semver.IsValid(current) && semver.Compare(latest, current) <= 0 {
		result.UpToDate = true
		return result, nil
	}
	if opts.CheckOnly {
		return result, nil
	}
	if runtime.GOOS == "windows" {
		return result, fmt.Errorf("upgrade: automatic replacement is not supported on Windows yet; download %s", result.BinaryURL)
	}

	executable := opts.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return result, fmt.Errorf("upgrade: locate current executable: %w", err)
		}
		if resolved, rerr := filepath.EvalSymlinks(executable); rerr == nil {
			executable = resolved
		}
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return result, err
	}
	result.Path = executable
	st, err := os.Stat(executable)
	if err != nil || !st.Mode().IsRegular() {
		return result, fmt.Errorf("upgrade: current executable is not a regular file: %s", executable)
	}

	sums, err := upgradeGET(ctx, client, base+"/download/SHA256SUMS", token, maxUpgradeMetadata)
	if err != nil {
		return result, fmt.Errorf("upgrade: read checksums: %w", err)
	}
	want, err := checksumFor(sums, name)
	if err != nil {
		return result, err
	}
	tmp, err := downloadUpgrade(ctx, client, result.BinaryURL, token, filepath.Dir(executable), st.Mode().Perm(), want)
	if err != nil {
		return result, err
	}
	defer os.Remove(tmp)
	if err := verifyUpgradeBinary(ctx, tmp, latest); err != nil {
		return result, err
	}
	if err := os.Rename(tmp, executable); err != nil {
		return result, fmt.Errorf("upgrade: replace %s: %w", executable, err)
	}
	upgradeSyncDir(filepath.Dir(executable))
	result.Changed = true
	return result, nil
}

func upgradeHub(opts UpgradeOptions) (hubURL, token string, err error) {
	if strings.TrimSpace(opts.HubURL) != "" {
		hubURL = strings.TrimRight(strings.TrimSpace(opts.HubURL), "/")
		if opts.VaultDir != "" {
			if c, readErr := readCredentials(opts.VaultDir); readErr == nil && strings.TrimRight(c.HubURL, "/") == hubURL {
				token = c.Token
			}
		}
		return hubURL, token, nil
	}
	if opts.VaultDir == "" {
		return "", "", errors.New("upgrade: no hub specified; pass a joined vault or --hub")
	}
	c, err := readCredentials(opts.VaultDir)
	if err != nil {
		return "", "", fmt.Errorf("upgrade: %w; alternatively use `go install github.com/bright-interaction/mesh/cmd/mesh@latest`", err)
	}
	return strings.TrimRight(c.HubURL, "/"), c.Token, nil
}

func validateUpgradeHubURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("upgrade: invalid hub URL %q", raw)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname())) {
		return "", errors.New("upgrade: hub must use HTTPS (plain HTTP is allowed only for loopback testing)")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func upgradeHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || len(via) == 0 || req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
				return errors.New("upgrade: cross-origin or excessive redirect refused")
			}
			return nil
		},
	}
}

func upgradeGET(ctx context.Context, client *http.Client, rawURL, token string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("response too large")
	}
	return b, nil
}

func normalizeRelease(v string) string {
	v = strings.TrimSpace(v)
	if v != "" && v[0] >= '0' && v[0] <= '9' {
		v = "v" + v
	}
	return v
}

func releaseFromSourceURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	v := normalizeRelease(filepath.Base(strings.TrimRight(u.Path, "/")))
	if semver.IsValid(v) {
		return v
	}
	return ""
}

func upgradeBinaryName(goos, goarch string) (string, error) {
	if (goos != "darwin" && goos != "linux" && goos != "windows") || (goarch != "amd64" && goarch != "arm64") {
		return "", fmt.Errorf("upgrade: no prebuilt client for %s/%s", goos, goarch)
	}
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return "mesh-" + goos + "-" + goarch + ext, nil
}

func checksumFor(body []byte, name string) ([]byte, error) {
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum, err := hex.DecodeString(fields[0])
		if err == nil && len(sum) == sha256.Size {
			return sum, nil
		}
		break
	}
	return nil, fmt.Errorf("upgrade: SHA256SUMS has no valid checksum for %s", name)
}

func downloadUpgrade(ctx context.Context, client *http.Client, rawURL, token, dir string, mode os.FileMode, want []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upgrade: download client: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upgrade: download client: %s", resp.Status)
	}
	f, err := os.CreateTemp(dir, ".mesh-upgrade-*")
	if err != nil {
		return "", fmt.Errorf("upgrade: create file beside current executable: %w", err)
	}
	name := f.Name()
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(name)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxUpgradeBinary+1))
	if err != nil {
		return "", fmt.Errorf("upgrade: download client: %w", err)
	}
	if n > maxUpgradeBinary {
		return "", errors.New("upgrade: downloaded client exceeds 256 MiB")
	}
	if !bytes.Equal(h.Sum(nil), want) {
		return "", errors.New("upgrade: downloaded client does not match SHA256SUMS; current executable was not changed")
	}
	if mode&0o111 == 0 {
		mode = 0o755
	}
	if err := f.Chmod(mode); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	keep = true
	return name, nil
}

func verifyUpgradeBinary(ctx context.Context, path, release string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, path, "version", "--json")
	cmd.Env = cleanVersionEnv(os.Environ())
	stdout := boundedUpgradeBuffer{max: maxUpgradeMetadata}
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("upgrade: downloaded client failed its identity check: %w", err)
	}
	if stdout.overflow {
		return errors.New("upgrade: downloaded client identity response is too large")
	}
	var got binaryVersion
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || got.Name != "mesh" || normalizeRelease(got.Release) != release {
		return fmt.Errorf("upgrade: downloaded client identity does not match %s; current executable was not changed", release)
	}
	return nil
}

// boundedUpgradeBuffer keeps a downloaded executable from allocating arbitrary
// memory during its pre-install identity check. It reports successful writes to the
// child while retaining only max bytes; overflow is rejected after the child exits.
type boundedUpgradeBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *boundedUpgradeBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.max - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return n, nil
	}
	if len(p) > remaining {
		b.overflow = true
		_, _ = b.Buffer.Write(p[:remaining])
		return n, nil
	}
	return b.Buffer.Write(p)
}

func cleanVersionEnv(env []string) []string {
	out := env[:0]
	for _, item := range env {
		name := strings.SplitN(item, "=", 2)[0]
		if name != "MESH_VERSION" && name != "MESH_RELEASE_VERSION" {
			out = append(out, item)
		}
	}
	return out
}

func upgradeSyncDir(dir string) {
	d, err := os.Open(dir)
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
