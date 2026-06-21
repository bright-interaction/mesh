package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config drives `mesh ingest all`: a list of connector instances to pull on each
// run. It lives at <vault>/.mesh/ingest.json and holds NO secrets - tokens are
// read from env per connector type, keeping config safe to commit/share.
//
// Example:
//
//	{"connectors":[
//	  {"type":"github","owner":"bright-interaction","repo":"automations"},
//	  {"type":"slack","channel":"C0123456"},
//	  {"type":"linear"},
//	  {"type":"jira","site":"https://acme.atlassian.net","email":"me@acme.com"},
//	  {"type":"notion"}
//	]}
type Config struct {
	Connectors []ConnectorConfig `json:"connectors"`
}

// ConnectorConfig is one instance. Fields used depend on type.
type ConnectorConfig struct {
	Type    string `json:"type"`              // github|slack|linear|jira|notion
	Owner   string `json:"owner,omitempty"`   // github
	Repo    string `json:"repo,omitempty"`    // github
	Channel string `json:"channel,omitempty"` // slack
	Site    string `json:"site,omitempty"`    // jira
	Email   string `json:"email,omitempty"`   // jira
	JQL     string `json:"jql,omitempty"`     // jira (optional)
}

// LoadConfig reads .mesh/ingest.json (absent = empty config, not an error).
func LoadConfig(vaultRoot string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(vaultRoot, ".mesh", "ingest.json"))
	if err != nil {
		return c, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("ingest.json: %w", err)
	}
	return c, nil
}

// Build turns a ConnectorConfig into a live Connector, reading the token from the
// per-type env var (never from config). Returns an error for an unknown type or a
// missing required field.
func (cc ConnectorConfig) Build() (Connector, error) {
	switch cc.Type {
	case "github":
		if cc.Owner == "" || cc.Repo == "" {
			return nil, fmt.Errorf("github connector needs owner + repo")
		}
		return &GitHub{Owner: cc.Owner, Repo: cc.Repo, Token: os.Getenv("MESH_INGEST_GITHUB_TOKEN")}, nil
	case "slack":
		if cc.Channel == "" {
			return nil, fmt.Errorf("slack connector needs channel")
		}
		return &Slack{Channel: cc.Channel, Token: os.Getenv("MESH_INGEST_SLACK_TOKEN")}, nil
	case "linear":
		return &Linear{Token: os.Getenv("MESH_INGEST_LINEAR_TOKEN")}, nil
	case "jira":
		if cc.Site == "" {
			return nil, fmt.Errorf("jira connector needs site")
		}
		return &Jira{Site: cc.Site, Email: cc.Email, Token: os.Getenv("MESH_INGEST_JIRA_TOKEN"), JQL: cc.JQL}, nil
	case "notion":
		return &Notion{Token: os.Getenv("MESH_INGEST_NOTION_TOKEN")}, nil
	default:
		return nil, fmt.Errorf("unknown connector type %q", cc.Type)
	}
}
