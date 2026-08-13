// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ErrNoVault is returned when a caller points Mesh at a vault root that is not there.
// Callers match it with errors.Is so a typo'd path reads the same wherever it enters.
var ErrNoVault = errors.New("no vault")

// RequireRoot refuses a vault root that does not already exist, so a mistyped path is
// an error naming the path instead of a brand new vault built out of nothing.
//
// The failure it exists to prevent: `mesh new decision "Typo test" --vault ./notez`
// used to print "created notez/decisions/typo-test.md" and exit 0. os.MkdirAll happily
// invented every missing parent, so the note landed in a phantom vault with no .mesh,
// no index and no reason for anyone to ever look in it. index.Open does the same thing
// one level down: it MkdirAlls <root>/.mesh, so `mesh watch ./notez`, `mesh mcp --vault
// ./notez`, `mesh sync ./notez` and the whole `mesh code` family each minted their own
// empty vault from a typo. Seven entry points, one missing question: does this vault
// exist?
//
// The rule is "an existing directory", deliberately NOT "a directory containing .mesh".
// A vault with no .mesh yet is a real, supported state: SPEC section 12 makes `mesh
// migrate <vault>` then `mesh index <vault>` over a plain folder of markdown the v1
// gate, and .mesh is what that pass CREATES. Requiring it here would refuse the
// documented first run to catch a typo that happens to name an existing directory,
// which trades a real workflow for a rare case. Existence is the check that closes the
// measured defect and costs nothing.
//
// Two commands are exempt because creating the vault IS their job, and both say so in
// their own help: `mesh init [path]` bootstraps one, and `mesh join <hub> <invite>
// [vault]` clones a team vault into a fresh directory (README: "mesh join
// https://mesh.example.com <invite-token> my-vault"). Neither calls this.
func RequireRoot(root string) error {
	st, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w at %s: that path does not exist, and Mesh will not create a vault from a typo. "+
				"Check the path, or run `mesh init %s` if you really want a new vault there", ErrNoVault, root, root)
		}
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%w at %s: that path is a file, not a vault directory", ErrNoVault, root)
	}
	return nil
}
