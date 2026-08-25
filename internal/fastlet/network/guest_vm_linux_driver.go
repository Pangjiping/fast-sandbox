package network

import (
	"context"
	"fmt"
	"net/netip"
)

// GuestVMNetNSDriver extends LinuxNetNSDriver with a host-side tap and DNAT
// rules for guest-VM runtimes (Firecracker). The tap lives in the Fastlet
// (firecracker) network namespace and is bridged to the shared bridge; the
// pod-side slot IP is DNATed to the guest address at the host so Ingress
// keeps dialing the pod IP and reply traffic is reverse-NATed by the host
// conntrack. Container runtimes keep using LinuxNetNSDriver and never see
// the tap or the rules.
type GuestVMNetNSDriver struct {
	LinuxNetNSDriver
}

// NewGuestVMNetNSDriver wraps a LinuxNetNSDriver with the guest-VM additions.
func NewGuestVMNetNSDriver(config LinuxDriverConfig) *GuestVMNetNSDriver {
	return &GuestVMNetNSDriver{LinuxNetNSDriver: *NewLinuxNetNSDriver(config)}
}

// Prepare runs the standard Linux preparation, then creates the host-side
// tap on the shared bridge and installs the pod-IP DNAT and forward rules.
func (d *GuestVMNetNSDriver) Prepare(ctx context.Context, slot *Slot) error {
	if err := d.LinuxNetNSDriver.Prepare(ctx, slot); err != nil {
		return err
	}
	guestIP, err := GuestVMIP(slot)
	if err != nil {
		return err
	}
	commands := [][]string{
		{"tuntap", "add", "dev", slot.GuestTap, "mode", "tap"},
		{"link", "set", slot.GuestTap, "master", slot.Bridge},
		{"link", "set", slot.GuestTap, "mtu", fmt.Sprint(slot.MTU)},
		{"link", "set", slot.GuestTap, "up"},
	}
	for _, arguments := range commands {
		if _, err := d.runner.Run(ctx, "ip", arguments...); err != nil {
			return fmt.Errorf("prepare guest tap %s: %w", slot.GuestTap, err)
		}
	}
	rules := [][]string{
		{"-t", "nat", "-A", "PREROUTING", "-d", slot.IP + "/32", "-j", "DNAT", "--to-destination", guestIP},
		{"-A", "FORWARD", "-d", guestIP + "/32", "-j", "ACCEPT"},
		{"-A", "FORWARD", "-s", guestIP + "/32", "-j", "ACCEPT"},
	}
	for _, arguments := range rules {
		check := append([]string(nil), arguments...)
		for index, argument := range check {
			if argument == "-A" {
				check[index] = "-C"
				break
			}
		}
		if _, err := d.runner.Run(ctx, "iptables", check...); err != nil {
			if _, err := d.runner.Run(ctx, "iptables", arguments...); err != nil {
				return fmt.Errorf("prepare guest DNAT rules: %w", err)
			}
		}
	}
	return nil
}

// Validate extends the Linux validation with a guest tap existence check.
func (d *GuestVMNetNSDriver) Validate(ctx context.Context, slot *Slot) error {
	if err := d.LinuxNetNSDriver.Validate(ctx, slot); err != nil {
		return err
	}
	if slot.GuestTap == "" {
		return fmt.Errorf("guest-VM slot has no tap name")
	}
	if _, err := d.runner.Run(ctx, "ip", "link", "show", "dev", slot.GuestTap); err != nil {
		return fmt.Errorf("guest tap %s: %w", slot.GuestTap, err)
	}
	return nil
}

// Destroy removes the guest tap and rules, then runs the Linux destruction.
func (d *GuestVMNetNSDriver) Destroy(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}
	var result error
	if slot.GuestTap != "" {
		if _, err := d.runner.Run(ctx, "ip", "link", "del", slot.GuestTap); err != nil && !isMissingNetworkResource(err) {
			result = fmt.Errorf("delete guest tap %s: %w", slot.GuestTap, err)
		}
	}
	if guestIP, err := GuestVMIP(slot); err == nil {
		rules := [][]string{
			{"-t", "nat", "-D", "PREROUTING", "-d", slot.IP + "/32", "-j", "DNAT", "--to-destination", guestIP},
			{"-D", "FORWARD", "-d", guestIP + "/32", "-j", "ACCEPT"},
			{"-D", "FORWARD", "-s", guestIP + "/32", "-j", "ACCEPT"},
		}
		for _, arguments := range rules {
			_, _ = d.runner.Run(ctx, "iptables", arguments...)
		}
	}
	if err := d.LinuxNetNSDriver.Destroy(ctx, slot); err != nil {
		result = errorsJoin(result, err)
	}
	return result
}

// GuestVMIP returns the guest address directly after the slot IP.
func GuestVMIP(slot *Slot) (string, error) {
	if slot == nil || slot.IP == "" || slot.PrivateCIDR == "" {
		return "", fmt.Errorf("slot IP and private CIDR are required")
	}
	address, err := netip.ParseAddr(slot.IP)
	if err != nil || !address.Is4() {
		return "", fmt.Errorf("invalid slot IP %q", slot.IP)
	}
	prefix, err := netip.ParsePrefix(slot.PrivateCIDR)
	if err != nil || !prefix.Contains(address) {
		return "", fmt.Errorf("slot IP %q is outside private CIDR %q", slot.IP, slot.PrivateCIDR)
	}
	guest := address.Next()
	if !prefix.Contains(guest) {
		return "", fmt.Errorf("no guest address after %q in %q", slot.IP, slot.PrivateCIDR)
	}
	return guest.String(), nil
}

func errorsJoin(left, right error) error {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	return fmt.Errorf("%v; %v", left, right)
}
