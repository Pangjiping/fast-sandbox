package infra

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	infracatalog "fast-sandbox/internal/catalog/infra"
	"fast-sandbox/internal/registryconfig"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

type ContainerdOCIArtifactOpener struct {
	socketPath  string
	snapshotter string
	namespace   string
	registry    registryconfig.Provider
}

func NewContainerdOCIArtifactOpener(
	socketPath string,
	snapshotter string,
	namespace string,
	registry registryconfig.Provider,
) *ContainerdOCIArtifactOpener {
	if socketPath == "" {
		socketPath = "/run/containerd/containerd.sock"
	}
	if snapshotter == "" {
		snapshotter = "overlayfs"
	}
	if namespace == "" {
		namespace = "k8s.io"
	}
	return &ContainerdOCIArtifactOpener{
		socketPath: socketPath, snapshotter: snapshotter, namespace: namespace, registry: registry,
	}
}

func (o *ContainerdOCIArtifactOpener) OpenOCI(
	ctx context.Context,
	source infracatalog.ArtifactSource,
) (io.ReadCloser, error) {
	if source.Type != infracatalog.SourceOCIImage {
		return nil, fmt.Errorf("%w: source %s is not an OCI image", ErrArtifactSourceUnsupported, source.Type)
	}
	client, err := containerd.New(o.socketPath, containerd.WithDefaultNamespace(o.namespace))
	if err != nil {
		return nil, fmt.Errorf("connect containerd for Infra artifact: %w", err)
	}
	cleanupClient := true
	defer func() {
		if cleanupClient {
			_ = client.Close()
		}
	}()

	namespaced := namespaces.WithNamespace(ctx, o.namespace)
	image, err := client.GetImage(namespaced, source.Reference)
	if err != nil {
		options := []containerd.RemoteOpt{
			containerd.WithPullUnpack,
			containerd.WithPullSnapshotter(o.snapshotter),
		}
		if o.registry != nil {
			credential, found, credentialErr := o.registry.Credentials(source.Reference)
			if credentialErr != nil {
				return nil, credentialErr
			}
			if found {
				secret := credential.Password
				if credential.IdentityToken != "" {
					secret = credential.IdentityToken
				}
				options = append(options, containerd.WithResolver(docker.NewResolver(docker.ResolverOptions{
					Credentials: func(string) (string, string, error) {
						return credential.Username, secret, nil
					},
				})))
			}
		}
		image, err = client.Pull(namespaced, source.Reference, options...)
		if err != nil {
			return nil, fmt.Errorf("pull Infra artifact image: %w", err)
		}
	}
	if actual := image.Target().Digest.String(); actual != source.Digest {
		return nil, fmt.Errorf("%w: OCI reference resolved to %s, expected %s", ErrDigestMismatch, actual, source.Digest)
	}
	unpacked, err := image.IsUnpacked(namespaced, o.snapshotter)
	if err != nil {
		return nil, fmt.Errorf("inspect Infra artifact snapshot: %w", err)
	}
	if !unpacked {
		if err := image.Unpack(namespaced, o.snapshotter); err != nil {
			return nil, fmt.Errorf("unpack Infra artifact image: %w", err)
		}
	}

	key, err := temporaryOCIKey(source.Digest)
	if err != nil {
		return nil, err
	}
	container, err := client.NewContainer(
		namespaced,
		key,
		containerd.WithImage(image),
		// Older containerd servers validate that every container object has an
		// OCI spec, even when it only owns a read-only snapshot view and will
		// never create a task. Keep the temporary artifact container portable
		// across those servers by generating the image's default spec.
		containerd.WithNewSpec(),
		containerd.WithSnapshotter(o.snapshotter),
		containerd.WithNewSnapshotView(key, image),
	)
	if err != nil {
		return nil, fmt.Errorf("create Infra artifact snapshot view: %w", err)
	}
	mountRoot, err := os.MkdirTemp("", "fast-sandbox-infra-oci-*")
	if err != nil {
		_ = container.Delete(namespaced, containerd.WithSnapshotCleanup)
		return nil, err
	}
	mounts, err := client.SnapshotService(o.snapshotter).Mounts(namespaced, key)
	if err != nil {
		_ = os.RemoveAll(mountRoot)
		_ = container.Delete(namespaced, containerd.WithSnapshotCleanup)
		return nil, fmt.Errorf("resolve Infra artifact snapshot mounts: %w", err)
	}
	if err := mount.All(mounts, mountRoot); err != nil {
		_ = os.RemoveAll(mountRoot)
		_ = container.Delete(namespaced, containerd.WithSnapshotCleanup)
		return nil, fmt.Errorf("mount Infra artifact snapshot: %w", err)
	}
	emptyRoot, err := os.MkdirTemp("", "fast-sandbox-infra-empty-*")
	if err != nil {
		_ = mount.UnmountAll(mountRoot, 0)
		_ = os.RemoveAll(mountRoot)
		_ = container.Delete(namespaced, containerd.WithSnapshotCleanup)
		return nil, err
	}
	stream := archive.Diff(ctx, emptyRoot, mountRoot)
	cleanupClient = false
	return &cleanupReadCloser{
		ReadCloser: stream,
		cleanup: func() error {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cleanupCtx = namespaces.WithNamespace(cleanupCtx, o.namespace)
			return errors.Join(
				mount.UnmountAll(mountRoot, 0),
				os.RemoveAll(mountRoot),
				os.RemoveAll(emptyRoot),
				container.Delete(cleanupCtx, containerd.WithSnapshotCleanup),
				client.Close(),
			)
		},
	}, nil
}

type cleanupReadCloser struct {
	io.ReadCloser
	once    sync.Once
	cleanup func() error
	err     error
}

func (c *cleanupReadCloser) Close() error {
	c.once.Do(func() {
		c.err = errors.Join(c.ReadCloser.Close(), c.cleanup())
	})
	return c.err
}

func temporaryOCIKey(digest string) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	value := digest
	if len(value) > len("sha256:")+12 {
		value = value[len("sha256:") : len("sha256:")+12]
	}
	return filepath.Base("fast-sandbox-infra-" + value + "-" + hex.EncodeToString(random[:])), nil
}
