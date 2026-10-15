package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// mitmCALineage is one baked certificate's public-key identity in the
// manifest lineage (lineage.egressMitmCA.certificates[]): fingerprints
// only — never the Secret reference, the private key, or certificate
// bytes.
type mitmCALineage struct {
	// CertSHA256 is the SHA-256 of the certificate DER (lowercase hex, the
	// manifest digest convention): the identity of the exact certificate
	// file, matched byte-for-byte against the egress pinned-CA fingerprint.
	CertSHA256 string `json:"certSha256"`
	// SPKISHA256 is the SHA-256 of the SubjectPublicKeyInfo DER: the key
	// identity, stable across re-issuance that keeps the same key.
	SPKISHA256 string `json:"spkiSha256"`
	// NotAfter is the certificate expiry (RFC3339, display-only): a baked
	// CA expiring is a future outage visible in the manifest itself.
	NotAfter string `json:"notAfter"`
}

// injectEgressMITMCertificate copies the mounted MITM CA certificate into
// the guest rootfs at the bootstrap contract path and returns the lineage
// fingerprints of the certificates it baked. The trust installation itself
// stays owned by the execd bootstrap script: it runs at this build's
// snapshot boot, so its rootfs writes and env become part of the golden
// image.
func injectEgressMITMCertificate(spec apiv1alpha2.SandboxTemplateSpec, mountPoint string) ([]mitmCALineage, error) {
	dir := os.Getenv(mitmCertificateDirEnv)
	if dir == "" {
		return nil, errors.New("egressMitmCertificate is set but the build Pod did not mount it: SANDBOX_TEMPLATE_MITM_CERT_DIR is unset")
	}
	payload, err := os.ReadFile(filepath.Join(dir, mitmCertificateItemName))
	if err != nil {
		return nil, fmt.Errorf("read mounted mitm certificate: %w", err)
	}
	certificates, err := parseCACertificates(payload)
	if err != nil {
		return nil, err
	}
	target := filepath.Join(mountPoint, strings.TrimPrefix(guestMITMCertPath, "/"))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(target, payload, 0o644); err != nil { //nolint:gosec // guest CA certificate, world-readable by design
		return nil, err
	}
	if spec.Execd == "" {
		klog.Warning("egressMitmCertificate is set but spec.execd is empty: no bootstrap script is injected, so the guest trust installation will not run")
	}
	return certificateLineage(certificates)
}

// parseCACertificates is the second gate after the controller's key-only
// projection. It rejects empty entries, entries containing a private key,
// and entries without at least one valid PEM CERTIFICATE block, returning
// the certificates in the order they appear. It is deliberately loose
// beyond that: non-CERTIFICATE PEM blocks (CSR, CRL, ...) are skipped and
// trailing non-PEM data is ignored rather than rejected, and the
// certificate is not required to be a CA (BasicConstraints CA:TRUE is
// not verified). The entry is baked verbatim, so Secret operators must
// supply a well-formed entry holding only the CA certificate.
func parseCACertificates(payload []byte) ([]*x509.Certificate, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, errors.New("mounted mitm certificate entry is empty")
	}
	if bytes.Contains(payload, []byte("PRIVATE KEY")) {
		return nil, errors.New("mounted mitm certificate entry contains a private key; only the certificate may be baked")
	}
	var certificates []*x509.Certificate
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
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("mitm certificate entry is not a valid X.509 certificate: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	if len(certificates) == 0 {
		return nil, errors.New("mitm certificate entry holds no PEM CERTIFICATE block")
	}
	return certificates, nil
}

// certificateLineage fingerprints the parsed certificates for the manifest
// lineage: SHA-256 over the certificate DER and over the SubjectPublicKeyInfo
// DER, plus the expiry.
func certificateLineage(certificates []*x509.Certificate) ([]mitmCALineage, error) {
	lineage := make([]mitmCALineage, 0, len(certificates))
	for _, certificate := range certificates {
		spki, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("fingerprint mitm certificate %q: %w", certificate.Subject, err)
		}
		certDigest := sha256.Sum256(certificate.Raw)
		spkiDigest := sha256.Sum256(spki)
		lineage = append(lineage, mitmCALineage{
			CertSHA256: hex.EncodeToString(certDigest[:]),
			SPKISHA256: hex.EncodeToString(spkiDigest[:]),
			NotAfter:   certificate.NotAfter.UTC().Format(time.RFC3339),
		})
	}
	return lineage, nil
}
