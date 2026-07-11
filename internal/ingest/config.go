// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"encoding/json"
	"fmt"
	"net/url"
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

// InstanceKey is the stable per-instance id, matching the built connector's Key().
// It needs no token, so callers (e.g. the hub Integrations UI) can look up a
// connector's saved high-water mark / last-run without constructing the connector.
func (cc ConnectorConfig) InstanceKey() string {
	switch cc.Type {
	case "github":
		return "github:" + cc.Owner + "/" + cc.Repo
	case "slack":
		return "slack:" + cc.Channel
	case "jira":
		return "jira:" + cc.Site
	case "linear":
		return "linear"
	case "notion":
		return "notion"
	default:
		return cc.Type
	}
}

// TokenEnv names the env var that holds this connector type's token (so a UI can
// report whether it is configured without ever reading the value).
func TokenEnv(connectorType string) string {
	switch connectorType {
	case "github":
		return "MESH_INGEST_GITHUB_TOKEN"
	case "slack":
		return "MESH_INGEST_SLACK_TOKEN"
	case "linear":
		return "MESH_INGEST_LINEAR_TOKEN"
	case "jira":
		return "MESH_INGEST_JIRA_TOKEN"
	case "notion":
		return "MESH_INGEST_NOTION_TOKEN"
	default:
		return ""
	}
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
	return cc.BuildWithToken("")
}

// BuildWithToken is Build but uses the supplied token when non-empty, falling back to
// the per-type env var otherwise. The hub passes a token an admin set through the
// Integrations UI (stored encrypted, never in config), so a non-vendor admin can make a
// connector work without editing the container env; a self-hosted deploy that prefers
// env tokens leaves it empty and gets the env value.
func (cc ConnectorConfig) BuildWithToken(token string) (Connector, error) {
	tok := func(env string) string {
		if token != "" {
			return token
		}
		return os.Getenv(env)
	}
	switch cc.Type {
	case "github":
		if cc.Owner == "" || cc.Repo == "" {
			return nil, fmt.Errorf("github connector needs owner + repo")
		}
		return &GitHub{Owner: cc.Owner, Repo: cc.Repo, Token: tok("MESH_INGEST_GITHUB_TOKEN")}, nil
	case "slack":
		if cc.Channel == "" {
			return nil, fmt.Errorf("slack connector needs channel")
		}
		return &Slack{Channel: cc.Channel, Token: tok("MESH_INGEST_SLACK_TOKEN")}, nil
	case "linear":
		return &Linear{Token: tok("MESH_INGEST_LINEAR_TOKEN")}, nil
	case "jira":
		if cc.Site == "" {
			return nil, fmt.Errorf("jira connector needs site")
		}
		// Reject anything but an https URL with a host at save time (UX + a first
		// guard); the runtime safeClient is what actually blocks SSRF to private IPs.
		if u, err := url.Parse(cc.Site); err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("jira site must be an https URL, e.g. https://acme.atlassian.net")
		}
		return &Jira{Site: cc.Site, Email: cc.Email, Token: tok("MESH_INGEST_JIRA_TOKEN"), JQL: cc.JQL}, nil
	case "notion":
		return &Notion{Token: tok("MESH_INGEST_NOTION_TOKEN")}, nil
	default:
		return nil, fmt.Errorf("unknown connector type %q", cc.Type)
	}
}
