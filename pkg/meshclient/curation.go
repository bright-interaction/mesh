package meshclient

import (
	"fmt"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

// ClientForVault builds an authed transport from a joined vault's stored
// credentials (.mesh/credentials). Used by the mesh-curator to call the hub's
// curation endpoints with the same token it syncs with.
func ClientForVault(vaultDir string) (*Client, error) {
	creds, err := readCredentials(vaultDir)
	if err != nil {
		return nil, err
	}
	return New(creds.HubURL, creds.Token), nil
}

// CurationJobs lists the hub's pending curation markers (metadata only).
func (c *Client) CurationJobs() ([]syncproto.CurationJob, error) {
	var resp syncproto.CurationJobsResponse
	if err := c.rpc("GET", "/v1/curation/jobs", nil, true, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// CurationJob fetches one job including its IncomingB64 (the losing version).
func (c *Client) CurationJob(id int64) (syncproto.CurationJob, error) {
	var job syncproto.CurationJob
	err := c.rpc("GET", fmt.Sprintf("/v1/curation/jobs/%d", id), nil, true, &job)
	return job, err
}

// ResolveCuration marks a job resolved, recording the HEAD the merge landed at.
func (c *Client) ResolveCuration(id int64, resolvedHead string) error {
	body := map[string]string{"resolved_head": resolvedHead}
	return c.rpc("POST", fmt.Sprintf("/v1/curation/jobs/%d/resolve", id), body, true, nil)
}

// FailCuration charges a failed attempt against a job; the hub marks it terminal
// at the attempt cap so a job the agent cannot satisfy stops being retried.
func (c *Client) FailCuration(id int64, reason string) error {
	body := map[string]string{"reason": reason}
	return c.rpc("POST", fmt.Sprintf("/v1/curation/jobs/%d/fail", id), body, true, nil)
}
