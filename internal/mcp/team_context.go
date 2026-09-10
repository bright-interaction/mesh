// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

// TeamReuseObserver is the hub-owned, aggregate-only side of cross-user flywheel
// measurement. Actor ids are opaque, keyed identifiers and must never reach the wire.
type TeamReuseObserver interface {
	RecordTeamWriteback(noteID, path, actorID, source string, authoredAt int64) error
	RecordTeamReuse(noteID, path, actorID string, reusedAt int64) error
}

// TeamCaller is authenticated identity supplied by the team hub for one request.
type TeamCaller struct {
	ActorID  string
	User     string
	Email    string
	Observer TeamReuseObserver
}

type teamCallerKey struct{}

func WithTeamCaller(ctx context.Context, caller TeamCaller) context.Context {
	return context.WithValue(ctx, teamCallerKey{}, caller)
}

func TeamCallerFromContext(ctx context.Context) (TeamCaller, bool) {
	if ctx == nil {
		return TeamCaller{}, false
	}
	c, ok := ctx.Value(teamCallerKey{}).(TeamCaller)
	return c, ok
}

// NotePublisher is the durable publication boundary for one write-back. Local MCP
// uses the filesystem creator; the hub replaces it with a Git-transactional publisher.
type NotePublisher func(context.Context, vault.NewNoteSpec) (*vault.CreateResult, error)

func (s *Server) publishNote(ctx context.Context, spec vault.NewNoteSpec) (*vault.CreateResult, error) {
	if s.notePublisher != nil {
		return s.notePublisher(ctx, spec)
	}
	return vault.CreateNoteContext(ctx, s.vaultRoot, spec)
}

// SetNotePublisher must be called before the server is exposed to requests.
func (s *Server) SetNotePublisher(p NotePublisher) { s.notePublisher = p }

func attributionActor(ctx context.Context) string {
	if c, ok := TeamCallerFromContext(ctx); ok && c.ActorID != "" {
		return c.ActorID
	}
	return "local"
}

func observeTeamWriteback(ctx context.Context, noteID, path, source string, at time.Time) {
	if c, ok := TeamCallerFromContext(ctx); ok && c.ActorID != "" && c.Observer != nil {
		_ = c.Observer.RecordTeamWriteback(noteID, path, c.ActorID, source, at.Unix())
	}
}

func observeTeamReuse(ctx context.Context, noteID, path string, at time.Time) {
	if c, ok := TeamCallerFromContext(ctx); ok && c.ActorID != "" && c.Observer != nil {
		_ = c.Observer.RecordTeamReuse(noteID, path, c.ActorID, at.Unix())
	}
}
