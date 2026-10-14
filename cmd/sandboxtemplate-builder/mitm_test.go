package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// testCACertificatePEM generates a self-signed CA certificate with real
// DER, so the X.509 parser is actually exercised.
func testCACertificatePEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "opensandbox-mitm-ca.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func stageMitmCertificate(t *testing.T, payload []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, mitmCertificateItemName), payload, 0o600); err != nil {
		t.Fatalf("stage mounted mitm certificate: %v", err)
	}
	t.Setenv(mitmCertificateDirEnv, dir)
	return dir
}

// TestValidateCACertificate: only a PEM X.509 certificate passes; a private
// key — alone or concatenated with the certificate — never does.
func TestValidateCACertificate(t *testing.T) {
	valid := testCACertificatePEM(t)
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not really der")})
	tests := []struct {
		name      string
		payload   []byte
		wantError string
	}{
		{name: "accepts a PEM X.509 certificate", payload: valid},
		{
			name:      "rejects an empty entry",
			payload:   []byte("  \n"),
			wantError: "empty",
		},
		{
			name:      "rejects a private key",
			payload:   privateKey,
			wantError: "private key",
		},
		{
			name:      "rejects a certificate shipped with its key",
			payload:   append(append([]byte{}, valid...), privateKey...),
			wantError: "private key",
		},
		{
			name:      "rejects a non-certificate PEM block",
			payload:   pem.EncodeToMemory(&pem.Block{Type: "DATA", Bytes: []byte("junk")}),
			wantError: "no PEM CERTIFICATE block",
		},
		{
			name:      "rejects non-PEM payload",
			payload:   []byte("hello"),
			wantError: "no PEM CERTIFICATE block",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCACertificate(test.payload)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("validateCACertificate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validateCACertificate error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

// TestInjectEgressMITMCertificate: the mounted certificate lands verbatim at
// the execd bootstrap contract path; a missing mount env or an invalid entry
// fails the build.
func TestInjectEgressMITMCertificate(t *testing.T) {
	certPEM := testCACertificatePEM(t)

	t.Run("bakes the certificate at the bootstrap contract path", func(t *testing.T) {
		stageMitmCertificate(t, certPEM)
		rootfs := t.TempDir()
		if err := injectEgressMITMCertificate(apiv1alpha2.SandboxTemplateSpec{Execd: "opensandbox/execd:1.1.0"}, rootfs); err != nil {
			t.Fatalf("injectEgressMITMCertificate: %v", err)
		}
		payload, err := os.ReadFile(filepath.Join(rootfs, strings.TrimPrefix(guestMITMCertPath, "/")))
		if err != nil {
			t.Fatalf("read baked certificate: %v", err)
		}
		if string(payload) != string(certPEM) {
			t.Fatalf("baked certificate differs from the mounted entry")
		}
	})

	t.Run("fails without the controller mount env", func(t *testing.T) {
		t.Setenv(mitmCertificateDirEnv, "")
		err := injectEgressMITMCertificate(apiv1alpha2.SandboxTemplateSpec{}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "SANDBOX_TEMPLATE_MITM_CERT_DIR is unset") {
			t.Fatalf("injectEgressMITMCertificate error = %v, want missing-mount error", err)
		}
	})

	t.Run("fails on a private key entry", func(t *testing.T) {
		stageMitmCertificate(t, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}))
		err := injectEgressMITMCertificate(apiv1alpha2.SandboxTemplateSpec{}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "private key") {
			t.Fatalf("injectEgressMITMCertificate error = %v, want private-key rejection", err)
		}
	})
}

// TestInjectRuntimeBakesMitmCertificateAndGate: the gate line follows the
// merged envs, so it wins over a user-provided value in spec.envs.
func TestInjectRuntimeBakesMitmCertificateAndGate(t *testing.T) {
	stageMitmCertificate(t, testCACertificatePEM(t))
	workdir, rootfs := t.TempDir(), t.TempDir()
	// injectRuntime assumes a real-rootfs shape: etc must exist.
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("stage etc: %v", err)
	}
	spec := apiv1alpha2.SandboxTemplateSpec{
		Execd: "opensandbox/execd:1.1.0",
		Envs:  []corev1.EnvVar{{Name: guestMITMTrustGate, Value: "false"}},
		EgressMITMCertificate: &apiv1alpha2.MITMCertificateSpec{
			SecretRef: corev1.LocalObjectReference{Name: "mitm-ca"},
			Key:       "ca.crt",
		},
	}
	if err := injectRuntime(spec, workdir, rootfs); err != nil {
		t.Fatalf("injectRuntime: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(rootfs, strings.TrimPrefix(guestMITMCertPath, "/")))
	if err != nil {
		t.Fatalf("baked certificate missing: %v", err)
	}
	if len(payload) == 0 {
		t.Fatalf("baked certificate is empty")
	}
	envPayload, err := os.ReadFile(filepath.Join(rootfs, "etc", "sandbox-init.env"))
	if err != nil {
		t.Fatalf("read sandbox-init.env: %v", err)
	}
	envContent := string(envPayload)
	userLine := "export " + guestMITMTrustGate + "='false'"
	gateLine := "export " + guestMITMTrustGate + "=true"
	userIdx := strings.Index(envContent, userLine)
	gateIdx := strings.LastIndex(envContent, gateLine)
	if userIdx == -1 || gateIdx == -1 || gateIdx < userIdx {
		t.Fatalf("sandbox-init.env must carry the gate after the user env (user at %d, gate at %d):\n%s",
			userIdx, gateIdx, envContent)
	}
}
