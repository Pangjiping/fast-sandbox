package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/auth"

	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

func installOCIRegistryConfig(t *testing.T, credentials ...registryconfig.Credential) {
	t.Helper()
	compiled, err := registryconfig.NewCompiled(credentials)
	require.NoError(t, err)
	raw, err := json.Marshal(compiled)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "registries.json")
	require.NoError(t, os.WriteFile(file, raw, 0600))
	t.Setenv("FAST_SANDBOX_REGISTRY_CONFIG_PATH", file)
}

func TestCheckpointCredentialsSeparateParentReadAndDestinationWrite(t *testing.T) {
	installOCIRegistryConfig(t,
		registryconfig.Credential{Host: "r.example", RepositoryPrefix: "templates", Username: "parent-reader", Password: "read-secret"},
		registryconfig.Credential{Host: "other.example", RepositoryPrefix: "templates", Username: "other-reader", Password: "other-secret"},
		registryconfig.Credential{Host: "r.example", RepositoryPrefix: "checkpoints", IdentityToken: "checkpoint-read-token", WriteUsername: "writer", WritePassword: "write-secret"},
	)
	credentials, err := checkpointCredentials("r.example/checkpoints/sandbox:pause")
	require.NoError(t, err)
	for _, test := range []struct {
		reference string
		want      auth.Credential
	}{
		{"r.example/checkpoints/sandbox:pause_rootfs", auth.Credential{Username: "writer", Password: "write-secret"}},
		{"r.example/templates/base:rootfs", auth.Credential{Username: "parent-reader", Password: "read-secret"}},
		{"other.example/templates/base:rootfs", auth.Credential{Username: "other-reader", Password: "other-secret"}},
		{"public.example/base:rootfs", auth.EmptyCredential},
	} {
		got, err := credentials(context.Background(), test.reference)
		require.NoError(t, err)
		require.Equal(t, test.want, got, test.reference)
	}
	_, err = checkpointCredentials("missing.example/checkpoints/sandbox:pause")
	require.ErrorContains(t, err, "no registry credential")
}

func TestAttachOCIPreservesBasicAuthAndCleansPartialResources(t *testing.T) {
	for _, mode := range []string{"success", "create-failed", "missing-state", "corrupt-state", "empty-device", "cleanup-failed", "existing-home"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			d := &Driver{config: firecrackerConfigForTest(t, root)}
			image := "oci://r.example/templates/base:build"
			manifest := cachedManifestPath(root, image)
			require.NoError(t, os.MkdirAll(filepath.Dir(manifest), 0750))
			require.NoError(t, os.WriteFile(manifest, []byte(`{"oci":{"rootfs":"r.example/templates/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","snapfiles":"r.example/templates/base@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`), 0600))
			installOCIRegistryConfig(t, registryconfig.Credential{Host: "r.example", RepositoryPrefix: "templates", Username: "reader", Password: "read-secret"})
			t.Setenv("TEST_OCI_MODE", mode)
			t.Setenv("TEST_OCI_ROOT", root)
			bin := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(bin, "fsb-vm"), []byte(`#!/bin/sh
set -eu
while [ "$1" != create ] && [ "$1" != delete ]; do shift; done
home="$TEST_OCI_ROOT/oci/sandboxes/sandbox-1"
if [ "$1" = delete ]; then
  touch "$TEST_OCI_ROOT/deleted"
  if [ "$TEST_OCI_MODE" = cleanup-failed ]; then echo cleanup-failed >&2; exit 1; fi
  rm -rf "$home"
  exit 0
fi
cp "$TEST_OCI_ROOT/oci/inputs/sandbox-1.json" "$TEST_OCI_ROOT/captured.json"
mkdir -p "$home"
case "$TEST_OCI_MODE" in
  create-failed) exit 1;;
  missing-state) exit 0;;
  corrupt-state|cleanup-failed) echo '{broken' > "$home/state.json";;
  empty-device) echo '{}' > "$home/state.json";;
  *) echo '{"rootfs_device":{"dev_path":"/dev/ublkb42"}}' > "$home/state.json";;
esac
`), 0755))
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			home := filepath.Join(root, "oci/sandboxes/sandbox-1")
			if mode == "existing-home" {
				require.NoError(t, os.MkdirAll(home, 0750))
			}
			device, _, _, err := d.attachOCI(context.Background(), "sandbox-1", image)
			if mode == "success" {
				require.NoError(t, err)
				require.Equal(t, "/dev/ublkb42", device)
				require.DirExists(t, home)
				require.NoFileExists(t, filepath.Join(root, "deleted"))
			} else {
				require.Error(t, err)
				if mode == "existing-home" {
					require.DirExists(t, home)
					require.NoFileExists(t, filepath.Join(root, "deleted"))
					return
				}
				require.FileExists(t, filepath.Join(root, "deleted"))
				if mode == "cleanup-failed" {
					require.ErrorContains(t, err, "cleanup-failed")
					require.DirExists(t, home)
				} else {
					require.NoDirExists(t, home)
				}
			}
			raw, err := os.ReadFile(filepath.Join(root, "captured.json"))
			require.NoError(t, err)
			var input struct {
				Template struct {
					Auth struct {
						DockerAuth string `json:"dockerAuth"`
					} `json:"auth"`
				} `json:"template"`
			}
			require.NoError(t, json.Unmarshal(raw, &input))
			var docker struct {
				Auths map[string]struct {
					Username string `json:"username"`
					Password string `json:"password"`
					Token    string `json:"identitytoken"`
				} `json:"auths"`
			}
			require.NoError(t, json.Unmarshal([]byte(input.Template.Auth.DockerAuth), &docker))
			require.Equal(t, "reader", docker.Auths["r.example/templates"].Username)
			require.Equal(t, "read-secret", docker.Auths["r.example/templates"].Password)
			require.Empty(t, docker.Auths["r.example/templates"].Token)
			require.NoFileExists(t, filepath.Join(root, "oci/inputs/sandbox-1.json"))
		})
	}
}

