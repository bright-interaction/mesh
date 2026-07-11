// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package buildinfo exposes the running Mesh version and an optional
// source-availability link. It is shared by every network-served surface (the
// hub's human pages, the hub /about endpoint, and the web app shell) so the notice is
// rendered identically everywhere a user interacts with the program remotely.
package buildinfo

import (
	"html"
	"os"
)

// Version is the build version. Stamp it at build time with
//
//	-ldflags "-X github.com/bright-interaction/mesh/internal/buildinfo.Version=$(git rev-parse --short HEAD)"
//
// or override at runtime with MESH_VERSION. Defaults to "dev".
var Version = "dev"

// License is the SPDX identifier of the Mesh core.
const License = "LicenseRef-Mesh-Sustainable-Use-License"

// Ver returns the effective version: the MESH_VERSION env override if set, else the
// build-stamped Version.
func Ver() string {
	if v := os.Getenv("MESH_VERSION"); v != "" {
		return v
	}
	return Version
}

// SourceURL is the source-availability location for THIS version, from
// MESH_SOURCE_URL (point it at https://github.com/bright-interaction/mesh at the
// running tag/commit). It is empty when unset (e.g. before the open-core repo is
// published); callers then show no source link, and one env var turns the offer on.
func SourceURL() string { return os.Getenv("MESH_SOURCE_URL") }

// FooterInline renders the version notice as inline content (no wrapper): the
// running version and (when MESH_SOURCE_URL is set) a link to the source. The
// version and URL are operator-controlled env, escaped defensively. Links inherit
// color so the caller controls placement and theme.
func FooterInline() string {
	s := `Mesh ` + html.EscapeString(Ver())
	if src := SourceURL(); src != "" {
		s += ` &middot; <a href="` + html.EscapeString(src) + `" target="_blank" rel="noopener noreferrer" style="color:inherit;text-decoration:underline">Source code</a>`
	}
	return s
}

// FooterHTML wraps FooterInline in a muted, dark-theme-friendly document-flow footer,
// for the hub's full-page human surfaces (landing/invite/download/team).
func FooterHTML() string {
	return `<footer class="mesh-source" style="margin-top:2rem;padding-top:1rem;border-top:1px solid rgba(125,133,144,.25);color:#7d8590;font-size:.8rem;line-height:1.6;text-align:center">` +
		FooterInline() + `</footer>`
}
