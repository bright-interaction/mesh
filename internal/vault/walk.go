package vault

import (
	"io/fs"
	"path/filepath"
	"strings"
)

var skipDirs = map[string]bool{
	".git": true, ".mesh": true, "node_modules": true,
	"graphify-out": true, "_archive": true,
}

// Walk returns every markdown file under root, skipping noise and hidden dirs.
// This is the M0 walker; .meshignore support lands with the index step.
func Walk(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if skipDirs[name] {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}
