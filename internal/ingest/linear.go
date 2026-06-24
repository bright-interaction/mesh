package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Linear imports issues via the GraphQL API. Personal API keys go in the
// Authorization header RAW (no "Bearer " prefix); OAuth tokens would use Bearer.
type Linear struct {
	Token  string
	Limit  int // issues to pull; 0 -> 100
	Client *http.Client
}

func (l *Linear) Name() string { return "linear" }
func (l *Linear) Key() string  { return "linear" }

type linearResp struct {
	Data struct {
		Issues struct {
			Nodes []struct {
				Identifier  string    `json:"identifier"`
				Title       string    `json:"title"`
				Description string    `json:"description"`
				URL         string    `json:"url"`
				CreatedAt   time.Time `json:"createdAt"`
				Creator     struct {
					Name string `json:"name"`
				} `json:"creator"`
			} `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"issues"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (l *Linear) Pull(ctx context.Context, since time.Time) ([]Doc, bool, error) {
	cl := l.Client
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	endpoint := "https://api.linear.app/graphql"
	if b := strings.TrimRight(apiBaseOverride, "/"); b != "" {
		endpoint = b
	}
	limit := l.Limit
	if limit <= 0 {
		limit = 100
	}
	// Incremental: filter by updatedAt when we have a high-water mark. Paginate the
	// result set to exhaustion via pageInfo.endCursor; a single `first: n` page
	// otherwise silently drops everything past the first n updated issues.
	filter := ""
	if !since.IsZero() {
		filter = "filter: {updatedAt: {gt: $since}}, "
	}
	decl := "$n: Int!, $after: String"
	if !since.IsZero() {
		decl += ", $since: DateTimeOrDuration"
	}
	query := fmt.Sprintf(`query(%s){ issues(first: $n, after: $after, %sorderBy: updatedAt){ nodes { identifier title description url createdAt creator { name } } pageInfo { hasNextPage endCursor } } }`, decl, filter)

	var docs []Doc
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxIngestPages {
			return docs, true, nil // truncated: caller holds the high-water mark
		}
		vars := map[string]any{"n": limit}
		if cursor != "" {
			vars["after"] = cursor
		}
		if !since.IsZero() {
			vars["since"] = since.UTC().Format(time.RFC3339)
		}
		reqBody, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Content-Type", "application/json")
		if l.Token != "" {
			req.Header.Set("Authorization", l.Token) // RAW, not Bearer (personal API key)
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
			return nil, false, httpError("linear", resp.StatusCode, body)
		}
		var lr linearResp
		if err := json.Unmarshal(body, &lr); err != nil {
			return nil, false, err
		}
		if len(lr.Errors) > 0 {
			return nil, false, fmt.Errorf("linear: %s", lr.Errors[0].Message)
		}
		for _, n := range lr.Data.Issues.Nodes {
			docs = append(docs, Doc{
				ExternalID: n.Identifier,
				Title:      "[" + n.Identifier + "] " + n.Title,
				Body:       n.Description,
				Author:     n.Creator.Name,
				SourceURL:  n.URL,
				CreatedAt:  n.CreatedAt.Format("2006-01-02"),
			})
		}
		pi := lr.Data.Issues.PageInfo
		if !pi.HasNextPage || pi.EndCursor == "" {
			break
		}
		cursor = pi.EndCursor
	}
	return docs, false, nil
}
