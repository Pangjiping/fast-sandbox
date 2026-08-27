package agent

// Cache layout (implementation plan §5):
//
//	<StateRoot>/images/<sha256(image)>/
//	├── rootfs.img      # ← published rootfs.ext4
//	├── vmstate.snap
//	├── memory.snap
//	└── manifest.json   # commit point, written last
//
// The imageKey derivation must stay byte-identical to the driver's cache
// key (internal/runtime/firecracker/images.go) so the driver's
// resolveRootfsImage and GC work on the pulled artifacts unchanged.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	imageCacheDir = "images"
	manifestName  = "manifest.json"
	cacheFileMode = 0o640
	cacheDirMode  = 0o750
)

// imageKey derives the content-addressed cache key of an image reference.
func imageKey(image string) string {
	digest := sha256.Sum256([]byte(image))
	return hex.EncodeToString(digest[:])
}

// imageDir returns the cache directory of an image reference.
func imageDir(stateRoot, image string) string {
	return filepath.Join(stateRoot, imageCacheDir, imageKey(image))
}

// cacheComplete reports whether the image directory holds a committed pull:
// the local manifest plus a native file set whose digests match. It is the
// idempotency check: a complete cache is never touched again.
//
// Deliberate trade-off: a committed cache is never refreshed for the same
// image reference, even when the publisher rebuilds it. Picking up a new
// build requires clearing the cache directory or using a new tag. Warm-image
// preheating relies on this "pull once per reference" behavior.
func cacheComplete(dir string) (bool, error) {
	payload, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	document, err := parseManifest(payload)
	if err != nil {
		return false, nil
	}
	files, err := document.nativeFiles()
	if err != nil {
		return false, nil
	}
	for _, file := range files {
		match, err := fileMatches(filepath.Join(dir, file.cache), file)
		if err != nil || !match {
			return false, nil
		}
	}
	return true, nil
}

// stageFile ensures the artifact is in the cache with the expected size and
// digest: an existing matching file is skipped (resume semantics), a
// mismatching file is deleted and re-pulled, and the download lands in a
// temporary file that is renamed into place only after the whole content
// verifies.
func stageFile(ctx context.Context, s3 *s3Client, dir, storeKey string, file nativeFile) error {
	target := filepath.Join(dir, file.cache)
	match, err := fileMatches(target, file)
	if err == nil && match {
		return nil
	}
	if err == nil {
		// Corrupt cache entry: drop it before re-pulling. The local
		// manifest is left alone; the commit-point check decides whether
		// the whole pull needs redoing.
		if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	body, err := s3.get(ctx, storeKey)
	if err != nil {
		return err
	}
	defer body.Close()

	tmp, err := os.CreateTemp(dir, file.cache+".tmp-*")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmp.Name())
		}
	}()
	digest := sha256.New()
	written, err := io.Copy(tmp, io.TeeReader(body, digest))
	if err != nil {
		return fmt.Errorf("download %s: %w", file.publish, err)
	}
	if written != file.sizeBytes {
		return fmt.Errorf("artifact %s size mismatch: got %d bytes, want %d", file.publish, written, file.sizeBytes)
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	if sum != file.sha256 {
		return fmt.Errorf("artifact %s digest mismatch: got %s, want %s", file.publish, sum, file.sha256)
	}
	if err := tmp.Chmod(cacheFileMode); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	keep = true
	return nil
}

// commitManifest writes the downloaded manifest as the pull commit point:
// its presence with matching files means the pull is complete. The write is
// atomic so a crash never leaves a half-written manifest.
func commitManifest(dir string, payload []byte) error {
	tmp, err := os.CreateTemp(dir, manifestName+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if _, err := tmp.Write(payload); err != nil {
		return err
	}
	if err := tmp.Chmod(cacheFileMode); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, manifestName))
}

// fileMatches reports whether the file exists with the expected size and
// sha256. The size is compared first via stat: hashing a multi-GiB rootfs
// on every idempotency check would defeat the point of the warm cache, so
// the full digest is only computed when the size matches.
func fileMatches(path string, file nativeFile) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Size() != file.sizeBytes {
		return false, nil
	}
	input, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer input.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, input); err != nil {
		return false, err
	}
	return hex.EncodeToString(digest.Sum(nil)) == file.sha256, nil
}
