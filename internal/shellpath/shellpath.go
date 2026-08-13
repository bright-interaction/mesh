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
	if safeBare(p) {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// safeBare reports whether p can be printed with no quotes at all.
//
// This is an ALLOW-list, and it replaced a deny-list of shell metacharacters that looked
// complete and was not. Two escapes were measured against real shells:
//
//   - A word-initial '=' is EQUALS EXPANSION in zsh, the default macOS shell and the one
//     a user actually pastes into. `mesh index =ls` runs /bin/ls. The deny-list had no
//     '=' because '=' is inert in sh and bash, and the test only ran sh, which on macOS
//     is bash in sh mode: the shell the defect lived in was never exercised.
//   - A bare carriage return survives `sh -c` intact, so no word-splitting test can see
//     it, and then acts as ENTER at the terminal. A path containing \r turns the printed
//     remedy into two commands, the second one attacker-chosen. Bracketed paste does not
//     save zsh. '\n' was on the deny-list and '\r' was not, which is the whole bug.
//
// Enumerating what is DANGEROUS is a losing game across three shells plus a terminal.
// Enumerating what is SAFE is not. Bytes >= 0x80 stay bare so ordinary non-ASCII paths
// print cleanly: no shell metacharacter is >= 0x80 in any encoding, and the default IFS
// is space/tab/newline only, so no UTF-8 continuation byte can split a word.
func safeBare(p string) bool {
	// A leading '-' makes the word a FLAG rather than a path and a leading '~' is tilde
	// expansion. Neither is fixable by quoting (see PathArg), but neither may go bare.
	if p[0] == '-' || p[0] == '~' {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '/', c == '.', c == '_', c == '-', c == '@', c == '+', c == ':', c == ',', c == '%':
		case c == '=' && i > 0:
			// zsh EQUALS-expands only a WORD-INITIAL '=', so a '=' inside a path is
			// ordinary and quoting it would be noise. i > 0 is the whole rule.
		case c >= 0x80:
		default:
			return false
		}
	}
	return true
}

// PathArg is Quote for a path in a POSITIONAL argument, where quoting alone is not enough:
// `mesh index -weird` is parsed as flags however it is quoted, because the shell hands the
// word over unchanged and the program's own flag parser sees the dash. A relative path
// starting with '-' is prefixed with "./", which is the same path and is no longer a flag.
// Quote stays right for a --flag=value position, where a leading dash is harmless.
func PathArg(p string) string {
	if strings.HasPrefix(p, "-") {
		return Quote("./" + p)
	}
	return Quote(p)
}
