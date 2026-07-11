#!/bin/sh
# SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
# Copyright (C) 2026 Bright Interaction AB
# mesh-hub container entrypoint. `init` is idempotent (it reuses an existing
# vault_id and leaves existing files), so it is safe to run on every boot: the
# first boot bootstraps the repo + hub.db on the mounted volume, later boots are
# a no-op. Then hand off to the long-running server.
set -e
mesh-hub init "$MESH_HUB_REPO" --gc-horizon "$MESH_HUB_GC_HORIZON"
exec mesh-hub serve --repo "$MESH_HUB_REPO" --addr "$MESH_HUB_ADDR"
