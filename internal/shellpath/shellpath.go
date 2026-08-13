// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package shellpath renders a filesystem path for the one purpose Mesh keeps needing
// it: printing a command the user is meant to copy, paste and run.
//
// Every "fix: mesh index <vault>" line in this program is a runnable command, and a
// vault path with a space in it (~/Library/Mobile Documents/..., "~/My Notes", and the
// macOS default "~/Documents") silently becomes two arguments when pasted. The remedy
// Mesh prints for a broken index was itself broken for exactly the users least able to
// debug it, and it fails in the worst way: `mesh index ~/My Notes` does not error, it
// indexes ~/My and reports success.
package shellpath

import "strings"

// Quote returns p ready to paste into a POSIX shell as a single argument.
//
// Unquoted when it needs nothing (the overwhelming case, and quoting every path would
// make ordinary output noisy). Otherwise single-quoted, which is the only shell quoting
// with no interior escapes at all: everything between the quotes is literal, so a path
// containing $, backticks, backslashes or globs needs no further treatment. A literal
// single quote is the one character single-quoting cannot carry, and it is closed,
// escaped and reopened in the usual '\” form.
func Quote(p string) string {
	if p == "" {
		return "''"
	}
	if !strings.ContainsAny(p, " \t\n\"'\\$`&|;<>()*?[]{}~#!^") {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
