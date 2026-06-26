package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Notion imports pages via the search endpoint. Auth is a Bearer integration
// token; the Notion-Version header is required. v1 indexes page title + url +
// timestamps (block content needs per-page calls; a follow-on).
type Notion struct {
	Token   string
	Version string // Notion-Version header; default below
	Limit   int    // page_size; 0 -> 100
	Client  *http.Client
}

func (n *Notion) Name() string { return "notion" }
func (n *Notion) Key() string  { return "notion" }

const notionVersion = "2022-06-28" // long-stable; override via Notion.Version

type notionResp struct {
	Results []struct {
		ID             string                    `json:"id"`
		URL            string                    `json:"url"`
		CreatedTime    time.Time                 `json:"created_time"`
		LastEditedTime time.Time                 `json:"last_edited_time"`
		Properties     map[string]notionProperty `json:"properties"`
	} `json:"results"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

type notionProperty struct {
	Type  string `json:"type"`
	Title []struct {
		PlainText string `json:"plain_text"`
	} `json:"title"`
}

func (n *Notion) Pull(ctx context.Context, since time.Time) ([]Doc, bool, error) {
	cl := n.Client
	if cl == nil {
		cl = safeClient(30 * time.Second)
	}
	endpoint := "https://api.notion.com/v1/search"
	if b := strings.TrimRight(apiBaseOverride, "/"); b != "" {
		endpoint = b + "/v1/search"
	}
	limit := n.Limit
	if limit <= 0 {
		limit = 100
	}
	ver := n.Version
	if ver == "" {
		ver = notionVersion
	}
	// Paginate via start_cursor. Results are sorted newest-edited first, so once a
	// whole page predates `since` we can stop (everything after is older). Without
	// pagination a workspace with more than page_size recently-edited pages silently
	// dropped the older edits beyond the first page.
	var docs []Doc
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxIngestPages {
			return docs, true, nil // truncated: caller holds the high-water mark
		}
		reqMap := map[string]any{
			"filter":    map[string]any{"value": "page", "property": "object"},
			"page_size": limit,
			"sort":      map[string]any{"direction": "descending", "timestamp": "last_edited_time"},
		}
		if cursor != "" {
			reqMap["start_cursor"] = cursor
		}
		reqBody, _ := json.Marshal(reqMap)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Notion-Version", ver)
		if n.Token != "" {
			req.Header.Set("Authorization", "Bearer "+n.Token)
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
			return nil, false, httpError("notion", resp.StatusCode, body)
		}
		var nr notionResp
		if err := json.Unmarshal(body, &nr); err != nil {
			return nil, false, err
		}
		allBeforeSince := !since.IsZero() && len(nr.Results) > 0
		for _, p := range nr.Results {
			if !since.IsZero() && p.LastEditedTime.Before(since) {
				continue
			}
			allBeforeSince = false
			title := notionTitle(p.Properties)
			if title == "" {
				title = "Untitled Notion page"
			}
			docs = append(docs, Doc{
				ExternalID: p.ID,
				Title:      "[notion] " + title,
				Body:       title,
				SourceURL:  p.URL,
				CreatedAt:  p.CreatedTime.Format("2006-01-02"),
			})
		}
		if allBeforeSince || !nr.HasMore || nr.NextCursor == "" {
			break
		}
		cursor = nr.NextCursor
	}
	return docs, false, nil
}

// notionTitle pulls the page title from whichever property has type "title".
func notionTitle(props map[string]notionProperty) string {
	for _, p := range props {
		if p.Type != "title" {
			continue
		}
		var b strings.Builder
		for _, t := range p.Title {
			b.WriteString(t.PlainText)
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}
