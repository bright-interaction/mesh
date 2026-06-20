package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

type ghIssue struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	CreatedAt   time.Time  `json:"created_at"`
	User        struct{ Login string } `json:"user"`
	PullRequest *struct{}  `json:"pull_request"`
}

func (g *GitHub) Pull(ctx context.Context, since time.Time) ([]Doc, error) {
	if g.Owner == "" || g.Repo == "" {
		return nil, fmt.Errorf("github: owner and repo are required")
	}
	cl := g.Client
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	base := "https://api.github.com"
	if b := strings.TrimRight(apiBaseOverride, "/"); b != "" {
		base = b
	}
	maxPages := g.MaxPages
	if maxPages <= 0 {
		maxPages = 5
	}
	var docs []Doc
	for page := 1; page <= maxPages; page++ {
		url := fmt.Sprintf("%s/repos/%s/%s/issues?state=all&per_page=100&page=%d", base, g.Owner, g.Repo, page)
		if !since.IsZero() {
			url += "&since=" + since.UTC().Format(time.RFC3339)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		if g.Token != "" {
			req.Header.Set("Authorization", "Bearer "+g.Token)
		}
		resp, err := cl.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, httpError("github", resp.StatusCode, body)
		}
		var issues []ghIssue
		if err := json.Unmarshal(body, &issues); err != nil {
			return nil, err
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
			break
		}
	}
	return docs, nil
}

// apiBaseOverride lets tests point connectors at a fake server. Empty in prod.
var apiBaseOverride string
