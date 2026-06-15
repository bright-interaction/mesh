package vault

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// NoteType is the kind of a note. The first five are the canonical Mesh types.
// concept and map are accepted so `mesh migrate` can ingest a Hive-style vault
// losslessly (Open Decision 4 default: extend the enum rather than remap).
type NoteType string

const (
	TypeNote       NoteType = "note"
	TypePostMortem NoteType = "post-mortem"
	TypeDecision   NoteType = "decision"
	TypeGotcha     NoteType = "gotcha"
	TypeEntity     NoteType = "entity"
	TypeConcept    NoteType = "concept"
	TypeMap        NoteType = "map"
)

var validTypes = map[NoteType]bool{
	TypeNote: true, TypePostMortem: true, TypeDecision: true,
	TypeGotcha: true, TypeEntity: true, TypeConcept: true, TypeMap: true,
}

func (t NoteType) Valid() bool { return validTypes[t] }

// RequiresFlywheel reports whether do/dont/why are mandatory for this type.
// These are the institutional-memory types that fuel tier-0 retrieval.
func (t NoteType) RequiresFlywheel() bool {
	return t == TypeDecision || t == TypeGotcha || t == TypePostMortem
}

// StringList decodes either a YAML scalar or a sequence into []string, so
// `related: foo` and `related: [foo, bar]` both work.
type StringList []string

func (s *StringList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		if value.Value == "" {
			*s = nil
			return nil
		}
		*s = StringList{value.Value}
		return nil
	}
	var xs []string
	if err := value.Decode(&xs); err != nil {
		return err
	}
	*s = xs
	return nil
}

// Frontmatter is the whitelisted view of a note's YAML header. Raw YAML is
// never spread into storage; only known keys are kept (the JSONB house rule).
type Frontmatter struct {
	ID         string     `yaml:"id"`
	Type       NoteType   `yaml:"type"`
	Title      string     `yaml:"title"`
	When       string     `yaml:"when"`
	Created    string     `yaml:"created,omitempty"`
	Updated    string     `yaml:"updated,omitempty"`
	Related    StringList `yaml:"related,omitempty"`
	Tags       StringList `yaml:"tags,omitempty"`
	Do         string     `yaml:"do,omitempty"`
	Dont       string     `yaml:"dont,omitempty"`
	Why        string     `yaml:"why,omitempty"`
	Status     string     `yaml:"status,omitempty"`
	Supersedes StringList `yaml:"supersedes,omitempty"`
	Severity   string     `yaml:"severity,omitempty"`
	Role       string     `yaml:"role,omitempty"`
	Stack      StringList `yaml:"stack,omitempty"`
	RepoPath   string     `yaml:"repo_path,omitempty"`
}

// ParseFrontmatter decodes a YAML frontmatter block into the whitelisted struct
// and a raw map. Empty input yields a zero Frontmatter, not an error.
func ParseFrontmatter(b []byte) (*Frontmatter, map[string]any, error) {
	fm := &Frontmatter{}
	raw := map[string]any{}
	if len(b) > 0 {
		if err := yaml.Unmarshal(b, fm); err != nil {
			return nil, nil, fmt.Errorf("frontmatter: %w", err)
		}
		if err := yaml.Unmarshal(b, &raw); err != nil {
			return nil, nil, fmt.Errorf("frontmatter raw: %w", err)
		}
	}
	if fm.Type == "" {
		fm.Type = TypeNote
	}
	return fm, raw, nil
}

// Validate returns the lint problems for this frontmatter. An empty slice means
// it satisfies the schema for its type.
func (f *Frontmatter) Validate() []string {
	var errs []string
	if f.ID == "" {
		errs = append(errs, "missing id")
	}
	if !f.Type.Valid() {
		errs = append(errs, fmt.Sprintf("invalid type %q", f.Type))
	}
	if f.When == "" {
		errs = append(errs, "missing when")
	}
	if f.Type.RequiresFlywheel() {
		if unfilled(f.Do) {
			errs = append(errs, "do not filled (required for "+string(f.Type)+")")
		}
		if unfilled(f.Dont) {
			errs = append(errs, "dont not filled (required for "+string(f.Type)+")")
		}
		if unfilled(f.Why) {
			errs = append(errs, "why not filled (required for "+string(f.Type)+")")
		}
	}
	return errs
}

// unfilled reports whether a flywheel field is still empty or a TODO placeholder
// (mesh new leaves "TODO" sentinels for the author to replace).
func unfilled(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || strings.HasPrefix(strings.ToUpper(s), "TODO")
}

// SplitFrontmatter separates a leading YAML frontmatter block from the body. It
// returns the inner YAML (no --- markers), the body after the closing marker,
// and whether a block was present.
func SplitFrontmatter(content string) (fm string, body string, had bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return "", content, false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", content, false
}
