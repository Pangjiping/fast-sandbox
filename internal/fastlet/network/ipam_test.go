package network

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIPv4IPAMAllocatesUsableAddresses(t *testing.T) {
	ipam, err := NewIPv4IPAM("172.30.0.0/29")
	require.NoError(t, err)
	require.Equal(t, "172.30.0.1", ipam.Gateway())

	used := map[string]struct{}{"172.30.0.2": {}}
	ip, address, err := ipam.Allocate(used)
	require.NoError(t, err)
	require.Equal(t, "172.30.0.3", ip)
	require.Equal(t, "172.30.0.3/29", address)
}

func TestIPv4IPAMRejectsTooSmallPrefix(t *testing.T) {
	_, err := NewIPv4IPAM("172.30.0.0/30")
	require.Error(t, err)
}

func TestGuestVMIPAMReservesBakedGuestAddress(t *testing.T) {
	ipam, err := NewGuestVMIPAM("172.30.0.0/24")
	require.NoError(t, err)
	require.Equal(t, "172.30.0.1", ipam.Gateway())

	// The baked guest address (gateway + 2 = 172.30.0.3) is never a slot
	// IP: a slot owning it would shadow the guest in its netns.
	allocated := make(map[string]struct{})
	var previous string
	for i := 0; i < 6; i++ {
		ip, address, err := ipam.Allocate(allocated)
		require.NoError(t, err)
		require.NotEqual(t, "172.30.0.3", ip, "baked guest address must not be a slot IP")
		require.Equal(t, ip+"/24", address)
		allocated[ip] = struct{}{}
		if previous != "" {
			require.NotEqual(t, previous, ip)
		}
		previous = ip
	}
	require.Contains(t, allocated, "172.30.0.2")
	require.Contains(t, allocated, "172.30.0.4")
	require.NotContains(t, allocated, "172.30.0.3")

	// The non-guest IPAM still allocates the address.
	plain, err := NewIPv4IPAM("172.30.0.0/24")
	require.NoError(t, err)
	ip, _, err := plain.Allocate(nil)
	require.NoError(t, err)
	require.Equal(t, "172.30.0.2", ip)

	// A prefix too small for the reservation is rejected explicitly.
	_, err = NewGuestVMIPAM("172.30.0.0/31")
	require.Error(t, err)
}
