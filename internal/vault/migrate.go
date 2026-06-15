package vault

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MigrateResult records what a migration pass did to one file.
type MigrateResult struct {
	Path    string
	Changed bool
	Actions []string // changes applied (or that would be, in dry-run)
	Issues  []string // flywheel fields still needing human authoring (never fabricated)
}

var wikilinkTarget = regexp.MustCompile(`\[\[([^\]|#]+)`)

// MigrateFile brings one file up to the Mesh schema, idempotently:
//   - synthesize a stable id from the filename when missing,
//   - add type: note when absent (concept/map are valid, so they are kept),
//   - map updated -> when (falling back to the file mtime),
//   - lift a "## Related" section's [[links]] into a related: array.
//
// It never fabricates do/dont/why; missing flywheel fields are reported in
// Issues. With dryRun it reports without writing. Re-running is a no-op.
func MigrateFile(path string, dryRun bool) (*MigrateResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fmText, body, had := SplitFrontmatter(string(data))
	fm, raw, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{Path: path}
	var add []string

	if _, ok := raw["id"]; !ok || strings.TrimSpace(fm.ID) == "" {
		id := Slugify(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
		add = append(add, "id: "+id)
		res.Actions = append(res.Actions, "added id "+id)
	}
	if _, ok := raw["type"]; !ok {
		add = append(add, "type: note")
		res.Actions = append(res.Actions, "added type note")
	}
	if _, ok := raw["when"]; !ok {
		when, src := fm.Updated, "updated"
		if when == "" {
			when, src = fileMtimeDate(path), "file mtime"
		}
		add = append(add, `when: "`+when+`"`)
		res.Actions = append(res.Actions, "set when from "+src)
	}
	if _, ok := raw["related"]; !ok {
		if links := relatedLinks(body); len(links) > 0 {
			block := "related:"
			for _, l := range links {
				block += "\n  - " + l
			}
			add = append(add, block)
			res.Actions = append(res.Actions, "lifted "+strconv.Itoa(len(links))+" related links")
		}
	}

	if fm.Type.RequiresFlywheel() {
		for _, e := range fm.Validate() {
			if strings.HasPrefix(e, "do ") || strings.HasPrefix(e, "dont ") || strings.HasPrefix(e, "why ") {
				res.Issues = append(res.Issues, e)
			}
		}
	}

	if len(add) == 0 {
		return res, nil // already clean: idempotent no-op
	}
	res.Changed = true
	if dryRun {
		return res, nil
	}

	newFM := strings.Join(add, "\n")
	if fmText != "" {
		newFM += "\n" + fmText
	}
	out := "---\n" + newFM + "\n---\n" + body
	if !had {
		out = "---\n" + newFM + "\n---\n\n" + body
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return nil, err
	}
	return res, nil
}

func fileMtimeDate(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return Now().Format("2006-01-02")
	}
	return fi.ModTime().Format("2006-01-02")
}

// relatedLinks collects the [[basenames]] under a "## Related" (or "## See also")
// section, deduped and in order.
func relatedLinks(body string) []string {
	var out []string
	seen := map[string]bool{}
	inRelated := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "## ") {
			h := strings.ToLower(strings.TrimSpace(t[3:]))
			inRelated = h == "related" || h == "see also"
			continue
		}
		if !inRelated {
			continue
		}
		for _, m := range wikilinkTarget.FindAllStringSubmatch(line, -1) {
			name := strings.TrimSuffix(strings.TrimSpace(m[1]), ".md")
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}
