// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package code

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// Lang maps a file extension (with the dot, any case) to the language tag Mesh
// stores, or "" if the extension is not an indexed source language. It is the
// single source of truth for "is this a code file" used by both the walker and
// the watcher.
func Lang(ext string) string {
	switch strings.ToLower(ext) {
	case ".go":
		return "go"
	case ".ts":
		return "ts"
	case ".tsx":
		return "tsx"
	case ".js":
		return "js"
	case ".jsx":
		return "jsx"
	case ".mjs":
		return "mjs"
	case ".cjs":
		return "cjs"
	case ".svelte":
		return "svelte"
	case ".astro":
		return "astro"
	case ".py":
		return "py"
	}
	return ""
}

// codeSkipDirs are directories that never hold first-party source worth indexing:
// dependency trees, build output, framework caches, and the vault's own derived
// artifacts. Kept in sync with vault.skipDirs plus the build-artifact dirs that a
// note vault never contains but a code root always does.
var codeSkipDirs = map[string]bool{
	".git": true, ".mesh": true, "node_modules": true, "vendor": true,
	"graphify-out": true, "_archive": true, "dist": true, "build": true,
	"out": true, ".next": true, ".svelte-kit": true, ".turbo": true,
	".cache": true, "__pycache__": true, ".venv": true, "venv": true,
	"target": true, "coverage": true, ".astro": true,
}

func codeSkipDir(path, root, name string) bool {
	if path != root && strings.HasPrefix(name, ".") {
		return true
	}
	return codeSkipDirs[name]
}

// skipFile drops files that match an indexed extension but are not worth a symbol
// row: minified/bundled assets (one giant line of generated JS) and lockfiles that
// happen to end in .js. Generated-source detection (the "DO NOT EDIT" banner) is a
// content check and lives in the parser, not here.
func skipFile(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".min.js", ".bundle.js", ".min.ts", "-lock.js", ".d.ts"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// WalkCode returns every indexable source file under each root whose extension is
// allowed by langs (a set of language tags from Lang; nil means all known
// languages). Roots are walked independently and results concatenated, so a vault
// can index code from several repos. Returned paths are absolute; the caller makes
// them root-relative for storage.
func WalkCode(roots []string, langs map[string]bool) ([]string, error) {
	var out []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if codeSkipDir(path, root, d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			lang := Lang(filepath.Ext(path))
			if lang == "" || skipFile(d.Name()) {
				return nil
			}
			if langs != nil && !langs[lang] {
				return nil
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
