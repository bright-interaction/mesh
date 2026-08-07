// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"os"
	"path/filepath"
	"testing"
)

// Generic vault-file helpers shared by this package's tests.
//
// These live HERE, and not in e2e_test.go where they started, because
// e2e_test.go is stripped from the public mirror as part of the pro hub-harness
// set (see PRO_PATHS in mesh/scripts/split-public-repo.sh: it defines setupHub
// and stands up a real internal/hub). Five OPEN tests in this package use these
// three helpers, so leaving them in the stripped file compiled fine in the
// monorepo and broke the mirror's own test suite the moment the strip ran:
// `FAIL github.com/bright-interaction/mesh/pkg/meshclient [build failed]`.
//
// Nothing here touches the hub, so nothing here is pro. Keep it that way: a
// helper that needs internal/hub belongs in e2e_test.go with setupHub, and a
// helper the open tests need belongs in this file.

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func exists(dir, rel string) bool {
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}
