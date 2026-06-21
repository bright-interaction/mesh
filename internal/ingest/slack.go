package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Slack imports a channel's recent messages into the vault. Auth is a bot/user
// token from the caller. Pure stdlib.
type Slack struct {
	Channel string // channel id, e.g. C0123456
	Token   string
	Limit   int // messages to pull; 0 -> 200
	Client  *http.Client
}

func (s *Slack) Name() string { return "slack" }
func (s *Slack) Key() string  { return "slack:" + s.Channel }

type slackResp struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Messages []struct {
		Type string `json:"type"`
		TS   string `json:"ts"`
		User string `json:"user"`
		Text string `json:"text"`
	} `json:"messages"`
}

func (s *Slack) Pull(ctx context.Context, since time.Time) ([]Doc, error) {
	if s.Channel == "" {
		return nil, fmt.Errorf("slack: channel is required")
	}
	cl := s.Client
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	base := "https://slack.com/api"
	if b := strings.TrimRight(apiBaseOverride, "/"); b != "" {
		base = b
	}
	limit := s.Limit
	if limit <= 0 {
		limit = 200
	}
	q := url.Values{}
	q.Set("channel", s.Channel)
	q.Set("limit", fmt.Sprint(limit))
	if !since.IsZero() {
		q.Set("oldest", fmt.Sprintf("%d.000000", since.Unix()))
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/conversations.history?"+q.Encode(), nil)
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("slack", resp.StatusCode, body)
	}
	var sr slackResp
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, err
	}
	if !sr.OK {
		return nil, fmt.Errorf("slack: %s", sr.Error)
	}
	var docs []Doc
	for _, m := range sr.Messages {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		title := firstLine(m.Text, 70)
		created := ""
		if secs := tsToUnix(m.TS); secs > 0 {
			created = time.Unix(secs, 0).UTC().Format("2006-01-02")
		}
		docs = append(docs, Doc{
			ExternalID: s.Channel + "-" + m.TS,
			Title:      "[slack] " + title,
			Body:       m.Text,
			Author:     m.User,
			SourceURL:  "slack://" + s.Channel + "/" + m.TS,
			CreatedAt:  created,
		})
	}
	return docs, nil
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

func tsToUnix(ts string) int64 {
	if i := strings.IndexByte(ts, '.'); i > 0 {
		ts = ts[:i]
	}
	var n int64
	for _, c := range ts {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
