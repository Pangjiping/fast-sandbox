package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// mitmCertificateItemName is the projected file name inside the
// controller-owned mitm-certificate mount.
const mitmCertificateItemName = "tls.crt"

// guestMITMCertPath is the execd bootstrap contract path the certificate
// must land at for the runtime trust flow to fire.
const guestMITMCertPath = "/opt/opensandbox/mitmproxy-ca-cert.pem"

// guestMITMTrustGate enables the bootstrap's MITM trust installation
// (system store, merged bundle, NSS, JDK keystores, client env vars).
const guestMITMTrustGate = "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT"

// injectEgressMITMCertificate copies the mounted MITM CA certificate into
// the guest rootfs at the bootstrap contract path. The trust installation
// itself stays owned by the execd bootstrap script: it runs at this build's
// snapshot boot, so its rootfs writes and env become part of the golden
// image.
func injectEgressMITMCertificate(spec apiv1alpha2.SandboxTemplateSpec, mountPoint string) error {
	dir := os.Getenv(mitmCertificateDirEnv)
	if dir == "" {
		return errors.New("egressMitmCertificate is set but the build Pod did not mount it: SANDBOX_TEMPLATE_MITM_CERT_DIR is unset")
	}
	payload, err := os.ReadFile(filepath.Join(dir, mitmCertificateItemName))
	if err != nil {
		return fmt.Errorf("read mounted mitm certificate: %w", err)
	}
	if err := validateCACertificate(payload); err != nil {
		return err
	}
	target := filepath.Join(mountPoint, strings.TrimPrefix(guestMITMCertPath, "/"))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, payload, 0o644); err != nil { //nolint:gosec // guest CA certificate, world-readable by design
		return err
	}
	if spec.Execd == "" {
		klog.Warning("egressMitmCertificate is set but spec.execd is empty: no bootstrap script is injected, so the guest trust installation will not run")
	}
	return nil
}

// validateCACertificate accepts only a PEM X.509 certificate — the second
// gate after the controller's key-only projection.
func validateCACertificate(payload []byte) error {
	if len(bytes.TrimSpace(payload)) == 0 {
		return errors.New("mounted mitm certificate entry is empty")
	}
	if bytes.Contains(payload, []byte("PRIVATE KEY")) {
		return errors.New("mounted mitm certificate entry contains a private key; only the certificate may be baked")
	}
	found := false
	rest := payload
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("mitm certificate entry is not a valid X.509 certificate: %w", err)
		}
		found = true
	}
	if !found {
		return errors.New("mitm certificate entry holds no PEM CERTIFICATE block")
	}
	return nil
}
