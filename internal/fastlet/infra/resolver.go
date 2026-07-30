package infra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	infracatalog "fast-sandbox/internal/catalog/infra"
)

var ErrArtifactSourceUnsupported = errors.New("Infra artifact source is unsupported")

type ArtifactResolver interface {
	Prepare(context.Context, infracatalog.ArtifactSource, *ArtifactStore) (PreparedSource, error)
}

// OCIArtifactOpener exports the root filesystem of a digest-pinned OCI image
// as an uncompressed tar stream. Implementations must verify that the resolved
// manifest digest equals source.Digest before returning the stream.
type OCIArtifactOpener interface {
	OpenOCI(context.Context, infracatalog.ArtifactSource) (io.ReadCloser, error)
}

type PlatformResolverOptions struct {
	OCI        OCIArtifactOpener
	HTTPClient *http.Client
}

type PlatformResolver struct {
	oci        OCIArtifactOpener
	httpClient *http.Client
}

func NewPlatformResolver(_ []string) *PlatformResolver {
	return NewPlatformResolverWithOptions(PlatformResolverOptions{})
}

func NewPlatformResolverWithOptions(options PlatformResolverOptions) *PlatformResolver {
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &PlatformResolver{oci: options.OCI, httpClient: client}
}

func (r *PlatformResolver) Prepare(
	ctx context.Context,
	source infracatalog.ArtifactSource,
	store *ArtifactStore,
) (PreparedSource, error) {
	if store == nil {
		return PreparedSource{}, errors.New("Infra artifact store is required")
	}
	switch source.Type {
	case infracatalog.SourceArchive:
		if !strings.HasPrefix(source.Reference, "https://") {
			return PreparedSource{}, fmt.Errorf("%w: archive URL must use HTTPS", ErrArtifactSourceUnsupported)
		}
		return store.StageTree(ctx, source.Digest, true, true, func() (io.ReadCloser, error) {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.Reference, nil)
			if err != nil {
				return nil, err
			}
			response, err := r.httpClient.Do(request)
			if err != nil {
				return nil, err
			}
			if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
				_ = response.Body.Close()
				return nil, fmt.Errorf("download Infra archive returned HTTP %d", response.StatusCode)
			}
			return response.Body, nil
		})
	case infracatalog.SourceOCIImage:
		if r.oci == nil {
			return PreparedSource{}, fmt.Errorf("%w: OCI resolver is not configured for %s", ErrArtifactSourceUnsupported, source.Reference)
		}
		return store.StageTree(ctx, source.Digest, false, false, func() (io.ReadCloser, error) {
			return r.oci.OpenOCI(ctx, source)
		})
	default:
		return PreparedSource{}, fmt.Errorf("%w: source type %s", ErrArtifactSourceUnsupported, source.Type)
	}
}
