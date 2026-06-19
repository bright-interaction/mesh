package code

import (
	"regexp"
	"strings"
)

// The declaration scanner is the pure-Go fallback for every non-Go language. It
// locates top-level (and, for Python, nested) declarations by line so Mesh can
// answer "where is X defined" without a per-language AST. It does not build a call
// graph: matching call sites reliably needs a real parser, and the daily-driver
// value is symbol location, which a line scan delivers for TS/JS/Svelte/Astro/Py.
// Svelte and Astro are scanned whole; their <script>/frontmatter declarations match
// the JS/TS patterns and their markup lines do not.
var (
	reJSFunc      = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s+([A-Za-z_$][\w$]*)`)
	reJSClass     = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	reJSInterface = regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?interface\s+([A-Za-z_$][\w$]*)`)
	reJSType      = regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?type\s+([A-Za-z_$][\w$]*)\s*[=<]`)
	reJSEnum      = regexp.MustCompile(`^\s*(?:export\s+)?(?:const\s+)?enum\s+([A-Za-z_$][\w$]*)`)
	reJSConst     = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?const\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=`)
	rePyDef       = regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+([A-Za-z_]\w*)`)
	rePyClass     = regexp.MustCompile(`^(\s*)class\s+([A-Za-z_]\w*)`)
)

func parseDecls(cf *CodeFile, src []byte, lang string) {
	lines := strings.Split(string(src), "\n")
	prevComment := ""
	add := func(name, kind string, lineNo int, sig string) {
		cf.Symbols = append(cf.Symbols, Symbol{
			Name: name, Kind: kind, Start: lineNo, End: lineNo,
			Signature: truncSig(sig), Doc: prevComment,
		})
	}
	for i, raw := range lines {
		lineNo := i + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			prevComment = ""
			continue
		}
		if lang == "py" {
			if m := rePyClass.FindStringSubmatch(raw); m != nil {
				add(m[2], "class", lineNo, trimmed)
			} else if m := rePyDef.FindStringSubmatch(raw); m != nil {
				kind := "func"
				if len(m[1]) > 0 {
					kind = "method" // indented def is a method/nested function
				}
				add(m[2], kind, lineNo, trimmed)
			}
		} else {
			switch {
			case reJSFunc.MatchString(raw):
				add(reJSFunc.FindStringSubmatch(raw)[1], "func", lineNo, trimmed)
			case reJSClass.MatchString(raw):
				add(reJSClass.FindStringSubmatch(raw)[1], "class", lineNo, trimmed)
			case reJSInterface.MatchString(raw):
				add(reJSInterface.FindStringSubmatch(raw)[1], "interface", lineNo, trimmed)
			case reJSEnum.MatchString(raw):
				add(reJSEnum.FindStringSubmatch(raw)[1], "enum", lineNo, trimmed)
			case reJSType.MatchString(raw):
				add(reJSType.FindStringSubmatch(raw)[1], "type", lineNo, trimmed)
			default:
				// A const is only a symbol worth indexing when it binds a function
				// (arrow or function expression); plain value consts are noise.
				if m := reJSConst.FindStringSubmatch(raw); m != nil && (strings.Contains(raw, "=>") || strings.Contains(raw, "function")) {
					add(m[1], "func", lineNo, trimmed)
				}
			}
		}
		if isCommentLine(trimmed) {
			prevComment = commentText(trimmed)
		} else {
			prevComment = ""
		}
	}
}

func isCommentLine(t string) bool {
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") ||
		strings.HasPrefix(t, "*") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "<!--")
}

func commentText(t string) string {
	t = strings.TrimLeft(t, "/*#<!- ")
	t = strings.TrimRight(t, "*/->")
	return truncSig(t)
}
