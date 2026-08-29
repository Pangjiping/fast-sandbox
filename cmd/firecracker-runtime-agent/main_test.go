package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fast-sandbox/internal/registryconfig"

	"github.com/stretchr/testify/require"
)

// fakeProvider mocks the registryconfig.Provider interface.
type fakeProvider struct {
	credential registryconfig.Credential
	found      bool
	err        error
	lastRef    string
}

func (f *fakeProvider) Credentials(reference string) (registryconfig.Credential, bool, error) {
	f.lastRef = reference
	return f.credential, f.found, f.err
}

func (f *fakeProvider) Revision() string { return "" }

func TestResolveCredentialMatchesStoreHost(t *testing.T) {
	provider := &fakeProvider{
		credential: registryconfig.Credential{
			Host: "oss-cn-hangzhou.aliyuncs.com", Username: "readonly-ak", Password: "readonly-sk",
		},
		found: true,
	}
	credential, err := resolveCredential(provider, "s3://oss-cn-hangzhou.aliyuncs.com/sandbox-images/publish", "")
	require.NoError(t, err)
	require.Equal(t, "readonly-ak", credential.Username)
	require.Equal(t, "readonly-sk", credential.Password)
	// The provider is matched against the store endpoint host.
	require.Equal(t, "oss-cn-hangzhou.aliyuncs.com", provider.lastRef)
}

func TestResolveCredentialMatchesExplicitEndpoint(t *testing.T) {
	provider := &fakeProvider{
		credential: registryconfig.Credential{
			Host: "127.0.0.1:9000", Username: "chain-test", Password: "chain-test-secret",
			Endpoint: "http://127.0.0.1:9000",
		},
		found: true,
	}
	credential, err := resolveCredential(provider, "s3://sandbox-images/publish", "http://127.0.0.1:9000")
	require.NoError(t, err)
	require.Equal(t, "chain-test", credential.Username)
	require.Equal(t, "http://127.0.0.1:9000", credential.Endpoint)
	require.Equal(t, "127.0.0.1:9000", provider.lastRef)
}

// TestResolveCredentialHostMatchAgainstFile exercises the real compiled
// configuration through FileProvider: a bare endpoint host (no "/") must
// match by host, not by the image-reference rules of Match (which would
// parse "127.0.0.1:9000" as repository "127.0.0.1:9000" under docker.io).
func TestResolveCredentialHostMatchAgainstFile(t *testing.T) {
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: "127.0.0.1:9000", Username: "chain-test", Password: "chain-test-secret",
		Endpoint: "http://127.0.0.1:9000",
	}})
	require.NoError(t, err)
	payload, err := compiled.Marshal()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "registry.json")
	require.NoError(t, os.WriteFile(path, payload, 0o640))

	provider := registryconfig.NewFileProvider(path)
	credential, err := resolveCredential(provider, "s3://sandbox-images/publish", "http://127.0.0.1:9000")
	require.NoError(t, err)
	require.Equal(t, "chain-test", credential.Username)
	require.Equal(t, "chain-test-secret", credential.Password)
	require.Equal(t, "http://127.0.0.1:9000", credential.Endpoint)
}

func TestResolveCredentialInvalidEndpoint(t *testing.T) {
	provider := &fakeProvider{found: true}
	_, err := resolveCredential(provider, "s3://sandbox-images/publish", "://not-a-url")
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpoint")
}

func TestResolveCredentialNoMatch(t *testing.T) {
	provider := &fakeProvider{found: false}
	_, err := resolveCredential(provider, "s3://sandbox-images/publish", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no read-only credential")
}

func TestResolveCredentialProviderError(t *testing.T) {
	provider := &fakeProvider{err: errors.New("config unreadable")}
	_, err := resolveCredential(provider, "s3://sandbox-images/publish", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "config unreadable")
}

func TestResolveCredentialInvalidStoreRoot(t *testing.T) {
	provider := &fakeProvider{found: true}
	_, err := resolveCredential(provider, "not a url ://", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "store root")
}

func TestResolveCredentialMissingHost(t *testing.T) {
	provider := &fakeProvider{found: true}
	_, err := resolveCredential(provider, "s3:///only-path", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpoint host")
}
