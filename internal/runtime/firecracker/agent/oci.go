package agent

import (
	"context"
	"crypto/sha256"
	"os"
	"sync"

	"oras.land/oras-go/v2/registry/remote/auth"

	"fast-sandbox/internal/ocitemplate"
	"fast-sandbox/internal/registryconfig"
)

func readOCI(ctx context.Context, image string, provider registryconfig.Provider) ([]byte, error) {
	if provider == nil {
		provider = registryconfig.NewFileProvider(registryconfig.MountPath)
	}
	return ocitemplate.Resolve(ctx, image, func(_ context.Context, reference string) (auth.Credential, error) {
		value, _, err := provider.Credentials(reference)
		return auth.Credential{Username: value.Username, Password: value.Password, RefreshToken: value.IdentityToken}, err
	})
}

var ociPullLocks [256]sync.Mutex

// PullOCIImage pulls OCI metadata independently of the native object-store client.
func PullOCIImage(ctx context.Context, stateRoot, image string, provider registryconfig.Provider) error {
	dir := imageDir(stateRoot, image)
	lock := &ociPullLocks[sha256.Sum256([]byte(dir))[0]]
	lock.Lock()
	defer lock.Unlock()
	if ready, err := cacheComplete(dir); ready || err != nil {
		return err
	}
	data, err := readOCI(ctx, image, provider)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, cacheDirMode); err != nil {
		return err
	}
	return commitManifest(dir, data)
}
