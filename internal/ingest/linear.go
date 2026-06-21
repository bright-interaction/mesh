package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		} `json:"issues"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (l *Linear) Pull(ctx context.Context, since time.Time) ([]Doc, error) {
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
	// Incremental: filter by updatedAt when we have a high-water mark.
	filter := ""
	vars := map[string]any{"n": limit}
	if !since.IsZero() {
		filter = "filter: {updatedAt: {gt: $since}}, "
		vars["since"] = since.UTC().Format(time.RFC3339)
	}
	decl := "$n: Int!"
	if !since.IsZero() {
		decl += ", $since: DateTimeOrDuration"
	}
	query := fmt.Sprintf(`query(%s){ issues(first: $n, %sorderBy: updatedAt){ nodes { identifier title description url createdAt creator { name } } } }`, decl, filter)
	reqBody, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if l.Token != "" {
		req.Header.Set("Authorization", l.Token) // RAW, not Bearer (personal API key)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("linear", resp.StatusCode, body)
	}
	var lr linearResp
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, err
	}
	if len(lr.Errors) > 0 {
		return nil, fmt.Errorf("linear: %s", lr.Errors[0].Message)
	}
	var docs []Doc
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
	return docs, nil
}
