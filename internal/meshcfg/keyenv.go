// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshcfg

import "log/slog"

// A *.key_env field is a POINTER to a secret: whatever env var it names gets read out of
// the process environment and sent to the configured endpoint as a bearer/API key. So the
// set of names it may point at has to be closed. Without that, anyone who can write
// config.toml (the web config API, a hand edit on the box, a synced vault file) can aim
// key_env at MESH_UI_TOKEN, MESH_HUB_TOKEN, MESH_UI_COOKIE_SECRET or any other process
// secret and have Mesh deliver it to an endpoint they also chose.
//
// This is the single definition. The web config API validates writes against it, and
// every READ site resolves through ResolveKeyEnv, so a value that predates the allow-list
// or was hand-edited into config.toml is never dereferenced either.
//
// These three names are Mesh's own BYOAI key slots. There is deliberately no way to add
// to the set from configuration: an operator who wants Mesh to use a differently-named
// secret exports it under one of these names.
var allowedKeyEnv = map[string]bool{
	"MESH_EMBED_KEY":         true,
	"MESH_RERANK_KEY":        true,
	"MESH_SECRET_BRIDGE_KEY": true,
}

// AllowedKeyEnvList is the human-readable form of the allow-list, for error messages.
const AllowedKeyEnvList = "MESH_EMBED_KEY, MESH_RERANK_KEY, MESH_SECRET_BRIDGE_KEY"

// KeyEnvAllowed reports whether name may be used as a *.key_env value. Empty is allowed
// and means "use the built-in default for this field".
func KeyEnvAllowed(name string) bool { return name == "" || allowedKeyEnv[name] }

// ResolveKeyEnv returns the env var name a read site should actually dereference for a
// key_env field. Empty resolves to def; an allow-listed name resolves to itself; anything
// else falls back to def and is logged, so a hand-edited config.toml cannot make Mesh read
// an unrelated process secret. field is the config key ("embedding.key_env") and is used
// only in the log line.
func ResolveKeyEnv(field, name, def string) string {
	if name == "" {
		return def
	}
	if allowedKeyEnv[name] {
		return name
	}
	slog.Warn("mesh: ignoring a disallowed key_env in config.toml; reading the default instead",
		"field", field, "configured", name, "using", def, "allowed", AllowedKeyEnvList)
	return def
}
