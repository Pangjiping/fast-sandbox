package firecracker

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildBootArgsAppendsGuestNetwork(t *testing.T) {
	base := "console=ttyS0 reboot=k panic=1 pci=off"
	args := buildBootArgs(base, "172.30.0.3", "172.30.0.1", "255.255.255.0")
	require.Equal(t, "console=ttyS0 reboot=k panic=1 pci=off ip=172.30.0.3::172.30.0.1:255.255.255.0::eth0:off", args)
}

func TestBuildBootArgsPreservesExistingIPArg(t *testing.T) {
	args := buildBootArgs("console=ttyS0 ip=10.0.0.1::10.0.0.254:255.255.255.0::eth0:off", "172.30.0.3", "172.30.0.1", "255.255.255.0")
	require.Equal(t, "console=ttyS0 ip=10.0.0.1::10.0.0.254:255.255.255.0::eth0:off", args)
}

func TestPrefixMask(t *testing.T) {
	mask, err := prefixMask(16)
	require.NoError(t, err)
	require.Equal(t, "255.255.0.0", mask)

	_, err = prefixMask(33)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestGuestMACIsDeterministic(t *testing.T) {
	require.Equal(t, guestMAC("sandbox-1"), guestMAC("sandbox-1"))
	require.NotEqual(t, guestMAC("sandbox-1"), guestMAC("sandbox-2"))
	mac := guestMAC("sandbox-1")
	require.True(t, len(mac) == 17 && mac[:3] == "02:")
}
