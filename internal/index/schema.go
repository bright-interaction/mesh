// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import _ "embed"

// SchemaSQL is the SQLite DDL applied when the store is created (used by the
// store step; embedded now so the schema travels with the binary).
//
//go:embed schema.sql
var SchemaSQL string
