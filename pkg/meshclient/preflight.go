// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"encoding/base64"
	"fmt"

	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/syncproto"
)

// BlockedNote describes a local content problem, not a server acknowledgement.
// Hash identifies the exact checked bytes for watch-log deduplication only; it
// must never be used as an accepted sync baseline.
type BlockedNote struct {
	Path   string
	Reason string
	Hash   string
}

func preflightOutbox(outbox []syncproto.OutboxItem) ([]syncproto.OutboxItem, []BlockedNote, error) {
	kept := make([]syncproto.OutboxItem, 0, len(outbox))
	var blocked []BlockedNote
	for _, item := range outbox {
		if item.Op == "upsert" {
			content, err := base64.StdEncoding.DecodeString(item.ContentB64)
			if err != nil {
				return nil, nil, fmt.Errorf("sync: invalid local outbox encoding for %s: %w", item.Path, err)
			}
			if reason := merge.TextRejectionReason(content); reason != "" {
				blocked = append(blocked, BlockedNote{Path: item.Path, Reason: reason, Hash: contentHash(content)})
				continue
			}
		}
		kept = append(kept, item)
	}
	return kept, blocked, nil
}