func TestDeliverGuestInfraMountsOCIDeviceDirectly(t *testing.T) {
	runner := &infraRunner{}
	require.NoError(t, deliverGuestInfra(context.Background(), runner, "/dev/ublkb42", fastletinfra.PreparedInstance{Mounts: infraMounts(t)}, true))
	require.Contains(t, runner.commands[0], "mount /dev/ublkb42 ")
	for _, command := range runner.commands {
		require.NotContains(t, command, "loop")
	}
}

func TestOCITokenOnlyCredentialsFailBeforeAttachmentOrPacking(t *testing.T) {
	root := t.TempDir()
	d := &Driver{config: firecrackerConfigForTest(t, root)}
	image := "oci://r.example/templates/base:build"
	manifest := cachedManifestPath(root, image)
	require.NoError(t, os.MkdirAll(filepath.Dir(manifest), 0750))
	require.NoError(t, os.WriteFile(manifest, []byte(`{"oci":{"rootfs":"r.example/templates/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","snapfiles":"r.example/templates/base@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`), 0600))
	installOCIRegistryConfig(t, registryconfig.Credential{Host: "r.example", IdentityToken: "private-token"})
	_, _, _, err := d.attachOCI(context.Background(), "sandbox-1", image)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.ErrorContains(t, err, "identitytoken-only credentials are not supported")
	require.NoDirExists(t, filepath.Join(root, "oci/inputs"))
	_, err = checkpointCredentials("r.example/checkpoints/sandbox:pause")
	require.ErrorContains(t, err, "identitytoken-only credentials are not supported")
}

