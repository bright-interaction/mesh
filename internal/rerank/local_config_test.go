// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package rerank

import (
	"os"
	"path/filepath"
	"testing"
)

func isolateUserConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

func TestLocalSubscriptionIsPerUserAndPerVault(t *testing.T) {
	isolateUserConfig(t)
	one, two := t.TempDir(), t.TempDir()
	sub := SubscriptionConfig{Agent: "codex", Model: "gpt-5.6-luna", Policy: "auto"}
	p, changed, err := SaveLocalSubscription(one, sub)
	if err != nil || !changed {
		t.Fatalf("save changed=%v err=%v", changed, err)
	}
	if _, changed, err := SaveLocalSubscription(one, sub); err != nil || changed {
		t.Fatalf("identical save changed=%v err=%v", changed, err)
	}
	got, ok, gotPath, err := LoadLocalSubscription(one)
	if err != nil || !ok || got != sub || gotPath != p {
		t.Fatalf("load got=%+v ok=%v path=%q err=%v", got, ok, gotPath, err)
	}
	if _, ok, _, err := LoadLocalSubscription(two); err != nil || ok {
		t.Fatalf("second vault inherited first vault's opt-in: ok=%v err=%v", ok, err)
	}
	second := SubscriptionConfig{Agent: "claude", Model: "claude-haiku-4-5-20251001", Policy: "auto"}
	if _, changed, err := SaveLocalSubscription(two, second); err != nil || !changed {
		t.Fatalf("save second vault changed=%v err=%v", changed, err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("local config mode = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("local config directory mode = %o, want 700", dirInfo.Mode().Perm())
	}
	if p == filepath.Join(one, ".mesh", "config.toml") || filepath.Dir(p) == one {
		t.Fatalf("subscription opt-in was stored in the vault: %s", p)
	}
	if _, changed, err := RemoveLocalSubscription(one); err != nil || !changed {
		t.Fatalf("remove changed=%v err=%v", changed, err)
	}
	if _, ok, _, err := LoadLocalSubscription(one); err != nil || ok {
		t.Fatalf("subscription still enabled after remove: ok=%v err=%v", ok, err)
	}
	if got, ok, _, err := LoadLocalSubscription(two); err != nil || !ok || got != second {
		t.Fatalf("removing one vault damaged another: got=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestLocalSubscriptionRejectsInvalidOrCorruptConfig(t *testing.T) {
	isolateUserConfig(t)
	if _, _, err := SaveLocalSubscription(t.TempDir(), SubscriptionConfig{Agent: "fable", Model: "large", Policy: "auto"}); err == nil {
		t.Fatal("unsupported provider should fail")
	}
	p, err := LocalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadLocalSubscription(t.TempDir()); err == nil {
		t.Fatal("corrupt user config should fail loudly")
	}
}
