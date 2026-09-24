// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package eval

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Execute the real helper with a fixture executable. No Mesh installation,
// model provider or user configuration is read by this test.
func TestABRerankUsesOneExplicitEndpointComparison(t *testing.T) {
	for _, status := range []string{"0", "7"} {
		t.Run("exit_"+status, func(t *testing.T) {
			dir := t.TempDir()
			binary, argsPath := filepath.Join(dir, "fixture mesh"), filepath.Join(dir, "args")
			stub := `#!/bin/sh
[ "$MESH_RERANK_AGENT" = http ] || exit 41
[ "$MESH_RERANK_ENDPOINT" = https://rerank.invalid ] || exit 42
[ "$MESH_RERANK_MODEL" = fixture-model ] || exit 43
[ ! -e "$MESH_AB_ARGS" ] || exit 44
printf '%s\n' "$@" > "$MESH_AB_ARGS"
printf '%s\n' 'fixture validity, local comparison, combined cost and fallback details'
exit "$MESH_AB_EXIT"
`
			if err := os.WriteFile(binary, []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", filepath.Join("..", "..", "eval", "ab-rerank.sh"), "vault with spaces", "cases with spaces.json")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "MESH=") && !strings.HasPrefix(entry, "MESH_RERANK_") && !strings.HasPrefix(entry, "MESH_AB_") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "MESH="+binary, "MESH_RERANK_AGENT=codex", "MESH_RERANK_ENDPOINT=https://rerank.invalid", "MESH_RERANK_MODEL=fixture-model", "MESH_AB_ARGS="+argsPath, "MESH_AB_EXIT="+status)
			out, err := cmd.CombinedOutput()
			if status == "0" && err != nil {
				t.Fatalf("helper failed: %v: %s", err, out)
			}
			if status == "7" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 7 {
					t.Fatalf("helper hid evaluator failure: %v: %s", err, out)
				}
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"eval", "cases with spaces.json", "--vault", "vault with spaces", "--budget", "0", "--require-rerank-win"}
			if got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"); !reflect.DeepEqual(got, want) {
				t.Fatalf("arguments = %q, want %q", got, want)
			}
			if !strings.Contains(string(out), "fixture validity, local comparison, combined cost and fallback details") {
				t.Fatalf("helper filtered away evaluation evidence: %s", out)
			}
		})
	}
}
