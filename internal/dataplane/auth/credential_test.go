package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCredentialRoundTripAndFencing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := NewIssuer(privateKey, 30*time.Second, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := NewVerifier(publicKey, func() time.Time { return now })
	require.NoError(t, err)

	expected := Claims{
		Tenant: "tenant-a", Namespace: "default", SandboxUID: "uid-a",
		TargetKind: TargetKindPort, Protocol: "HTTP", TargetPort: 8080,
		FastletPodUID: "pod-a", AssignmentAttempt: 2, RouteGeneration: 3,
	}
	token, issued, err := issuer.Issue(expected)
	require.NoError(t, err)
	require.Equal(t, now.Add(30*time.Second).Unix(), issued.ExpiresAt)
	require.NotEmpty(t, issued.Nonce)

	actual, err := verifier.VerifyExpected(token, expected)
	require.NoError(t, err)
	require.Equal(t, issued, actual)

	stale := expected
	stale.RouteGeneration++
	_, err = verifier.VerifyExpected(token, stale)
	require.ErrorIs(t, err, ErrClaimMismatch)

	wrongPort := expected
	wrongPort.TargetPort = 8081
	_, err = verifier.VerifyExpected(token, wrongPort)
	require.ErrorIs(t, err, ErrClaimMismatch)
}

func TestCredentialRejectsTamperAndExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := NewIssuer(privateKey, time.Second, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := NewVerifier(publicKey, func() time.Time { return now.Add(2 * time.Second) })
	require.NoError(t, err)
	token, _, err := issuer.Issue(Claims{
		Namespace: "default", SandboxUID: "uid-a", TargetKind: TargetKindPort, Protocol: "HTTP",
		TargetPort: 80, FastletPodUID: "pod-a",
		AssignmentAttempt: 1, RouteGeneration: 1,
	})
	require.NoError(t, err)

	_, err = verifier.Verify(token)
	require.ErrorIs(t, err, ErrExpiredCredential)

	tampered := token[:len(token)-1] + "x"
	_, err = verifier.Verify(tampered)
	require.True(t, errors.Is(err, ErrInvalidCredential) || errors.Is(err, ErrExpiredCredential))
}

func TestVerifierSetSupportsOverlappingKeyRotation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	oldPublicKey, oldPrivateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	newPublicKey, newPrivateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	oldIssuer, err := NewIssuer(oldPrivateKey, 5*time.Minute, func() time.Time { return now })
	require.NoError(t, err)
	newIssuer, err := NewIssuer(newPrivateKey, 5*time.Minute, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := NewVerifierSet([]ed25519.PublicKey{oldPublicKey, newPublicKey}, func() time.Time { return now })
	require.NoError(t, err)
	claims := Claims{
		Namespace: "default", SandboxUID: "uid-a", TargetKind: TargetKindPort, Protocol: "HTTP",
		TargetPort: 8080, FastletPodUID: "pod-a",
		AssignmentAttempt: 1, RouteGeneration: 1,
	}

	oldToken, _, err := oldIssuer.Issue(claims)
	require.NoError(t, err)
	newToken, _, err := newIssuer.Issue(claims)
	require.NoError(t, err)
	_, err = verifier.VerifyExpected(oldToken, claims)
	require.NoError(t, err)
	_, err = verifier.VerifyExpected(newToken, claims)
	require.NoError(t, err)

	encoded := base64.StdEncoding.EncodeToString(oldPublicKey) + "," + base64.RawStdEncoding.EncodeToString(newPublicKey)
	parsed, err := ParsePublicKeySet(encoded)
	require.NoError(t, err)
	require.Equal(t, []ed25519.PublicKey{oldPublicKey, newPublicKey}, parsed)
}

func TestCredentialFencesNamedComponentIdentity(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := NewIssuer(privateKey, time.Minute, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := NewVerifier(publicKey, func() time.Time { return now })
	require.NoError(t, err)
	claims := Claims{
		Namespace: "default", SandboxUID: "uid-a", TargetKind: TargetKindComponent,
		ComponentName: "execd", Protocol: "HTTP", TargetPort: 44772,
		FastletPodUID: "pod-a", AssignmentAttempt: 1, RouteGeneration: 1,
	}
	token, _, err := issuer.Issue(claims)
	require.NoError(t, err)

	wrongComponent := claims
	wrongComponent.ComponentName = "envd"
	_, err = verifier.VerifyExpected(token, wrongComponent)
	require.ErrorIs(t, err, ErrClaimMismatch)

	rawPort := claims
	rawPort.TargetKind = TargetKindPort
	rawPort.ComponentName = ""
	_, err = verifier.VerifyExpected(token, rawPort)
	require.ErrorIs(t, err, ErrClaimMismatch)
}

func TestCredentialFencesFastletPodPort(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := NewIssuer(privateKey, time.Minute, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := NewVerifier(publicKey, func() time.Time { return now })
	require.NoError(t, err)

	claims := Claims{
		Namespace: "tenant-a", SandboxUID: "uid-a", TargetKind: TargetKindFastletPort,
		TargetPort: 9000, FastletPodUID: "pod-a", AssignmentAttempt: 2, RouteGeneration: 4,
	}
	token, _, err := issuer.Issue(claims)
	require.NoError(t, err)

	actual, err := verifier.VerifyFastletPortCredential(token, "pod-a", 9000)
	require.NoError(t, err)
	require.Equal(t, "uid-a", actual.SandboxUID)
	require.Equal(t, "tenant-a", actual.Namespace)

	_, err = verifier.VerifyFastletPortCredential(token, "pod-replaced", 9000)
	require.ErrorIs(t, err, ErrClaimMismatch)

	_, err = verifier.VerifyFastletPortCredential(token, "pod-a", 9001)
	require.ErrorIs(t, err, ErrClaimMismatch)

	// A raw-port credential must not satisfy the Pod Port check.
	portClaims := claims
	portClaims.TargetKind = TargetKindPort
	portClaims.Protocol = "HTTP"
	portToken, _, err := issuer.Issue(portClaims)
	require.NoError(t, err)
	_, err = verifier.VerifyFastletPortCredential(portToken, "pod-a", 9000)
	require.ErrorIs(t, err, ErrClaimMismatch)
}

func TestCredentialRequiresProtocolForPortAndComponentKinds(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	_, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := NewIssuer(privateKey, time.Minute, func() time.Time { return now })
	require.NoError(t, err)

	base := Claims{
		Namespace: "default", SandboxUID: "uid-a", TargetPort: 8080,
		FastletPodUID: "pod-a", AssignmentAttempt: 1, RouteGeneration: 1,
	}

	rawPort := base
	rawPort.TargetKind = TargetKindPort
	_, _, err = issuer.Issue(rawPort)
	require.ErrorIs(t, err, ErrInvalidCredential)

	component := base
	component.TargetKind = TargetKindComponent
	component.ComponentName = "execd"
	_, _, err = issuer.Issue(component)
	require.ErrorIs(t, err, ErrInvalidCredential)

	component.Protocol = "HTTP"
	_, _, err = issuer.Issue(component)
	require.NoError(t, err)

	podPort := base
	podPort.TargetKind = TargetKindFastletPort
	_, _, err = issuer.Issue(podPort)
	require.NoError(t, err)
}
