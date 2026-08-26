package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// stageManifest assembles manifest.json (content-addressed, design schema)
// and SHA256SUMS in the workdir. Checksums are computed once and shared
// between the two outputs via the cache.
func stageManifest(spec apiv1alpha2.SandboxTemplateSpec, sourceDigest, kernel, rootfs, vmstate, memory string, layers []string, workdir string) ([]byte, error) {
	cache := map[string]string{}
	rootfsGiB, err := sizeGiB(spec.Output.RootfsSize)
	if err != nil {
		return nil, err
	}
	manifest, err := buildManifest(spec, sourceDigest, kernel, rootfs, vmstate, memory, layers, cache, rootfsGiB)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(workdir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return nil, err
	}
	if err := writeChecksums(workdir, cache); err != nil {
		return nil, err
	}
	return manifestBytes, nil
}

// buildManifest assembles the content-addressed manifest (design schema).
// rootfsGiB is the actual size passed to oci2rootfs (the declared
// rootfsSize rounded up to SI GiB), recorded so consumers can reconcile the
// declared minimum with the real artifact size.
func buildManifest(spec apiv1alpha2.SandboxTemplateSpec, sourceDigest, kernel, rootfs, vmstate, memory string, layers []string, cache map[string]string, rootfsGiB int) (map[string]any, error) {
	files := map[string]any{}
	artifacts := []struct{ name, path string }{
		{"rootfs.ext4", rootfs},
		{"vmstate.snap", vmstate},
		{"memory.snap", memory},
	}
	if len(layers) > 0 {
		artifacts = append(artifacts,
			struct{ name, path string }{"overlaybd/rootfs/layer.lsmt", layers[0]})
		if len(layers) > 1 {
			artifacts = append(artifacts,
				struct{ name, path string }{"overlaybd/memory/layer.lsmt", layers[1]})
		}
	}
	for _, artifact := range artifacts {
		entry, err := fileEntry(artifact.path, cache)
		if err != nil {
			return nil, fmt.Errorf("checksum %s: %w", artifact.name, err)
		}
		files[artifact.name] = entry
	}
	kernelDigest, err := sha256FileCached(kernel, cache)
	if err != nil {
		return nil, fmt.Errorf("checksum kernel: %w", err)
	}
	return map[string]any{
		"schemaVersion":     1,
		"runtime":           "firecracker",
		"sourceImage":       spec.Image,
		"sourceImageDigest": sourceDigest,
		"execd":             spec.Execd,
		"kernel": map[string]any{
			"name":   filepath.Base(kernel),
			"digest": kernelDigest,
		},
		"machine": map[string]any{
			"vcpu":   spec.Machine.VCPU,
			"memory": spec.Machine.Memory,
		},
		"entrypoint": spec.Entrypoint,
		"init":       spec.Init,
		// Note: envs are published verbatim into the manifest — do not place
		// secrets here; use the publishSecretRef secret for credentials.
		"envs": spec.Envs,
		// The actual rootfs size (rounded up from the declared minimum to SI
		// GiB), matching files['rootfs.ext4'].sizeBytes.
		"rootfsSize": fmt.Sprintf("%dG", rootfsGiB),
		"format":     spec.Output.Format,
		"files":      files,
		"validation": map[string]any{
			"booted":   true,
			"restored": true,
		},
	}, nil
}

// fileEntry describes one artifact: sha256 (sparse-aware) and logical size.
func fileEntry(path string, cache map[string]string) (map[string]any, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	sum, err := sha256FileCached(path, cache)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"sha256":    sum,
		"sizeBytes": info.Size(),
	}, nil
}

// writeChecksums writes SHA256SUMS covering only the published artifact set
// (rootfs/vmstate/memory/layers), reusing the checksums already computed for
// the manifest. Intermediate build files (OCI layout, console logs, etc.) are
// deliberately excluded.
func writeChecksums(workdir string, cache map[string]string) error {
	artifacts := []string{"rootfs.ext4", "vmstate.snap", "memory.snap"}
	layers, err := filepath.Glob(filepath.Join(workdir, "overlaybd", "*", "layer.lsmt"))
	if err != nil {
		return fmt.Errorf("glob overlaybd layers: %w", err)
	}
	for _, layer := range layers {
		relative, err := filepath.Rel(workdir, layer)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, relative)
	}
	var lines []string
	for _, relative := range artifacts {
		path := filepath.Join(workdir, relative)
		sum, err := sha256FileCached(path, cache)
		if err != nil {
			return err
		}
		lines = append(lines, sum+"  "+relative)
	}
	return os.WriteFile(filepath.Join(workdir, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// sha256FileCached returns the checksum of path, memoized in cache.
func sha256FileCached(path string, cache map[string]string) (string, error) {
	if sum, ok := cache[path]; ok {
		return sum, nil
	}
	sum, err := sha256File(path)
	if err != nil {
		return "", err
	}
	cache[path] = sum
	return sum, nil
}
