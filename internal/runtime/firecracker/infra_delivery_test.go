package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"

	"github.com/stretchr/testify/require"
)

// infraRunner records commands and can fail a specific command.
type infraRunner struct {
	commands []string
	failOn   string
}

func (r *infraRunner) Run(_ context.Context, command string, args ...string) ([]byte, error) {
	line := command + " " + strings.Join(args, " ")
	r.commands = append(r.commands, line)
	if r.failOn != "" && strings.Contains(line, r.failOn) {
		return nil, os.ErrPermission
	}
	return nil, nil
}

func infraMounts(t *testing.T) []fastletinfra.Mount {
	t.Helper()
	sandboxInit := filepath.Join(t.TempDir(), "sandbox-init")
	infraJSON := filepath.Join(t.TempDir(), "infra.json")
	execd := filepath.Join(t.TempDir(), "execd")
	for _, path := range []string{sandboxInit, infraJSON, execd} {
		require.NoError(t, os.WriteFile(path, []byte("payload"), 0o755))
	}
	return []fastletinfra.Mount{
		{Source: sandboxInit, Destination: "/.fast/bin/sandbox-init", Options: []string{"ro"}},
		{Source: infraJSON, Destination: "/.fast/run/infra.json", Options: []string{"ro"}},
		{Source: execd, Destination: "/.fast/components/execd/execd", Options: []string{"ro"}},
	}
}

func TestDeliverGuestInfraCopiesArtifacts(t *testing.T) {
	runner := &infraRunner{}
	instance := fastletinfra.PreparedInstance{Mounts: infraMounts(t)}
	err := deliverGuestInfra(context.Background(), runner, "/var/lib/fast-sandbox/rootfs.img", instance)
	require.NoError(t, err)

	joined := strings.Join(runner.commands, "\n")
	require.Contains(t, joined, "mount -o loop /var/lib/fast-sandbox/rootfs.img")
	for _, want := range []string{
		"mkdir -p ", "cp -a ",
		"/.fast/bin/sandbox-init", "/.fast/run/infra.json", "/.fast/components/execd/execd",
	} {
		require.Contains(t, joined, want)
	}
	require.Equal(t, 1, strings.Count(joined, "umount "))
}

func TestDeliverGuestInfraSkipsEmptyPlan(t *testing.T) {
	runner := &infraRunner{}
	require.NoError(t, deliverGuestInfra(context.Background(), runner, "img", fastletinfra.PreparedInstance{}))
	require.Empty(t, runner.commands)
}

func TestDeliverGuestInfraUnmountsOnFailure(t *testing.T) {
	runner := &infraRunner{failOn: "execd"}
	err := deliverGuestInfra(context.Background(), runner, "img", fastletinfra.PreparedInstance{Mounts: infraMounts(t)})
	require.Error(t, err)
	require.Contains(t, strings.Join(runner.commands, "\n"), "umount ")
}

func TestDeliverGuestInfraRequiresRunner(t *testing.T) {
	err := deliverGuestInfra(context.Background(), nil, "img", fastletinfra.PreparedInstance{Mounts: infraMounts(t)})
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestDeliverGuestNetworkConfigWritesStaticConfig(t *testing.T) {
	runner := &infraRunner{}
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	require.NoError(t, os.MkdirAll(filepath.Join(stateRoot, imageCacheDir, imageKey(image)), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(stateRoot, imageCacheDir, imageKey(image), guestNetHookMarker), nil, 0o640))
	slot := &fastletnetwork.Slot{IP: "172.30.0.2", Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24"}
	err := deliverGuestNetworkConfig(context.Background(), runner, stateRoot, image, "/var/lib/fast-sandbox/rootfs.img", slot)
	require.NoError(t, err)

	joined := strings.Join(runner.commands, "\n")
	require.Contains(t, joined, "mount -o loop /var/lib/fast-sandbox/rootfs.img")
	require.Contains(t, joined, "mkdir -p ")
	require.Contains(t, joined, "/etc/fast-sandbox")
	require.Contains(t, joined, "cp -a ")
	require.Equal(t, 1, strings.Count(joined, "umount "))
}

func TestDeliverGuestNetworkConfigSkipsWithoutHook(t *testing.T) {
	runner := &infraRunner{}
	stateRoot := t.TempDir()
	image := "example.com/app:v1"
	slot := &fastletnetwork.Slot{IP: "172.30.0.2", Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24"}
	require.NoError(t, deliverGuestNetworkConfig(context.Background(), runner, stateRoot, image, "img", slot))
	require.Empty(t, runner.commands)
}

func TestDeliverGuestNetworkConfigSkipsNilSlot(t *testing.T) {
	runner := &infraRunner{}
	require.NoError(t, deliverGuestNetworkConfig(context.Background(), runner, t.TempDir(), "img", "img", nil))
	require.Empty(t, runner.commands)
}

func TestDeliverGuestNetworkConfigRequiresRunner(t *testing.T) {
	slot := &fastletnetwork.Slot{IP: "172.30.0.2", Gateway: "172.30.0.1", PrivateCIDR: "172.30.0.0/24"}
	require.NoError(t, deliverGuestNetworkConfig(context.Background(), nil, t.TempDir(), "img", "img", slot))
}