func TestSnapshotStagingRecoveryBeforeRetryAndDelete(t *testing.T) {
	for _, operation := range []string{"retry", "delete"} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail=%t", operation, failed), func(t *testing.T) {
				fixture := newDriverFixture(t)
				require.NoError(t, os.MkdirAll(filepath.Join(fixture.driver.config.StateRoot, sandboxStateDir, fixture.sandboxSpec.Identity.SandboxUID), 0750))
				require.NoError(t, saveState(filepath.Join(fixture.driver.config.StateRoot, sandboxStateDir, fixture.sandboxSpec.Identity.SandboxUID), &SandboxState{Config: fixture.sandboxSpec, Phase: PhaseStopped}))
				staging := filepath.Join(fixture.driver.config.StateRoot, snapshotStagingDir, "snap-1")
				require.NoError(t, os.MkdirAll(staging, 0750))
				marker := filepath.Join(staging, ".pack-resources-active")
				require.NoError(t, os.WriteFile(marker, []byte("retained"), 0600))
				bin := t.TempDir()
				t.Setenv("TEST_PACK_STAGING", staging)
				t.Setenv("TEST_PACK_FAIL", strconv.FormatBool(failed))
				require.NoError(t, os.WriteFile(filepath.Join(bin, "fsb-vm"), []byte(`#!/bin/sh
set -eu
test -f "$TEST_PACK_STAGING/.pack-resources-active"
printf '%s\n' "$@" > "$TEST_PACK_STAGING/arguments"
if [ "$TEST_PACK_FAIL" = true ]; then exit 7; fi
rm "$TEST_PACK_STAGING/.pack-resources-active"
touch "$TEST_PACK_STAGING/cleaned"
`), 0755))
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				var err error
				if operation == "delete" {
					err = fixture.driver.DeleteSnapshot(context.Background(), "snap-1")
				} else {
					// Invalid live state makes the snapshot fail after stale staging recovery.
					_, err = fixture.driver.CreateSnapshot(context.Background(), &runtimecontract.SnapshotInput{SandboxID: fixture.sandboxSpec.Identity.SandboxUID, SnapshotID: "snap-1", Kind: fastletapi.SnapshotKindCheckpoint})
				}
				if failed {
					require.ErrorContains(t, err, "recover checkpoint pack resources")
					require.FileExists(t, marker)
					raw, readErr := os.ReadFile(filepath.Join(staging, "arguments"))
					require.NoError(t, readErr)
					require.Contains(t, string(raw), "cleanup-pack\n--bundle-dir\n"+staging)
				} else {
					if operation == "delete" {
						require.NoError(t, err)
					}
					require.NoDirExists(t, staging)
				}
			})
		}
	}
}

func TestOCIRecordsOwnershipBeforePrepareAndRecoversInterruptedAttachment(t *testing.T) {
	fixture := newDriverFixture(t)
	fixture.sandboxSpec.Spec.Image = "oci://r.example/templates/base:build"
	fixture.prepareCachedImage(t, fixture.sandboxSpec.Spec.Image)
	manifest := cachedManifestPath(fixture.stateRoot, fixture.sandboxSpec.Spec.Image)
	raw, err := os.ReadFile(manifest)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	doc["oci"] = map[string]string{"rootfs": "r.example/templates/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "snapfiles": "r.example/templates/base@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	raw, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifest, raw, 0600))
	installOCIRegistryConfig(t, registryconfig.Credential{Host: "r.example", Username: "reader", Password: "secret"})
	bin := t.TempDir()
	t.Setenv("TEST_OCI_ROOT", fixture.stateRoot)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "fsb-vm"), []byte(`#!/bin/sh
set -eu
while [ "$1" != create ] && [ "$1" != delete ]; do shift; done
if [ "$1" = create ]; then
 cp "$TEST_OCI_ROOT/sandboxes/sandbox-1/meta.json" "$TEST_OCI_ROOT/at-prepare.json"
 exit 7
fi
rm -rf "$TEST_OCI_ROOT/oci/sandboxes/sandbox-1"
touch "$TEST_OCI_ROOT/recovered"
`), 0755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.ErrorContains(t, err, "attach OCI template")
	raw, err = os.ReadFile(filepath.Join(fixture.stateRoot, "at-prepare.json"))
	require.NoError(t, err)
	var state SandboxState
	require.NoError(t, json.Unmarshal(raw, &state))
	require.Equal(t, fixture.sandboxSpec, state.Config)
	require.Equal(t, PhaseStarting, state.Phase)
	require.Zero(t, state.PID)
	// Model process death immediately after prepare-only succeeded: only the
	// ownership record exists; neither API address nor VMM PID was persisted.
	home := filepath.Join(fixture.stateRoot, "oci/sandboxes/sandbox-1")
	require.NoError(t, os.MkdirAll(home, 0750))
	require.NoError(t, os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"rootfs_device":{"dev_id":42}}`), 0600))
	require.NoError(t, fixture.driver.RecoverRuntimeResources(context.Background(), nil))
	require.NoDirExists(t, home)
	require.NoDirExists(t, filepath.Join(fixture.stateRoot, sandboxStateDir, "sandbox-1"))
	require.FileExists(t, filepath.Join(fixture.stateRoot, "recovered"))
	require.Equal(t, 0, fixture.manager.Snapshot().Bound)
	// A retry reaches prepare again rather than rejecting an existing OCI home.
	require.NoError(t, os.Remove(filepath.Join(fixture.stateRoot, "at-prepare.json")))
	_, err = fixture.driver.EnsureSandbox(context.Background(), ensureInput(&fixture.sandboxSpec))
	require.ErrorContains(t, err, "attach OCI template")
	require.FileExists(t, filepath.Join(fixture.stateRoot, "at-prepare.json"))
}
