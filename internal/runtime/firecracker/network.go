package firecracker

import (
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
)

// guestInterface is the virtio-net interface name configured inside the guest
// through the kernel boot argument ip= parameter.
const guestInterface = "eth0"

// buildBootArgs appends a static guest network configuration to the base
// kernel command line. The guest owns its address inside the private CIDR;
// the pod-side IP remains on the slot netns and is DNATed to the guest by
// the pre-provisioned GuestVMNetNSDriver.
func buildBootArgs(base string, guestIP, gateway, prefixMask string) string {
	if !strings.Contains(base, " ip=") && !strings.HasSuffix(base, " ip=") {
		return fmt.Sprintf("%s ip=%s::%s:%s::%s:off", base, guestIP, gateway, prefixMask, guestInterface)
	}
	return base
}

// prefixMask converts a CIDR prefix length to a dotted IPv4 mask.
func prefixMask(prefixBits int) (string, error) {
	if prefixBits < 0 || prefixBits > 32 {
		return "", fmt.Errorf("%w: invalid prefix length %d", ErrInvalidConfig, prefixBits)
	}
	return net.IP(net.CIDRMask(prefixBits, 32)).String(), nil
}

// guestMAC derives a stable, locally administered MAC from the Sandbox id.
func guestMAC(sandboxID string) string {
	digest := sha256.Sum256([]byte(sandboxID))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", digest[0], digest[1], digest[2], digest[3], digest[4])
}
