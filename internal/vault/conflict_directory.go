// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"os"
	"path/filepath"
)

// PrepareSiblingDirectory prepares the one extra component an overflow sibling
// needs before a live inode is renamed into it. Refuse pre-existing symlinks:
// adding an overflow directory must not introduce an escape from the base's
// already-validated parent. As elsewhere in vault, directory fsync is best effort.
func PrepareSiblingDirectory(base, sibling string) error {
	parent, dir := filepath.Dir(base), filepath.Dir(sibling)
	if parent == dir {
		return nil
	}
	if filepath.Dir(dir) != parent || !IsConflictSibling(filepath.Base(dir)) {
		return fmt.Errorf("conflict directory is not immediately below the base directory")
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("conflict directory must be a real directory")
	}
	syncDir(parent)
	return nil
}
