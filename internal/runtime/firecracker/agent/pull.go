package agent

// PullImage chain (implementation plan §3): SandboxSpec.Image -> index ->
// manifest -> digest-verified download into
// <StateRoot>/images/<sha256(image)>/.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	"fast-sandbox/internal/registryconfig"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

// Client pulls published Firecracker artifacts for an image reference into
// the node-local cache. Stage 1 carries only the pure pull layer: the UDS
// server, device leasing, and driver wiring arrive with the runtime-agent
// work.
type Client struct {
	s3 *s3Client
}

// NewClient builds a pull client for a store root (s3://bucket/prefix) with
// a read-only access key pair. The credential Host names the store endpoint
// and Username/Password carry the access key pair.
func NewClient(storeRoot string, credential registryconfig.Credential, options ...Option) (*Client, error) {
	config := optionsConfig{}
	for _, option := range options {
		option(&config)
	}
	s3, err := newS3Client(storeRoot, credential, config.httpClient, config.region, config.endpoint)
	if err != nil {
		return nil, err
	}
	return &Client{s3: s3}, nil
}

// Option configures the pull client.
type Option func(*optionsConfig)

type optionsConfig struct {
	region     string
	endpoint   string
	httpClient *http.Client
}

// WithRegion overrides the SigV4 signing region (default us-east-1).
func WithRegion(region string) Option {
	return func(config *optionsConfig) { config.region = region }
}

// WithEndpoint overrides the store endpoint; by default the credential Host
// is used.
func WithEndpoint(endpoint string) Option {
	return func(config *optionsConfig) { config.endpoint = endpoint }
}

// WithHTTPClient replaces the default HTTP client (5-minute request
// timeout), e.g. to tune timeouts or the transport.
func WithHTTPClient(client *http.Client) Option {
	return func(config *optionsConfig) { config.httpClient = client }
}

// PullImage resolves the addressing chain for image and materializes the
// native artifact set in the local cache:
//
//  1. validate the image reference;
//  2. idempotency: a committed cache returns without any network request;
//  3. GET the image reference index (404 -> ErrImageNotReady);
//  4. GET the manifest it points at, verified against index.artifactDigest;
//  5. download each native artifact into the per-build digest namespace,
//     skipping files that already verify (resume) and re-pulling corrupt
//     ones;
//  6. write the local manifest as the commit point.
//
// The call is safe to race with a concurrent PullImage of the same image:
// every write lands in a temporary file and renames into place only after
// full verification, so no half-published state is ever observable.
func (c *Client) PullImage(ctx context.Context, stateRoot, image string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("%w: image reference is required", runtimecontract.ErrInvalidConfig)
	}
	dir := imageDir(stateRoot, image)
	if complete, err := cacheComplete(dir); err != nil {
		return err
	} else if complete {
		return nil
	}
	if err := os.MkdirAll(dir, cacheDirMode); err != nil {
		return fmt.Errorf("prepare image cache: %w", err)
	}

	index, err := fetchIndex(ctx, c.s3, image)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			return fmt.Errorf("%w: %q", runtimecontract.ErrImageNotReady, image)
		}
		return err
	}
	// The index image field must match the requested reference byte for
	// byte: no normalization, no default tags (details doc §1.2).
	if index.Image != image {
		return fmt.Errorf("image index for %q belongs to %q: reference mismatch", image, index.Image)
	}

	manifestKey, err := c.s3.resolveRef(index.ManifestRef)
	if err != nil {
		return err
	}
	payload, err := c.fetchManifest(ctx, manifestKey, index.ArtifactDigest)
	if err != nil {
		return err
	}
	document, err := parseManifest(payload)
	if err != nil {
		return err
	}
	files, err := document.nativeFiles()
	if err != nil {
		return err
	}

	// Artifacts live in the same per-build digest namespace as the
	// manifest, so their store keys derive from the manifest reference.
	buildDir := path.Dir(manifestKey)
	for _, file := range files {
		if err := stageFile(ctx, c.s3, dir, path.Join(buildDir, file.publish), file); err != nil {
			return err
		}
	}
	return commitManifest(dir, payload)
}

// fetchManifest downloads the manifest and verifies its content against the
// index artifactDigest, binding the whole artifact set to the index.
func (c *Client) fetchManifest(ctx context.Context, manifestKey, artifactDigest string) ([]byte, error) {
	body, err := c.s3.get(ctx, manifestKey)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			return nil, fmt.Errorf("published manifest %s not found: %w (index points at an incomplete build)", manifestKey, err)
		}
		return nil, fmt.Errorf("fetch published manifest: %w", err)
	}
	defer body.Close()
	payload, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read published manifest: %w", err)
	}
	if sum := sha256Hex(payload); sum != artifactDigest {
		return nil, fmt.Errorf("published manifest digest mismatch: got %s, want %s", sum, artifactDigest)
	}
	return payload, nil
}
