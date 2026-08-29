package network

import (
	"fmt"
	"net/netip"
)

type IPv4IPAM struct {
	prefix  netip.Prefix
	gateway netip.Addr
	guest   netip.Addr // zero when the baked guest address is not reserved
}

func NewIPv4IPAM(cidr string) (*IPv4IPAM, error) {
	return newIPv4IPAM(cidr, false)
}

// NewGuestVMIPAM is the guest-VM variant of the IPAM: it reserves the baked
// guest address of the clone model (gateway + 2) so a slot IP can never
// collide with the shared guest address every slot netns translates to
// (e.g. with gateway 172.30.0.1 the guest address 172.30.0.3 would
// otherwise be allocated as the second slot's IP, shadowing the guest).
func NewGuestVMIPAM(cidr string) (*IPv4IPAM, error) {
	return newIPv4IPAM(cidr, true)
}

func newIPv4IPAM(cidr string, reserveGuest bool) (*IPv4IPAM, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse private CIDR: %w", err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("private CIDR %q is not IPv4", cidr)
	}
	if prefix.Bits() > 29 {
		return nil, fmt.Errorf("private CIDR %q has no usable sandbox addresses", cidr)
	}
	gateway := prefix.Addr().Next()
	ipam := &IPv4IPAM{prefix: prefix, gateway: gateway}
	if reserveGuest {
		ipam.guest = gateway.Next().Next()
		if !ipam.prefix.Contains(ipam.guest) {
			return nil, fmt.Errorf("private CIDR %q has no guest address after the gateway", cidr)
		}
	}
	return ipam, nil
}

func (i *IPv4IPAM) CIDR() string { return i.prefix.String() }

func (i *IPv4IPAM) Gateway() string { return i.gateway.String() }

func (i *IPv4IPAM) GatewayPrefix() string {
	return netip.PrefixFrom(i.gateway, i.prefix.Bits()).String()
}

func (i *IPv4IPAM) Allocate(used map[string]struct{}) (string, string, error) {
	for address := i.gateway.Next(); i.prefix.Contains(address); address = address.Next() {
		// The final address in an IPv4 prefix is the broadcast address.
		if !i.prefix.Contains(address.Next()) {
			break
		}
		// The baked guest address (clone model) is never a slot IP.
		if i.guest.IsValid() && address == i.guest {
			continue
		}
		if _, exists := used[address.String()]; exists {
			continue
		}
		return address.String(), netip.PrefixFrom(address, i.prefix.Bits()).String(), nil
	}
	return "", "", ErrNoCleanSlot
}
