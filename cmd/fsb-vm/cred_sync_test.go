package main

import (
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOverlaybdCredentialsReplaceRevokedAndRotatedRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cred.json")
	private := dockerConfig{Auths: map[string]authEntry{"registry.example/team": {Username: "old", Password: "old-secret"}}}
	refs := []string{"registry.example/team/template:rootfs", "registry.example/team/template:snapfiles"}
	handoff, err := overlaybdCredentials(private, refs...)
	require.NoError(t, err)
	handoff.Auths["registry.example/other"] = authEntry{Username: "other", Password: "keep"}
	require.NoError(t, mergeCredentials(path, handoff))
	// A projected Secret no longer matching this repository must overwrite its
	// old credential, even if a less-specific credential remains in the file.
	require.NoError(t, mergeCredentials(path, dockerConfig{Auths: map[string]authEntry{"registry.example": {Username: "broad", Password: "stale"}}}))
	handoff, err = overlaybdCredentials(dockerConfig{}, refs...)
	require.NoError(t, err)
	require.NoError(t, mergeCredentials(path, handoff))
	current, err := readCredentials(path)
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(":")), current.Auths["registry.example/team/template"].Auth)
	require.Equal(t, "other", current.Auths["registry.example/other"].Username)
	ref, err := parseRegistryReference(refs[0])
	require.NoError(t, err)
	credential, _, err := credentialFor(current, ref)
	require.NoError(t, err)
	require.Empty(t, credential.Username)
	require.Empty(t, credential.Password)
	handoff, err = overlaybdCredentials(dockerConfig{Auths: map[string]authEntry{"registry.example/team": {Username: "rotated", Password: "new-secret"}}}, refs...)
	require.NoError(t, err)
	require.NoError(t, mergeCredentials(path, handoff))
	current, err = readCredentials(path)
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("rotated:new-secret")), current.Auths["registry.example/team/template"].Auth)
}
