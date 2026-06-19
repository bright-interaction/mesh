package code

import (
	"os"
	"path/filepath"
	"strings"
)

// maxFileBytes skips pathological inputs (checked-in bundles, generated blobs that
// slipped the dir filter). A 1 MiB source file is already far past anything a human
// wrote, and parsing it would dominate a reindex for no retrieval value.
const maxFileBytes = 1 << 20

// generatedRe markers: a file whose first ~5 lines carry the Go convention banner
// ("// Code generated ... DO NOT EDIT.") is machine output (sqlc, templ, protoc,
// mockgen). It holds thousands of symbols nobody searches by name, so it is
// skipped to keep the index signal-dense.
func isGenerated(src []byte) bool {
	head := src
	if len(head) > 2048 {
		head = head[:2048]
	}
	for _, line := range strings.Split(string(head), "\n")[:min(8, strings.Count(string(head), "\n")+1)] {
		l := strings.TrimSpace(line)
		if strings.Contains(l, "Code generated") && strings.Contains(l, "DO NOT EDIT") {
			return true
		}
	}
	return false
}

// ParseFile reads path and extracts its symbols. lang selects the extractor: Go
// uses the standard library AST (full fidelity); every other language uses the
// declaration scanner. mtime is the caller's stat so parsing and drift detection
// agree on the same clock. A nil CodeFile with nil error means the file was
// deliberately skipped (generated, too large); the caller drops it.
func ParseFile(path, rootRel, lang string, mtime int64) (*CodeFile, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(src) > maxFileBytes || isGenerated(src) {
		return nil, nil
	}
	cf := &CodeFile{Path: rootRel, Lang: lang, Mtime: mtime}
	switch lang {
	case "go":
		parseGo(cf, src)
	default:
		parseDecls(cf, src, lang)
	}
	if len(cf.Symbols) == 0 {
		return nil, nil
	}
	return cf, nil
}

// LangForPath is a convenience for callers (watcher, CLI) that have a path and need
// its language tag without re-deriving the extension rules.
func LangForPath(path string) string { return Lang(filepath.Ext(path)) }

// collapseWS replaces every run of whitespace (including newlines) with a single
// space and trims the ends, so a multi-line signature stores as one clean line.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
