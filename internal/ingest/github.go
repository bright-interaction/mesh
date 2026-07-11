// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// GitHub imports issues + pull requests (the issues endpoint returns both) for one
// repo into the vault. Auth is a token (PAT or fine-grained) from the caller; a
// preset OAuth app is a follow-on. Pure stdlib, no SDK.
type GitHub struct {
	Owner    string
	Repo     string
	Token    string
	MaxPages int // page cap (per_page=100); 0 -> 5
	Client   *http.Client
}

func (g *GitHub) Name() string { return "github" }
func (g *GitHub) Key() string  { return "github:" + g.Owner + "/" + g.Repo }

type ghIssue struct {
	Number      int                    `json:"number"`
	Title       string                 `json:"title"`
	Body        string                 `json:"body"`
	HTMLURL     string                 `json:"html_url"`
	CreatedAt   time.Time              `json:"created_at"`
	User        struct{ Login string } `json:"user"`
	PullRequest *struct{}              `json:"pull_request"`
}

func (g *GitHub) Pull(ctx context.Context, since time.Time) ([]Doc, bool, error) {
	if g.Owner == "" || g.Repo == "" {
		return nil, false, fmt.Errorf("github: owner and repo are required")
	}
	cl := g.Client
	if cl == nil {
		cl = safeClient(30 * time.Second)
	}
	base := "https://api.github.com"
	if b := strings.TrimRight(apiBaseOverride, "/"); b != "" {
		base = b
	}
	// Page to exhaustion (a short page ends it). g.MaxPages, when set, is an explicit
	// cap; either way maxIngestPages is the hard safety bound. Previously the default
	// 5-page cap silently dropped any backlog over 500 items because the high-water
	// mark advanced past the unread overflow.
	pageCap := g.MaxPages
	if pageCap <= 0 || pageCap > maxIngestPages {
		pageCap = maxIngestPages
	}
	var docs []Doc
	for page := 1; page <= pageCap; page++ {
		url := fmt.Sprintf("%s/repos/%s/%s/issues?state=all&per_page=100&page=%d", base, g.Owner, g.Repo, page)
		if !since.IsZero() {
			url += "&since=" + since.UTC().Format(time.RFC3339)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if g.Token != "" {
			req.Header.Set("Authorization", "Bearer "+g.Token)
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
			return nil, false, httpError("github", resp.StatusCode, body)
		}
		var issues []ghIssue
		if err := json.Unmarshal(body, &issues); err != nil {
			return nil, false, err
		}
		if len(issues) == 0 {
			break
		}
		for _, is := range issues {
			kind := "issue"
			if is.PullRequest != nil {
				kind = "pr"
			}
			docs = append(docs, Doc{
				ExternalID: fmt.Sprintf("%s-%s-%s-%d", g.Owner, g.Repo, kind, is.Number),
				Title:      fmt.Sprintf("[%s #%d] %s", strings.ToUpper(kind), is.Number, is.Title),
				Body:       is.Body,
				Author:     is.User.Login,
				SourceURL:  is.HTMLURL,
				CreatedAt:  is.CreatedAt.Format("2006-01-02"),
			})
		}
		if len(issues) < 100 {
			break // last page
		}
		if page == pageCap {
			return docs, true, nil // a full page at the cap: more remains upstream
		}
	}
	return docs, false, nil
}

// apiBaseOverride lets tests point connectors at a fake server. Empty in prod.
var apiBaseOverride string
