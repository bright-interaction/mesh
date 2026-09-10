// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package rerank

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const localConfigVersion = 1

// SubscriptionConfig is one user's opt-in for one vault. It deliberately lives
// outside the vault, project, and agent MCP files: none of those shared surfaces
// may make a teammate send knowledge through a subscription account.
type SubscriptionConfig struct {
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Policy string `json:"policy"`
}

type localConfig struct {
	Version int                           `json:"version"`
	Vaults  map[string]SubscriptionConfig `json:"vaults"`
}

// LocalConfigPath returns the user-private file used for subscription choices.
func LocalConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(dir, "mesh", "subscription-rerank.json"), nil
}

// LoadLocalSubscription reads one vault's local opt-in. Missing file/entry means
// off. The returned path is always populated when the user config dir resolves.
func LoadLocalSubscription(vaultRoot string) (SubscriptionConfig, bool, string, error) {
	var zero SubscriptionConfig
	p, err := LocalConfigPath()
	if err != nil {
		return zero, false, "", err
	}
	cfg, err := readLocalConfig(p)
	if err != nil {
		return zero, false, p, err
	}
	key, err := canonicalVault(vaultRoot)
	if err != nil {
		return zero, false, p, err
	}
	v, ok := cfg.Vaults[key]
	if !ok {
		return zero, false, p, nil
	}
	if err := validateSubscription(v); err != nil {
		return zero, false, p, fmt.Errorf("invalid subscription rerank config for %s: %w", key, err)
	}
	return v, true, p, nil
}

// SaveLocalSubscription records an explicit user opt-in for one vault.
func SaveLocalSubscription(vaultRoot string, sub SubscriptionConfig) (string, bool, error) {
	if err := validateSubscription(sub); err != nil {
		return "", false, err
	}
	p, err := LocalConfigPath()
	if err != nil {
		return "", false, err
	}
	cfg, err := readLocalConfig(p)
	if err != nil {
		return p, false, err
	}
	key, err := canonicalVault(vaultRoot)
	if err != nil {
		return p, false, err
	}
	if old, ok := cfg.Vaults[key]; ok && old == sub {
		return p, false, nil
	}
	cfg.Vaults[key] = sub
	return p, true, writeLocalConfig(p, cfg)
}

// RemoveLocalSubscription disables one vault without touching other vaults.
func RemoveLocalSubscription(vaultRoot string) (string, bool, error) {
	p, err := LocalConfigPath()
	if err != nil {
		return "", false, err
	}
	cfg, err := readLocalConfig(p)
	if err != nil {
		return p, false, err
	}
	key, err := canonicalVault(vaultRoot)
	if err != nil {
		return p, false, err
	}
	if _, ok := cfg.Vaults[key]; !ok {
		return p, false, nil
	}
	delete(cfg.Vaults, key)
	return p, true, writeLocalConfig(p, cfg)
}

func validateSubscription(sub SubscriptionConfig) error {
	model, err := DefaultSubscriptionModel(sub.Agent)
	if err != nil {
		return err
	}
	if strings.TrimSpace(sub.Model) == "" {
		return fmt.Errorf("model is empty (default for %s is %s)", sub.Agent, model)
	}
	if sub.Policy != "auto" && sub.Policy != "always" {
		return fmt.Errorf("policy %q is invalid (want auto|always)", sub.Policy)
	}
	return nil
}

func canonicalVault(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}

func readLocalConfig(p string) (localConfig, error) {
	cfg := localConfig{Version: localConfigVersion, Vaults: map[string]SubscriptionConfig{}}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return cfg, err
	}
	if len(data) > 1<<20 {
		return cfg, fmt.Errorf("%s exceeds the 1 MiB local-config limit", p)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", p, err)
	}
	if cfg.Version != localConfigVersion {
		return cfg, fmt.Errorf("unsupported %s version %d (want %d)", p, cfg.Version, localConfigVersion)
	}
	if cfg.Vaults == nil {
		cfg.Vaults = map[string]SubscriptionConfig{}
	}
	return cfg, nil
}

func writeLocalConfig(p string, cfg localConfig) error {
	dirPath := filepath.Dir(p)
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dirPath, 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp, err := os.CreateTemp(dirPath, ".subscription-rerank-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	clean := func() { _ = os.Remove(tmpPath) }
	defer clean()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
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
	if err := os.Rename(tmpPath, p); err != nil {
		return err
	}
	return localConfigSyncDir(dirPath)
}

func localConfigSyncDir(dirPath string) error {
	dir, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}
