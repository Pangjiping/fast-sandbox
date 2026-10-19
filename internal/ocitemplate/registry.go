// Package ocitemplate publishes and resolves fsb-vm template pairs. Only small
// metadata is fetched here; OverlayBD owns the remote layer reads.
package ocitemplate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const ConfigMediaType = "application/vnd.fast-sandbox.template.v1+json"
const Prefix = "oci://"

type Pair struct {
	Rootfs    string `json:"rootfs"`
	Snapfiles string `json:"snapfiles"`
}

type CredentialFunc func(context.Context, string) (auth.Credential, error)

func Repository(reference string, credentials CredentialFunc) (*remote.Repository, registry.Reference, error) {
	ref, err := registry.ParseReference(strings.TrimPrefix(reference, Prefix))
	if err != nil {
		return nil, ref, err
	}
	if ref.Reference == "" {
		return nil, ref, fmt.Errorf("OCI template requires a tag or digest")
	}
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, ref, err
	}
	repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache(), Credential: auth.CredentialFunc(credentials)}
	return repo, ref, nil
}

func Decode(payload []byte) (*Pair, error) {
	var document struct {
		OCI *Pair `json:"oci"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, err
	}
	if document.OCI != nil {
		for _, value := range []string{document.OCI.Rootfs, document.OCI.Snapfiles} {
			ref, err := registry.ParseReference(value)
			if err != nil || !strings.HasPrefix(ref.Reference, "sha256:") {
				return nil, fmt.Errorf("template pair must use immutable sha256 references")
			}
			if err := digest.Digest(ref.Reference).Validate(); err != nil {
				return nil, err
			}
		}
	}
	return document.OCI, nil
}

// Resolve verifies the metadata config and both pair descriptors without
// downloading any layer. Digest references make tag changes harmless.
func Resolve(ctx context.Context, reference string, credentials CredentialFunc) ([]byte, error) {
	repo, ref, err := Repository(reference, credentials)
	if err != nil {
		return nil, err
	}
	desc, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return nil, err
	}
	if desc.MediaType != oci.MediaTypeImageManifest {
		return nil, fmt.Errorf("unexpected template manifest media type %q", desc.MediaType)
	}
	raw, err := fetchSmall(ctx, repo, desc)
	if err != nil {
		return nil, err
	}
	var manifest oci.Manifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	if manifest.Config.MediaType != ConfigMediaType {
		return nil, fmt.Errorf("not a fast-sandbox OCI template")
	}
	payload, err := fetchSmall(ctx, repo, manifest.Config)
	if err != nil {
		return nil, err
	}
	pair, err := Decode(payload)
	if err != nil {
		return nil, err
	}
	if pair == nil {
		return nil, fmt.Errorf("template has no OCI pair")
	}
	for _, value := range []string{pair.Rootfs, pair.Snapfiles} {
		pairRef, e := registry.ParseReference(value)
		if e != nil {
			return nil, e
		}
		if pairRef.Registry != ref.Registry || pairRef.Repository != ref.Repository {
			return nil, fmt.Errorf("template pair must stay in the template repository")
		}
		pairRepo, _, e := Repository(value, credentials)
		if e != nil {
			return nil, e
		}
		if _, e = pairRepo.Resolve(ctx, pairRef.Reference); e != nil {
			return nil, e
		}
	}
	return payload, nil
}

func fetchSmall(ctx context.Context, repo *remote.Repository, desc oci.Descriptor) ([]byte, error) {
	if desc.Size < 0 || desc.Size > 1<<20 {
		return nil, fmt.Errorf("template metadata exceeds 1 MiB")
	}
	return content.FetchAll(ctx, repo, desc)
}

// Push uploads referenced blobs before pair manifests, and commits the template
// metadata manifest last. A failed push never publishes a usable template tag.
func Push(ctx context.Context, dir, destination string, metadata []byte, credentials CredentialFunc) (string, string, error) {
	repo, ref, err := Repository(destination, credentials)
	if err != nil {
		return "", "", err
	}
	if strings.HasPrefix(ref.Reference, "sha256:") {
		return "", "", fmt.Errorf("publish destination must have a tag")
	}
	var index struct {
		Images []localImage `json:"images"`
	}

	raw, err := os.ReadFile(filepath.Join(dir, "manifest/index.json"))
	if err != nil {
		return "", "", err
	}
	if err = json.Unmarshal(raw, &index); err != nil {
		return "", "", err
	}
	if len(index.Images) != 2 {
		return "", "", fmt.Errorf("expected exactly two template images")
	}
	// Incremental exports carry descriptors for inherited remote layers, not
	// local copies. Keep those blobs reachable from the checkpoint repository.
	sources := map[string]string{}
	if rawSources, e := os.ReadFile(filepath.Join(dir, RemoteSourcesName)); e == nil {
		if e = json.Unmarshal(rawSources, &sources); e != nil {
			return "", "", e
		}
	} else if !os.IsNotExist(e) {
		return "", "", e
	}
	pair := Pair{}
	for _, image := range index.Images {
		suffix, immutable, e := pushLocalImage(ctx, repo, dir, ref.Reference, image, sources, credentials)
		if e != nil {
			return "", "", e
		}
		if suffix == "_rootfs" {
			pair.Rootfs = immutable
		} else {
			pair.Snapfiles = immutable
		}
	}

	var doc map[string]any
	if err = json.Unmarshal(metadata, &doc); err != nil {
		return "", "", err
	}
	doc["oci"] = pair
	payload, err := json.Marshal(doc)
	if err != nil {
		return "", "", err
	}
	if _, err = Decode(payload); err != nil {
		return "", "", err
	}
	cfg := content.NewDescriptorFromBytes(ConfigMediaType, payload)
	if err = repo.Blobs().Push(ctx, cfg, bytes.NewReader(payload)); err != nil {
		return "", "", err
	}
	manifest := oci.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: oci.MediaTypeImageManifest, Config: cfg, Layers: []oci.Descriptor{}}
	raw, err = json.Marshal(manifest)
	if err != nil {
		return "", "", err
	}
	desc := content.NewDescriptorFromBytes(oci.MediaTypeImageManifest, raw)
	if err = repo.Manifests().PushReference(ctx, desc, bytes.NewReader(raw), ref.Reference); err != nil {
		return "", "", err
	}
	got, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return "", "", err
	}
	if got.Digest != desc.Digest {
		return "", "", fmt.Errorf("remote template digest mismatch")
	}
	return Prefix + ref.Registry + "/" + ref.Repository + "@" + desc.Digest.String(), desc.Digest.Encoded(), nil
}

// ValidateDigestReference binds a checkpoint's immutable address to its durable
// status digest. Resume never accepts a mutable tag or a mismatched digest.
func ValidateDigestReference(reference, sum string) error {
	ref, err := registry.ParseReference(strings.TrimPrefix(reference, Prefix))
	if err != nil {
		return err
	}
	expected := digest.Digest("sha256:" + sum)
	if err = expected.Validate(); err != nil {
		return err
	}
	if ref.Reference != expected.String() {
		return fmt.Errorf("OCI checkpoint manifest digest does not match checkpoint status")
	}
	return nil
}

// inheritBlob avoids downloading the parent's rootfs to local checkpoint
// staging. Already present blobs need no transfer; registry mounts avoid the
// first cross-repository transfer when supported. ORAS streams on mount fallback.
func inheritBlob(ctx context.Context, destination *remote.Repository, desc oci.Descriptor, source string, credentials CredentialFunc) error {
	exists, err := destination.Blobs().Exists(ctx, desc)
	if err != nil || exists {
		return err
	}
	from, parsed, err := Repository(source+"@"+desc.Digest.String(), credentials)
	if err != nil {
		return err
	}
	getContent := func() (io.ReadCloser, error) { return from.Blobs().Fetch(ctx, desc) }
	if parsed.Registry == destination.Reference.Registry {
		return destination.Mount(ctx, desc, parsed.Repository, getContent)
	}
	stream, err := getContent()
	if err != nil {
		return err
	}
	defer stream.Close()
	return destination.Blobs().Push(ctx, desc, stream)
}

type localImage struct {
	Name   string `json:"name"`
	File   string `json:"manifest_file"`
	Digest string `json:"manifest_digest"`
}

func pushLocalImage(ctx context.Context, repo *remote.Repository, dir, tag string, image localImage, sources map[string]string, credentials CredentialFunc) (string, string, error) {
	if filepath.Base(image.File) != image.File {
		return "", "", fmt.Errorf("invalid manifest file")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest", image.File))
	if err != nil {
		return "", "", err
	}
	if digest.FromBytes(raw).String() != image.Digest {
		return "", "", fmt.Errorf("manifest digest mismatch")
	}
	var manifest oci.Manifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return "", "", err
	}
	for _, desc := range append([]oci.Descriptor{manifest.Config}, manifest.Layers...) {
		if err = pushLocalBlob(ctx, repo, dir, desc, sources, credentials); err != nil {
			return "", "", err
		}
	}

	suffix := ""
	if strings.HasSuffix(image.Name, "_rootfs") {
		suffix = "_rootfs"
	} else if strings.HasSuffix(image.Name, "_snapfiles") {
		suffix = "_snapfiles"
	} else {
		return "", "", fmt.Errorf("unknown pair image %q", image.Name)
	}
	desc := content.NewDescriptorFromBytes(oci.MediaTypeImageManifest, raw)
	if err = repo.Manifests().PushReference(ctx, desc, bytes.NewReader(raw), tag+suffix); err != nil {
		return "", "", err
	}
	immutable := repo.Reference.Registry + "/" + repo.Reference.Repository + "@" + desc.Digest.String()
	return suffix, immutable, nil
}

func pushLocalBlob(ctx context.Context, repo *remote.Repository, dir string, desc oci.Descriptor, sources map[string]string, credentials CredentialFunc) error {
	if desc.Digest.Algorithm() != digest.SHA256 {
		return fmt.Errorf("unsupported blob digest")
	}
	if err := desc.Digest.Validate(); err != nil {
		return err
	}
	file, e := os.Open(filepath.Join(dir, "blobs/sha256", desc.Digest.Encoded()))
	if os.IsNotExist(e) && sources[desc.Digest.String()] != "" {
		if e = inheritBlob(ctx, repo, desc, sources[desc.Digest.String()], credentials); e != nil {
			return e
		}
		return nil
	}
	if e != nil {
		return e
	}
	verifier := desc.Digest.Verifier()
	n, e := io.Copy(verifier, file)
	if e != nil || n != desc.Size || !verifier.Verified() {
		file.Close()
		return fmt.Errorf("blob digest/size mismatch: %s", desc.Digest)
	}
	_, e = file.Seek(0, 0)
	if e == nil {
		var exists bool
		exists, e = repo.Blobs().Exists(ctx, desc)
		if e == nil && !exists {
			e = repo.Blobs().Push(ctx, desc, file)
		}
	}
	file.Close()
	if e != nil {
		return e
	}
	return nil
}
