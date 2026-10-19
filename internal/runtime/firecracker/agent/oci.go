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

// WithOCIProvider supplies the node's live registry credentials.
func WithOCIProvider(provider registryconfig.Provider) Option {
	return func(c *optionsConfig) { c.ociProvider = provider }
}
func (c *Client) readOCI(ctx context.Context, image string) ([]byte, error) {
	provider := c.ociProvider
	if provider == nil {
		provider = registryconfig.NewFileProvider(registryconfig.MountPath)
	}
	return ocitemplate.Resolve(ctx, image, func(_ context.Context, reference string) (auth.Credential, error) {
		value, _, err := provider.Credentials(reference)
		return auth.Credential{Username: value.Username, Password: value.Password, RefreshToken: value.IdentityToken}, err
	})
}

var ociPullLocks [256]sync.Mutex

func (c *Client) pullOCI(ctx context.Context, stateRoot, image string) error {
	dir := imageDir(stateRoot, image)
	lock := &ociPullLocks[sha256.Sum256([]byte(dir))[0]]
	lock.Lock()
	defer lock.Unlock()
	if ready, err := cacheComplete(dir); ready || err != nil {
		return err
	}
	data, err := c.readOCI(ctx, image)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, cacheDirMode); err != nil {
		return err
	}
	return commitManifest(dir, data)
}

// PullOCIImage is independent of the optional native object-store configuration.
func PullOCIImage(ctx context.Context, stateRoot, image string, provider registryconfig.Provider) error {
	return (&Client{ociProvider: provider}).pullOCI(ctx, stateRoot, image)
}
