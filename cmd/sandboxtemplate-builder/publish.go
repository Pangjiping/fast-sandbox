package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// publish uploads the artifacts under a digest namespace and returns the
// manifest URI. The manifest is uploaded last so consumers never observe a
// half-published artifact set. OverlayBD layers keep their relative paths
// (overlaybd/rootfs/layer.lsmt, overlaybd/memory/layer.lsmt) — a flat
// basename would collide on the same S3 key.
func publish(ctx context.Context, spec apiv1alpha2.SandboxTemplateSpec, workdir string, manifestBytes []byte) (string, error) {
	aws, err := exec.LookPath(awsBin)
	if err != nil {
		return "", fmt.Errorf("aws CLI not found: %w", err)
	}
	base := strings.TrimRight(spec.Output.Publish, "/") + "/" + sha256Of(manifestBytes)[:16]
	entries := []struct{ local, key string }{
		{filepath.Join(workdir, "rootfs.ext4"), "rootfs.ext4"},
		{filepath.Join(workdir, "vmstate.snap"), "vmstate.snap"},
		{filepath.Join(workdir, "memory.snap"), "memory.snap"},
	}
	if layers, err := filepath.Glob(filepath.Join(workdir, "overlaybd", "*", "layer.lsmt")); err == nil {
		for _, layer := range layers {
			relative, err := filepath.Rel(workdir, layer)
			if err != nil {
				return "", err
			}
			entries = append(entries, struct{ local, key string }{layer, relative})
		}
	}
	// --endpoint-url pins the S3-compatible target even with awscli v1
	// (AWS_ENDPOINT_URL is only honored by botocore >=1.29.16 / CLI v2).
	args := []string{"s3", "cp"}
	if endpoint := os.Getenv("AWS_ENDPOINT_URL"); endpoint != "" {
		args = append(args, "--endpoint-url", endpoint)
	}
	for _, entry := range entries {
		if err := uploadWithRetry(ctx, aws, args, entry.local, base+"/"+entry.key, entry.key); err != nil {
			return "", err
		}
	}
	manifestURI := base + "/manifest.json"
	if err := uploadWithRetry(ctx, aws, args, filepath.Join(workdir, "manifest.json"), manifestURI, "manifest.json"); err != nil {
		return "", err
	}
	return manifestURI, nil
}

// publishRetries is how many times a transient upload failure is retried.
const publishRetries = 3

// uploadWithRetry uploads one object, retrying transient failures with
// exponential backoff so a flaky network does not fail the whole build.
func uploadWithRetry(ctx context.Context, aws string, args []string, local, target, name string) error {
	backoff := 2 * time.Second
	var lastErr error
	for attempt := 0; attempt <= publishRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff *= 2
		}
		command := exec.CommandContext(ctx, aws, append(args, local, target)...)
		if output, err := command.CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("publish %s: %w: %s", name, err, output)
			klog.V(2).InfoS("publish attempt failed, retrying", "object", name, "attempt", attempt, "err", err)
			continue
		}
		return nil
	}
	return lastErr
}

// patchPodAnnotations records the build outcome on the builder Pod so the
// controller can surface it on the template status. It uses a merge Patch
// touching only the two annotations to avoid racing kubelet's own status
// updates. Standalone runs (E2E, local debugging) simply skip the report.
func patchPodAnnotations(ctx context.Context, manifestRef, digest string) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.V(2).InfoS("skipping pod annotation update (not running in a cluster)", "err", err)
		return nil
	}
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = serviceAccountNamespace()
	}
	name := os.Getenv("POD_NAME")
	if name == "" {
		return errors.New("POD_NAME is required to report build results")
	}
	refJSON, err := json.Marshal(manifestRef)
	if err != nil {
		return err
	}
	digestJSON, err := json.Marshal(digest)
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":%s,"%s":%s}}}`,
		manifestRefAnnotation, refJSON, digestAnnotation, digestJSON)
	_, err = clientSet.CoreV1().Pods(namespace).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

func serviceAccountNamespace() string {
	payload, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "default"
	}
	return string(payload)
}
