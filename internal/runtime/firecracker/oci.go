package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"fast-sandbox/internal/ocitemplate"
	"fast-sandbox/internal/registryconfig"
)

func validateOCISupport(image string) error {
	if !strings.HasPrefix(image, ocitemplate.Prefix) {
		return nil
	}
	switch os.Getenv("FAST_SANDBOX_OCI_DISABLED") {
	case "", "false":
		return nil
	case "true":
		return fmt.Errorf("%w: OCI templates and checkpoints are disabled by FAST_SANDBOX_OCI_DISABLED=true", ErrInvalidConfig)
	default:
		return fmt.Errorf("%w: FAST_SANDBOX_OCI_DISABLED must be true or false", ErrInvalidConfig)
	}
}

func ociPair(stateRoot, image string) (*ocitemplate.Pair, error) {
	if !strings.HasPrefix(image, ocitemplate.Prefix) {
		return nil, nil //nolint:nilnil // a nil pair selects the native restore path
	}
	raw, err := os.ReadFile(cachedManifestPath(stateRoot, image))
	if err != nil {
		return nil, err
	}
	pair, err := ocitemplate.Decode(raw)
	if err == nil && pair == nil {
		return nil, fmt.Errorf("OCI image has no pair")
	}
	return pair, err
}
func (d *Driver) fsbArgs() []string {
	return []string{"--state-dir", filepath.Join(d.config.StateRoot, "oci/sandboxes"), "--runtime-dir", filepath.Join(d.config.StateRoot, "oci/runtime"), "--firecracker", d.config.BinaryPath}
}
func (d *Driver) attachOCI(ctx context.Context, id, image string) (rootfs, vmstate, memory string, resultErr error) {
	pair, err := ociPair(d.config.StateRoot, image)
	if err != nil {
		return "", "", "", err
	}
	registryPath := os.Getenv("FAST_SANDBOX_REGISTRY_CONFIG_PATH")
	if registryPath == "" {
		registryPath = registryconfig.MountPath
	}
	credential, _, err := registryconfig.NewFileProvider(registryPath).Credentials(strings.TrimPrefix(image, ocitemplate.Prefix))
	if err != nil {
		return "", "", "", err
	}
	if _, err := ocitemplate.BasicCredential(credential.Username, credential.Password, credential.IdentityToken); err != nil {
		return "", "", "", fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	key := credential.Host
	if credential.RepositoryPrefix != "" {
		key += "/" + strings.Trim(credential.RepositoryPrefix, "/")
	}
	auth, err := json.Marshal(map[string]any{"auths": map[string]any{key: map[string]string{"username": credential.Username, "password": credential.Password}}})
	if err != nil {
		return "", "", "", err
	}
	document := map[string]any{"sandbox_id": id, "template": map[string]any{"rootfs": pair.Rootfs, "snapfiles": pair.Snapfiles, "auth": map[string]string{"dockerAuth": string(auth)}}}
	raw, err := json.Marshal(document)
	if err != nil {
		return "", "", "", err
	}
	dir := filepath.Join(d.config.StateRoot, "oci/inputs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", "", "", err
	}
	file := filepath.Join(dir, id+".json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		return "", "", "", err
	}
	defer os.Remove(file)
	home := filepath.Join(d.config.StateRoot, "oci/sandboxes", id)
	if _, err = os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		return "", "", "", fmt.Errorf("OCI sandbox home already exists or is inaccessible: %s", home)
	}
	// fsb-vm rolls back normal failures. Retry teardown if it retained resources,
	// or if attachment succeeded but reading the resulting state fails.
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, d.releaseOCI(context.Background(), id))
		}
	}()
	args := append(d.fsbArgs(), "create", "--prepare-only", "-f", file)
	output, err := exec.CommandContext(ctx, "fsb-vm", args...).CombinedOutput()
	if err != nil {
		return "", "", "", fmt.Errorf("attach OCI template: %w: %s", err, output)
	}
	var state struct {
		Rootfs struct {
			Path string `json:"dev_path"`
		} `json:"rootfs_device"`
	}
	raw, err = os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		return "", "", "", err
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return "", "", "", err
	}
	if state.Rootfs.Path == "" {
		return "", "", "", fmt.Errorf("OCI attachment state has no rootfs device")
	}
	return state.Rootfs.Path, filepath.Join(home, "mnt/vmstate.bin"), filepath.Join(home, "mnt/memfile"), nil
}
func (d *Driver) releaseOCI(ctx context.Context, id string) error {
	home := filepath.Join(d.config.StateRoot, "oci/sandboxes", id)
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	args := append(d.fsbArgs(), "delete", id)
	output, err := exec.CommandContext(ctx, "fsb-vm", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("release OCI template devices: %w: %s", err, output)
	}
	return nil
}
func (d *Driver) prepareOCIInstance(id, directory, rootfs, vmstate, memory string) (string, string, string, error) {
	if d.config.JailerPath == "" {
		if err := os.Symlink(rootfs, filepath.Join(directory, "rootfs.dev")); err != nil {
			return "", "", "", err
		}
		return rootfs, "", filepath.Join(directory, "api.sock"), nil
	}
	root := jailerRoot(filepath.Join(d.config.StateRoot, jailerChrootBaseDir), filepath.Base(d.config.BinaryPath), truncatedSandboxID(id))
	if err := os.MkdirAll(filepath.Join(root, jailerChrootSnapshotsDir), 0750); err != nil {
		return "", "", "", err
	}
	for _, entry := range []struct{ source, target string }{{rootfs, "rootfs.dev"}, {vmstate, filepath.Join(jailerChrootSnapshotsDir, vmstateSnapshotName)}, {memory, filepath.Join(jailerChrootSnapshotsDir, memorySnapshotName)}} {
		target := filepath.Join(root, entry.target)
		file, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0640)
		if err != nil {
			return "", "", "", err
		}
		file.Close()
		if err = bindMount(entry.source, target); err != nil {
			return "", "", "", err
		}
	}
	d.prepareSnapshotSpill(root)
	return filepath.Join(root, "rootfs.dev"), root, filepath.Join(root, "api.sock"), nil
}
