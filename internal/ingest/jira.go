// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Jira imports issues via the Jira Cloud REST v3 enhanced search endpoint
// (GET /rest/api/3/search/jql; the old /search was removed in late 2025). Auth is
// HTTP Basic with email + API token.
type Jira struct {
	Site   string // https://your-domain.atlassian.net
	Email  string
	Token  string
	JQL    string // base filter; default "order by created DESC"
	Max    int    // page size; 0 -> 100
	Client *http.Client
}

func (j *Jira) Name() string { return "jira" }
func (j *Jira) Key() string  { return "jira:" + j.Site }

type jiraResp struct {
	Issues []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary     string          `json:"summary"`
			Description json.RawMessage `json:"description"` // ADF document
			Created     string          `json:"created"`
			Creator     struct {
				DisplayName string `json:"displayName"`
			} `json:"creator"`
		} `json:"fields"`
	} `json:"issues"`
	NextPageToken string `json:"nextPageToken"`
	IsLast        bool   `json:"isLast"`
}

func (j *Jira) Pull(ctx context.Context, since time.Time) ([]Doc, bool, error) {
	if j.Site == "" {
		return nil, false, fmt.Errorf("jira: site is required")
	}
	cl := j.Client
	if cl == nil {
		cl = safeClient(30 * time.Second)
	}
	base := strings.TrimRight(j.Site, "/")
	if b := strings.TrimRight(apiBaseOverride, "/"); b != "" {
		base = b
	}
	jql := j.JQL
	if strings.TrimSpace(jql) == "" {
		jql = "order by created DESC"
	}
	if !since.IsZero() {
		// Prepend an incremental filter; Jira wants minute precision, quoted.
		jql = fmt.Sprintf(`updated >= "%s" AND (%s)`, since.UTC().Format("2006-01-02 15:04"), jql)
	}
	max := j.Max
	if max <= 0 {
		max = 100
	}
	// Paginate the enhanced /search/jql endpoint to exhaustion via nextPageToken; a
	// single page otherwise silently drops everything past maxResults.
	var docs []Doc
	pageToken := ""
	for page := 0; ; page++ {
		if page >= maxIngestPages {
			return docs, true, nil // truncated: caller holds the high-water mark
		}
		q := url.Values{}
		q.Set("jql", jql)
		q.Set("maxResults", fmt.Sprint(max))
		q.Set("fields", "summary,description,created,creator")
		if pageToken != "" {
			q.Set("nextPageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rest/api/3/search/jql?"+q.Encode(), nil)
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Accept", "application/json")
		if j.Email != "" || j.Token != "" {
			cred := base64.StdEncoding.EncodeToString([]byte(j.Email + ":" + j.Token))
			req.Header.Set("Authorization", "Basic "+cred)
		}
		resp, err := cl.Do(req)
		if err != nil {
			return nil, false, err
		}
		body, err := readBody(resp)
		if err != nil {
			return nil, false, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, false, httpError("jira", resp.StatusCode, body)
		}
		var jr jiraResp
		if err := json.Unmarshal(body, &jr); err != nil {
			return nil, false, err
		}
		for _, is := range jr.Issues {
			created := is.Fields.Created
			if len(created) >= 10 {
				created = created[:10]
			}
			docs = append(docs, Doc{
				ExternalID: is.Key,
				Title:      "[" + is.Key + "] " + is.Fields.Summary,
				Body:       adfText(is.Fields.Description),
				Author:     is.Fields.Creator.DisplayName,
				SourceURL:  base + "/browse/" + is.Key,
				CreatedAt:  created,
			})
		}
		if jr.IsLast || jr.NextPageToken == "" {
			break
		}
		pageToken = jr.NextPageToken
	}
	return docs, false, nil
}

// adfText flattens Atlassian Document Format (a nested JSON doc) to plain text by
// collecting every "text" leaf in order. Good enough to make an issue searchable.
func adfText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	var b strings.Builder
	walkADF(node, &b)
	return strings.TrimSpace(b.String())
}

func walkADF(node map[string]any, b *strings.Builder) {
	if t, ok := node["text"].(string); ok {
		b.WriteString(t)
	}
	if node["type"] == "paragraph" || node["type"] == "heading" {
		b.WriteString("\n")
	}
	if content, ok := node["content"].([]any); ok {
		for _, c := range content {
			if cm, ok := c.(map[string]any); ok {
				walkADF(cm, b)
			}
		}
	}
}
