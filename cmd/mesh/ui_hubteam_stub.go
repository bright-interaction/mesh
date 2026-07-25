// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

//go:build !pro

package main

import "errors"

// openHubTeam is the stub for `mesh ui --hub-db` in this repository. Team mode resolves
// each request against the Mesh hub's client store, and the hub is a commercial product
// that is not part of this open-core repository, so there is no client store to read and
// the call returns a clear error instead. Plain `mesh ui` (no --hub-db) serves a solo
// vault and is fully covered by this repository. See LICENSING.md for what the
// commercial hub build adds.
func openHubTeam(_, _ string) (func(string) (int64, string, bool), func(int64) map[string]bool, func(int64) (string, int64, bool), func() error, error) {
	return nil, nil, nil, nil, errors.New("team mode (mesh ui --hub-db) needs the commercial Mesh hub build, which is not part of this open-core repository (see LICENSING.md); plain `mesh ui` serves a solo vault")
}
