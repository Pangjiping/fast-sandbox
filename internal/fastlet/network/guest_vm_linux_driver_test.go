package network

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuestVMIP(t *testing.T) {
	slot := &Slot{IP: "10.17.0.2", PrivateCIDR: "10.17.0.0/16"}
	guest, err := GuestVMIP(slot)
	require.NoError(t, err)
	require.Equal(t, "10.17.0.3", guest)

	_, err = GuestVMIP(&Slot{IP: "not-an-ip", PrivateCIDR: "10.17.0.0/16"})
	require.Error(t, err)
	_, err = GuestVMIP(&Slot{IP: "192.168.1.5", PrivateCIDR: "10.17.0.0/16"})
	require.Error(t, err)
	_, err = GuestVMIP(&Slot{IP: "10.17.0.7", PrivateCIDR: "10.17.0.0/29"})
	require.Error(t, err)
	_, err = GuestVMIP(nil)
	require.Error(t, err)
}

func TestGuestVMNetNSDriverPrepare(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{Runner: runner, ResolverPath: "/etc/resolv.conf"})
	slot := &Slot{
		NetNSName: "ns-1", NetNSPath: root + "/netns/ns-1", HostNetNSPath: root + "/host-netns/ns-1",
		HostVeth: "fh-1", PeerVeth: "fp-1", Bridge: "fsb0",
		Address: "10.17.0.2/16", IP: "10.17.0.2", Gateway: "10.17.0.1",
		PrivateCIDR: "10.17.0.0/16", DNSPath: root + "/dns", MTU: 1400,
		GuestTap: "fc-abc123",
	}
	require.NoError(t, driver.Prepare(context.Background(), slot))

	joined := strings.Join(runner.commands, "\n")
	require.Contains(t, joined, "ip tuntap add dev fc-abc123 mode tap")
	require.Contains(t, joined, "ip link set fc-abc123 master fsb0")
	require.Contains(t, joined, "ip link set fc-abc123 mtu 1400")
	require.Contains(t, joined, "ip link set fc-abc123 up")
	// The nat -C check fails in the harness, so the -A fallback runs.
	require.Contains(t, joined, "iptables -t nat -A PREROUTING -d 10.17.0.2/32 -j DNAT --to-destination 10.17.0.3")
	// The FORWARD -C check succeeds in the harness, so no duplicate rules.
	require.NotContains(t, joined, "iptables -A FORWARD")
}

func TestGuestVMNetNSDriverDestroy(t *testing.T) {
	runner := &recordingRunner{}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{Runner: runner, ResolverPath: "/etc/resolv.conf"})
	slot := &Slot{
		NetNSName: "ns-1", HostVeth: "fh-1",
		Address: "10.17.0.2/16", IP: "10.17.0.2", PrivateCIDR: "10.17.0.0/16",
		GuestTap: "fc-abc123",
	}
	require.NoError(t, driver.Destroy(context.Background(), slot))

	joined := strings.Join(runner.commands, "\n")
	require.Contains(t, joined, "ip link del fc-abc123")
	require.Contains(t, joined, "iptables -t nat -D PREROUTING -d 10.17.0.2/32 -j DNAT --to-destination 10.17.0.3")
	require.Contains(t, joined, "ip netns delete ns-1")
}

func TestGuestVMNetNSDriverValidate(t *testing.T) {
	root := t.TempDir()
	netnsPath := root + "/ns-1"
	dnsPath := root + "/resolv.conf"
	require.NoError(t, os.WriteFile(dnsPath, []byte("nameserver 10.96.0.10\n"), 0o644))
	require.NoError(t, os.MkdirAll(netnsPath, 0o755))
	runner := &recordingRunner{}
	driver := NewGuestVMNetNSDriver(LinuxDriverConfig{Runner: runner, ResolverPath: "/etc/resolv.conf"})
	slot := &Slot{
		NetNSName: "ns-1", NetNSPath: netnsPath, HostNetNSPath: root + "/host-ns-1",
		HostVeth: "fh-1", PeerVeth: "fp-1", Bridge: "fsb0",
		Address: "10.17.0.2/16", IP: "10.17.0.2", Gateway: "10.17.0.1",
		PrivateCIDR: "10.17.0.0/16", DNSPath: dnsPath, MTU: 1400,
		GuestTap: "fc-abc123",
	}
	require.NoError(t, driver.Validate(context.Background(), slot))

	slot.GuestTap = ""
	require.Error(t, driver.Validate(context.Background(), slot))
}
