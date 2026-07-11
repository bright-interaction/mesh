// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package meshclient is the Mesh team-sync client: the low-level RPC transport to
// a mesh-hub plus the high-level vault orchestration (join + reconcile) that
// reads and writes a vault's local sync state. It never runs git; it speaks the
// pull-based reconcile protocol over HTTPS. cmd/mesh drives it, and the future
// TUI RemoteBackend will reuse it.
package meshclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

// Client is the HTTP transport to one hub.
type Client struct {
	HubURL string
	Token  string
	HTTP   *http.Client
}

// New builds a client for a hub base URL and (optional) bearer token.
func New(hubURL, token string) *Client {
	return &Client{
		HubURL: strings.TrimRight(hubURL, "/"),
		Token:  token,
		HTTP:   &http.Client{Timeout: 60 * time.Second},
	}
}

// Join redeems a one-time invite for a client token (no bearer needed).
func (c *Client) Join(invite string) (syncproto.JoinResponse, error) {
	var jr syncproto.JoinResponse
	err := c.rpc("POST", "/v1/join", syncproto.JoinRequest{Invite: invite}, false, &jr)
	return jr, err
}

// Vault fetches vault metadata (authed).
func (c *Client) Vault() (syncproto.VaultInfo, error) {
	var vi syncproto.VaultInfo
	err := c.rpc("GET", "/v1/vault", nil, true, &vi)
	return vi, err
}

// Sync runs one reconcile round (authed).
func (c *Client) Sync(req syncproto.SyncRequest) (syncproto.SyncResponse, error) {
	var sr syncproto.SyncResponse
	err := c.rpc("POST", "/v1/sync", req, true, &sr)
	return sr, err
}

func (c *Client) rpc(method, path string, body any, authed bool, out any) error {
	var rdr io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		rdr = &buf
	}
	req, err := http.NewRequest(method, c.HubURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
