// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

//go:build !pro

package main

import "errors"

// openHubTeam is the open-core stub for `mesh ui --hub-db`. Team mode resolves each
// request against the hub's client store, and the hub is the commercial (pro) layer,
// so the open build has no hub package to read and returns a clear error instead.
// Plain `mesh ui` (no --hub-db) serves a solo vault and is fully open. The pro build
// (-tags pro) replaces this with ui_hubteam_pro.go, which wires the real resolver.
func openHubTeam(_, _ string) (func(string) (int64, string, bool), func(int64) map[string]bool, func() error, error) {
	return nil, nil, nil, errors.New("team mode (mesh ui --hub-db) requires the pro build of mesh, which provides the team-sync hub; plain `mesh ui` serves a solo vault")
}
