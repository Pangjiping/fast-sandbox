package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
)

func installPreparedOCI(t *testing.T, fixture *driverFixture, metadata string) {
	t.Helper()
	t.Setenv("FAST_SANDBOX_OCI_ENABLED", "true")
	installOCIRegistryConfig(t, registryconfig.Credential{Host: "r.example", Username: "pool-reader", Password: "pool-secret"})
	t.Setenv("TEST_PREPARE_ROOT", fixture.stateRoot)
	state, err := json.Marshal(map[string]any{"rootfs_device": map[string]string{"dev_path": "/dev/ublkb42"}, "template_metadata": json.RawMessage(metadata)})
	if err != nil {
		// Malformed output models a broken/incompatible CLI.
		state = []byte(metadata)
	}
	t.Setenv("TEST_PREPARE_STATE", string(state))
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "fsb-vm"), []byte(`#!/bin/sh
set -eu
while [ "$1" != create ] && [ "$1" != delete ]; do shift; done
home="$TEST_PREPARE_ROOT/oci/sandboxes/sandbox-1"
if [ "$1" = delete ]; then
  rm -rf "$home"
  exit 0
fi
cp "$TEST_PREPARE_ROOT/oci/inputs/sandbox-1.json" "$TEST_PREPARE_ROOT/cli-input.json"
mkdir -p "$home/mnt"
printf '%s' "$TEST_PREPARE_STATE" > "$home/state.json"
touch "$home/mnt/vmstate.bin" "$home/mnt/memfile"
if [ -n "${TEST_PREPARE_BUNDLE:-}" ]; then
  cp "$TEST_PREPARE_BUNDLE/"* "$home/mnt/"
fi
`), 0755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func preparedMetadata(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"oci":          map[string]string{"rootfs": "r.example/templates/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "snapfiles": "r.example/templates/base@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		"machine":      map[string]string{"vcpu": "2", "memory": "1Gi"},
		"guestNetwork": map[string]string{"ip": "172.30.0.3", "gateway": "172.30.0.1", "netmask": "255.255.255.0"},
	}
}

func TestOCICreateAndResumeUseCLIResultWithoutAgentMetadata(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "resume"}[resume], func(t *testing.T) {
			fixture := newDriverFixture(t)
			require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
			image := "oci://r.example/templates/base@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			fixture.sandboxSpec.Spec.Image = image
			metadata := preparedMetadata(t)
			if resume {
				fixture.sandboxSpec.Spec.Restore = &fastletapi.RestoreSpec{ManifestRef: image, ArtifactDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
				metadata["checkpoint"] = map[string]string{"sandboxUID": "sandbox-1", "namespace": "tenant-a"}
			}
			raw, err := json.Marshal(metadata)
			require.NoError(t, err)
			installPreparedOCI(t, fixture, string(raw))
			fixture.driver.newAgentClient = func(string) (AgentClient, error) {
				t.Fatal("OCI create/resume must not request node credentials")
				return nil, nil
			}
			fixture.driver.agentSocket = "/missing-agent.sock"
			_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
			require.NoError(t, err)
			require.Len(t, fixture.launcher.started, 1)
			require.NoFileExists(t, cachedManifestPath(fixture.stateRoot, image))
			input, err := os.ReadFile(filepath.Join(fixture.stateRoot, "cli-input.json"))
			require.NoError(t, err)
			require.Contains(t, string(input), image)
			require.Contains(t, string(input), "pool-reader")

			// A later pause must preserve lineage from the CLI state as well;
			// no source manifest exists in the old agent cache.
			staging := t.TempDir()
			for _, name := range []string{publishedRootfsName, vmstateSnapshotName, memorySnapshotName} {
				require.NoError(t, os.WriteFile(filepath.Join(staging, name), []byte("snapshot-data"), 0600))
			}
			sandboxDir := filepath.Join(fixture.stateRoot, sandboxStateDir, "sandbox-1")
			_, err = assembleSnapshotManifest(fixture.stateRoot, staging, sandboxDir, nil)
			require.NoError(t, err)
			raw, err = os.ReadFile(filepath.Join(staging, snapshotManifestName))
			require.NoError(t, err)
			var checkpoint map[string]any
			require.NoError(t, json.Unmarshal(raw, &checkpoint))
			require.Equal(t, "172.30.0.3", checkpoint["guestNetwork"].(map[string]any)["ip"])
		})
	}
}

func TestOCIPreparePreservesAdmissionAndReleasesDevices(t *testing.T) {
	for _, invalid := range []string{"owner", "memory", "network", "compatibility"} {
		t.Run(invalid, func(t *testing.T) {
			fixture := newDriverFixture(t)
			require.NoError(t, fixture.driver.Initialize(context.Background(), ""))
			fixture.sandboxSpec.Spec.Image = "oci://r.example/templates/base:build"
			metadata := preparedMetadata(t)
			switch invalid {
			case "owner":
				metadata["checkpoint"] = map[string]string{"sandboxUID": "someone-else", "namespace": "tenant-a"}
			case "memory":
				metadata["machine"] = map[string]string{"vcpu": "2", "memory": "2Gi"}
			case "network":
				metadata["guestNetwork"] = map[string]string{"ip": "invalid-ip", "gateway": "172.30.0.1", "netmask": "255.255.255.0"}
			case "compatibility":
				metadata["compatibility"] = map[string]any{"vendor": "invalid-vendor", "cpuTemplate": "invalid-template"}
			}
			raw, err := json.Marshal(metadata)
			require.NoError(t, err)
			installPreparedOCI(t, fixture, string(raw))
			_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
			require.Error(t, err)
			require.Empty(t, fixture.launcher.started)
			require.Zero(t, fixture.manager.Snapshot().Bound)
			require.NoDirExists(t, filepath.Join(fixture.stateRoot, "oci/sandboxes/sandbox-1"))
		})
	}
}
