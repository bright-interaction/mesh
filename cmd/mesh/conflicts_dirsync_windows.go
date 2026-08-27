// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

//go:build windows

package main

import "errors"

func resolutionSyncDir(dir string) error {
	// Microsoft documents FlushFileBuffers for a GENERIC_WRITE file handle, while its
	// exhaustive directory-handle function list does not include FlushFileBuffers.
	// Pretending that flushing a FILE_FLAG_BACKUP_SEMANTICS directory handle proves a
	// rename/delete durable would therefore be a false receipt. Keep the conflict
	// sibling and refuse before mutation until resolution uses a documented Windows
	// write-through primitive for the complete capture/delete protocol.
	return errors.New("durable conflict-resolution directory updates are not supported on Windows; the conflict file was retained")
}
