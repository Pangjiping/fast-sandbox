package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
)

const ociMemoryField = "memory"
const ociGuestInterface = "eth0"

// buildOCITemplate delegates disk conversion, cold boot and snapshots to fsb-vm.
// It shares the existing guest bootstrap contract with native templates.
func buildOCITemplate(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir, kernel string) error {
	if spec.Output.Format != apiv1alpha2.ArtifactFormatOverlayBD {
		return fmt.Errorf("OCI templates require output.format=overlaybd")
	}
	authDir := os.Getenv(publishCredentialsDirEnv)
	authFile := filepath.Join(authDir, ".dockerconfigjson")
	if _, err := os.Stat(authFile); err != nil {
		return fmt.Errorf("OCI publishSecretRef must contain .dockerconfigjson: %w", err)
	}
	dockerDir := filepath.Join(workdir, "docker-auth")
	if err := os.MkdirAll(dockerDir, 0700); err != nil {
		return err
	}
	data, err := os.ReadFile(authFile)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dockerDir, "config.json"), data, 0600); err != nil {
		return err
	}
	if err = os.Setenv("DOCKER_CONFIG", dockerDir); err != nil {
		return err
	}
	sourceDigest, err := pullOCILayout(ctx, spec, workdir)
	if err != nil {
		return err
	}
	inject := filepath.Join(workdir, "inject")
	if err = os.MkdirAll(filepath.Join(inject, "etc"), 0755); err != nil {
		return err
	}
	ca, err := injectRuntime(spec, workdir, inject)
	if err != nil {
		return err
	}
	vcpu, err := resource.ParseQuantity(spec.Machine.VCPU)
	if err != nil {
		return err
	}
	memory, err := resource.ParseQuantity(spec.Machine.Memory)
	if err != nil {
		return err
	}
	sizeQuantity, err := resource.ParseQuantity(spec.Output.RootfsSize)
	if err != nil || sizeQuantity.Sign() <= 0 {
		return fmt.Errorf("rootfsSize must be a positive resource quantity")
	}
	size := artifacts.SizeGiB(sizeQuantity.Value())

	tap := "fsbtpl0"
	if out, e := exec.CommandContext(ctx, "ip", "tuntap", "add", "dev", tap, "mode", "tap").CombinedOutput(); e != nil {
		return fmt.Errorf("create build tap: %w: %s", e, out)
	}
	defer func() { _ = exec.Command("ip", "link", "del", tap).Run() }()
	if out, e := exec.CommandContext(ctx, "ip", "link", "set", tap, "up").CombinedOutput(); e != nil {
		return fmt.Errorf("enable build tap: %w: %s", e, out)
	}
	args := []string{"--firecracker", firecrackerBin, "--overlaybd-convertor", "/usr/local/bin/convertor", "--build-work-dir", filepath.Join(workdir, "builds"), "--build-output-dir", filepath.Join(workdir, "artifacts"), "build", "--source", spec.Image, "--output", "template:build", "--kernel", kernel, "--kernel-version", filepath.Base(kernel), "--vcpu", strconv.FormatInt(vcpu.Value(), 10), "--memory-mb", strconv.FormatInt((memory.Value()+(1<<20)-1)/(1<<20), 10), "--disk-size-gb", strconv.Itoa(size), "--inject-dir", inject, "--init-path", guestInitPath(spec), "--init-script-file", filepath.Join(inject, strings.TrimPrefix(guestInitPath(spec), "/")), "--boot-args", bakedNetworkBootArgs("console=ttyS0 reboot=k panic=1 pci=off nomodules net.ifnames=0 biosdevname=0 random.trust_cpu=on root=/dev/vda rw init=" + guestInitPath(spec)), "--tap-name", tap, "--guest-mac", bakedGuestMAC, "--ready-pattern", "SANDBOX_READY", "--ready-timeout", "3m", "--auth-file", authFile}
	cmd := exec.CommandContext(ctx, "fsb-vm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("fsb-vm build: %w", err)
	}
	identity := artifacts.HostCPUIdentity()
	doc := map[string]any{"schemaVersion": 1, "runtime": "firecracker", "format": "overlaybd", "machine": map[string]any{"vcpu": spec.Machine.VCPU, ociMemoryField: spec.Machine.Memory, "rootfs": fmt.Sprintf("%dGi", size)}, "lineage": map[string]any{"image": spec.Image, "imageDigest": sourceDigest, "execd": spec.Execd, "entrypoint": spec.Entrypoint}, "compatibility": map[string]any{"vendor": identity.Vendor, "cpuFamily": identity.Family, "cpuModel": identity.Model, "cpuStepping": identity.Stepping, "cpuTemplate": "none", "firecrackerVersion": artifacts.FirecrackerVersion(firecrackerBin)}, "guestNetwork": map[string]any{"iface": ociGuestInterface, "mac": bakedGuestMAC, "ip": bakedGuestIP, "gateway": bakedGuestGateway, "netmask": bakedGuestNetmask, "mtu": bakedGuestMTU}, "validation": map[string]any{"booted": true}}
	if len(ca) > 0 {
		doc["lineage"].(map[string]any)["egressMitmCA"] = map[string]any{"certificates": ca}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	metaFile := filepath.Join(workdir, "template-metadata.json")
	if err = os.WriteFile(metaFile, raw, 0600); err != nil {
		return err
	}
	cmd = exec.CommandContext(ctx, "fsb-vm", "push", "--artifact-dir", filepath.Join(workdir, "artifacts", "template_build"), "--destination", strings.TrimPrefix(spec.Output.Publish, "oci://"), "--metadata-file", metaFile, "--auth-file", authFile)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("fsb-vm push: %w", err)
	}
	var result struct {
		ManifestRef    string `json:"manifestRef"`
		ArtifactDigest string `json:"artifactDigest"`
	}
	if err = json.Unmarshal(output, &result); err != nil {
		return err
	}
	if result.ManifestRef == "" || result.ArtifactDigest == "" {
		return fmt.Errorf("fsb-vm push returned incomplete publication")
	}
	return patchPodAnnotations(ctx, result.ManifestRef, result.ArtifactDigest)
}
