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
	"strconv"
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

// Sync runs one reconcile round (authed). It always declares the protocol this
// build speaks, so the hub can refuse a client that predates a response field
// instead of sending one it would silently mishandle (see syncproto.ProtoVersion).
func (c *Client) Sync(req syncproto.SyncRequest) (syncproto.SyncResponse, error) {
	req.Proto = syncproto.ProtoVersion
	var sr syncproto.SyncResponse
	err := c.rpc("POST", "/v1/sync", req, true, &sr)
	return sr, err
}

func (c *Client) rpc(method, path string, body any, authed bool, out any) error {
	_, err := c.rpcHeaders(method, path, body, authed, out)
	return err
}

// rpcHeaders is rpc plus the response headers, for the one contract that puts part of
// its answer in a header: the curation activity trail returns its paging position in
// one, because the response BODY type is shared with the pending-jobs route and adding
// a field there would change what every existing client decodes. A caller that cannot
// see the headers cannot page, which is exactly how the client ended up able to read
// only the newest page of an endpoint that had a cursor.
//
// The headers come back only on success; every error path returns nil, so a caller can
// never read a paging position off a response the hub refused.
func (c *Client) rpcHeaders(method, path string, body any, authed bool, out any) (http.Header, error) {
	var rdr io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
		rdr = &buf
	}
	req, err := http.NewRequest(method, c.HubURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Every request carries the protocol version, including the ones with no body,
	// so the hub's audit log can spot a stale client before it corrupts anything.
	req.Header.Set(syncproto.ProtoHeader, strconv.Itoa(syncproto.ProtoVersion))
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, err
		}
	}
	return resp.Header, nil
}
