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

// WithOCICredentials supplies the pool's compiled registry rules to control
// plane metadata reads. Node agents use the mounted live registry provider.
func WithOCIProvider(provider registryconfig.Provider) Option {
	return func(c *optionsConfig) { c.ociProvider = provider }
}
func WithOCICredentials(credentials []registryconfig.Credential) Option {
	return func(c *optionsConfig) { c.ociCredentials = credentials }
}
func RegistryCredentials(provider registryconfig.Provider) ocitemplate.CredentialFunc {
	return func(_ context.Context, reference string) (auth.Credential, error) {
		value, _, err := provider.Credentials(reference)
		return auth.Credential{Username: value.Username, Password: value.Password, RefreshToken: value.IdentityToken}, err
	}
}
func (c *Client) readOCI(ctx context.Context, image string) ([]byte, error) {
	provider := c.ociProvider
	if provider == nil {
		provider = registryconfig.NewFileProvider(registryconfig.MountPath)
	}
	credentials := RegistryCredentials(provider)
	if c.ociCredentials != nil {
		compiled, _ := registryconfig.NewCompiled(c.ociCredentials)
		credentials = func(_ context.Context, reference string) (auth.Credential, error) {
			v, _ := compiled.Match(reference)
			return auth.Credential{Username: v.Username, Password: v.Password, RefreshToken: v.IdentityToken}, nil
		}
	}
	// Auth callbacks receive a host; repository matching is performed once here
	// to preserve prefix-scoped registry rules.
	callback := credentials
	value, err := callback(ctx, image[len(ocitemplate.Prefix):])
	if err != nil {
		return nil, err
	}
	return ocitemplate.Resolve(ctx, image, func(context.Context, string) (auth.Credential, error) { return value, nil })
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
func ReadOCIImage(ctx context.Context, image string, credentials []registryconfig.Credential) ([]byte, error) {
	return (&Client{ociCredentials: credentials}).readOCI(ctx, image)
}
