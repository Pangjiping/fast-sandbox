package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// createManifestCachePath scopes immutable manifests to the registry access
// context. Credential rotation cannot reuse another credential's cached metadata;
// only a hash of the scope is persisted, never the credential itself.
func createManifestCachePath(ref registry.Reference, config dockerConfig) (string, error) {
	if !strings.HasPrefix(ref.Reference, "sha256:") {
		return "", nil // mutable tags retain their remote resolution semantics
	}
	expected := digest.Digest(ref.Reference)
	if err := expected.Validate(); err != nil {
		return "", err
	}
	credential, _, err := credentialFor(config, ref)
	if err != nil {
		return "", err
	}
	scope, err := json.Marshal(struct {
		Repository string
		PlainHTTP  bool
		Credential auth.Credential
	}{ref.Registry + "/" + ref.Repository, global.plainHTTP, credential})
	if err != nil {
		return "", err
	}
	return filepath.Join(global.stateDir, ".manifest-cache", digest.FromBytes(scope).Encoded(), expected.Encoded()+".json"), nil
}

func readCachedImageManifest(path, reference string) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil || digest.FromBytes(raw).String() != reference {
		return nil, false
	}
	return raw, true
}

func cacheImageManifest(path, reference string, raw []byte) error {
	// fetchManifestJSON may have selected a platform manifest from an index.
	// Never install that child's bytes under its parent's digest.
	if path == "" || digest.FromBytes(raw).String() != reference {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, raw, 0o600)
}
