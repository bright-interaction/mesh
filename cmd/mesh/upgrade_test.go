// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bright-interaction/mesh/pkg/meshclient"
)

func TestUpgradeCommandCheckJSON(t *testing.T) {
	orig := runPrebuiltUpgrade
	t.Cleanup(func() { runPrebuiltUpgrade = orig })
	runPrebuiltUpgrade = func(_ context.Context, opts meshclient.UpgradeOptions) (meshclient.UpgradeResult, error) {
		if !opts.CheckOnly || opts.HubURL != "https://mesh.example" {
			t.Fatalf("options = %+v", opts)
		}
		return meshclient.UpgradeResult{
			Current: "v0.12.0", Latest: "v0.13.0", HubURL: opts.HubURL,
			BinaryURL: opts.HubURL + "/download/mesh-linux-amd64",
		}, nil
	}
	var got meshclient.UpgradeResult
	if err := json.Unmarshal([]byte(runRoot(t, "upgrade", "--hub", "https://mesh.example", "--check", "--json")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Latest != "v0.13.0" || got.Changed || got.UpToDate {
		t.Fatalf("result = %+v", got)
	}
}
