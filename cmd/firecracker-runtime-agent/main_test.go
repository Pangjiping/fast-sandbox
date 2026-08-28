package main

import (
	"errors"
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
	credential, err := resolveCredential(provider, "s3://oss-cn-hangzhou.aliyuncs.com/sandbox-images/publish")
	require.NoError(t, err)
	require.Equal(t, "readonly-ak", credential.Username)
	require.Equal(t, "readonly-sk", credential.Password)
	// The provider is matched against the store endpoint host.
	require.Equal(t, "oss-cn-hangzhou.aliyuncs.com", provider.lastRef)
}

func TestResolveCredentialNoMatch(t *testing.T) {
	provider := &fakeProvider{found: false}
	_, err := resolveCredential(provider, "s3://sandbox-images/publish")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no read-only credential")
}

func TestResolveCredentialProviderError(t *testing.T) {
	provider := &fakeProvider{err: errors.New("config unreadable")}
	_, err := resolveCredential(provider, "s3://sandbox-images/publish")
	require.Error(t, err)
	require.Contains(t, err.Error(), "config unreadable")
}

func TestResolveCredentialInvalidStoreRoot(t *testing.T) {
	provider := &fakeProvider{found: true}
	_, err := resolveCredential(provider, "not a url ://")
	require.Error(t, err)
	require.Contains(t, err.Error(), "store root")
}

func TestResolveCredentialMissingHost(t *testing.T) {
	provider := &fakeProvider{found: true}
	_, err := resolveCredential(provider, "s3:///only-path")
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpoint host")
}
