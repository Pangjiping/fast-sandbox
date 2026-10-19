# OverlayBD OCI quick start

This guide uses the FastPath gRPC API to create, pause, and resume Sandboxes on an existing Firecracker cluster.

**Enable this path with a Firecracker Pool, an OCI-capable Fastlet image, and an `oci://` template.** Although `output.format` defaults to `overlaybd`, the OCI path described here requires `output.publish: oci://...`.

## 1. Prepare the Pool

Deploy OpenSandbox images that include OCI support, on nodes with KVM and ublk support. Replace `<version>` below with a release that includes this feature.

| Component | Image |
| --- | --- |
| Controller / FastPath | `opensandbox/fsb-controller:release-<version>` |
| Runtime agent | `opensandbox/fsb-firecracker-runtime:release-<version>` |
| Builder | `opensandbox/fsb-sandboxtemplate-builder:release-<version>` |
| Fastlet | `opensandbox/fsb-fastlet:release-<version>-oci` |
| execd | `opensandbox/execd:release-<version>` |

The OCI-capable Fastlet image starts ublkd by default. Use a published OCI variant when available; the tags above illustrate the release naming and do not assert that a particular release has been published. To build and publish your own OCI Fastlet:

```bash
# On a Linux amd64 build host; native tool dependencies are listed in scripts/build-oci-tools.sh.
make images-oci OCI_FASTLET_IMAGE=opensandbox/fsb-fastlet:dev-oci
docker push opensandbox/fsb-fastlet:dev-oci
```

Select that image in the Pool. The ordinary `make images COMPONENT=fastlet` builds the existing non-OCI image. `make images-oci` builds missing native tools automatically; use `make oci-tools` to rebuild them after changing native sources or patches.

Configure these fields on an existing Firecracker Pool, retaining its node settings, agent socket, and storage mounts. This is a configuration fragment; see the [Firecracker sample](../../config/samples/pool-firecracker.yaml) for a complete Pool.

```yaml
spec:
  runtime: firecracker
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: opensandbox/fsb-fastlet:release-<version>-oci
```

The examples below assume a Pool named `firecracker-pool` in namespace `fast-sandbox`, with a per-Sandbox profile of 1 CPU and 512Mi memory.

## 2. Configure registry credentials

For the simplest setup, use one repository for templates and checkpoints, with one account that has pull and push permissions. Set `REGISTRY_USER` and `REGISTRY_PASSWORD`, then run:

```bash
kubectl -n fast-sandbox create secret docker-registry registry-credentials \
  --docker-server=registry.example.com \
  --docker-username="$REGISTRY_USER" \
  --docker-password="$REGISTRY_PASSWORD"
```

Create this ConfigMap so the Pool uses that account to read templates and publish and restore checkpoints:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: fast-sandbox-registry
  namespace: fast-sandbox
data:
  registries.yaml: |
    registries:
      - host: registry.example.com
        repositoryPrefix: sandbox
        secretRef:
          name: registry-credentials
```

Keep the Secret, ConfigMap, Pool, and template in the same namespace. For private source or execd images, also include their credentials in the Builder's `.dockerconfigjson`.

## 3. Create a template

Save this as `template.yaml`, replace the registry address and image version, then apply it:

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxTemplate
metadata:
  name: alpine-oci
  namespace: fast-sandbox
spec:
  image: alpine:3.19
  execd: opensandbox/execd:release-<version>
  kernel: vmlinux.bin
  machine:
    vcpu: "1"
    memory: 512Mi
  readiness:
    probe: tcp://127.0.0.1:44772
    warmupSeconds: 15
  output:
    rootfsSize: 2Gi
    format: overlaybd
    publish: oci://registry.example.com/sandbox/templates:alpine-v1
    publishSecretRef:
      name: registry-credentials
```

```bash
kubectl apply -f template.yaml
kubectl -n fast-sandbox get sandboxtemplate alpine-oci -w
```

Once `status.phase` is `Ready`, retrieve the result:

```bash
TEMPLATE_REF=$(kubectl -n fast-sandbox get sandboxtemplate alpine-oci \
  -o jsonpath='{.status.manifestRef}')
```

`TEMPLATE_REF` should look like `oci://registry.example.com/sandbox/templates@sha256:...`. Use this reference when creating Sandboxes, rather than the source image `alpine:3.19` or the resource name `alpine-oci`.

## 4. Create, pause, and resume through FastPath

Run the examples in Bash from the repository root, with `grpcurl` and `jq` installed. First, forward FastPath in one terminal:

```bash
kubectl -n fast-sandbox-system port-forward svc/fast-sandbox-fastpath 9090:9090
```

In another terminal, retrieve `TEMPLATE_REF` as above, define the helper, and create a Sandbox:

```bash
fp() {
  grpcurl -plaintext -max-time 120 \
    -import-path api/proto/v2 -proto fastpath.proto \
    -d "$2" 127.0.0.1:9090 "fastpath.v2.FastPathService/$1"
}

fp CreateSandbox "{
  \"requestId\": \"oci-create-$(date +%s)-${RANDOM}\",
  \"namespace\": \"fast-sandbox\",
  \"poolRef\": \"firecracker-pool\",
  \"image\": \"$TEMPLATE_REF\",
  \"expiresAtUnixSeconds\": $(( $(date +%s) + 3600 )),
  \"completion\": \"CREATE_COMPLETION_RUNTIME_READY\"
}" > created.json

SANDBOX_REF=$(jq -c '.sandbox.identity |
  {namespacedName: {namespace: .namespace, name: .name}, expectedUid: .uid}' created.json)
```

`RuntimeReady` means the runtime is ready. To wait for aggregate readiness, use `CREATE_COMPLETION_READY` instead.

Pause, then inspect the state:

```bash
fp PauseSandbox "{\"sandbox\": $SANDBOX_REF}"
fp GetSandbox "{\"sandbox\": $SANDBOX_REF}"
```

Pause is asynchronous. **Repeat `GetSandbox` until the runtime state is `RUNTIME_STATE_PAUSED` before resuming.** While pausing or paused, `GetSandbox` returns the controller’s durable state, including during runtime release and assignment cleanup. At that point, the checkpoint is durable and the runtime has been released; assignment cleanup may still be converging.

```bash
CHECKPOINT_ID=$(fp GetSandbox "{\"sandbox\": $SANDBOX_REF}" \
  | jq -r '.sandbox.checkpoint.checkpointId')

fp ResumeSandbox "{
  \"sandbox\": $SANDBOX_REF,
  \"expectedCheckpointId\": \"$CHECKPOINT_ID\"
}"
fp GetSandbox "{\"sandbox\": $SANDBOX_REF}"
```

Wait for the runtime state to return to `RUNTIME_STATE_READY`. Resume restores the same Sandbox; a checkpoint cannot be used as a template to create another Sandbox. The Sandbox expiration time still applies while paused.

Delete the test Sandbox when finished:

```bash
fp DeleteSandbox "{\"sandbox\": $SANDBOX_REF}"
```

## 5. Template and checkpoint destinations

- **Template:** set `SandboxTemplate.spec.output.publish`, including `oci://` and a tag.
- **Checkpoint:** defaults to the repository of the current restore source, which is the template repository on the first pause. The system generates the checkpoint tag.

For a separate checkpoint repository, add this environment variable to the Pool's Fastlet container:

```yaml
env:
- name: FAST_SANDBOX_OCI_SNAPSHOT_REPOSITORY
  value: registry.example.com/sandbox/checkpoints
```

Use `registry/repository` without `oci://`, a tag, or a digest. The account must be able to read both repositories and push to the checkpoint repository. For separate read/write accounts, see [Private registries](private-registries.md) and [Sandbox snapshots](sandbox-snapshots.md).

## Usage notes

- Match the Pool's CPU and memory profile to the template. After updating a template, create instances with its new `status.manifestRef`.
- Add `TEMPLATE_REF` to the Pool's `warmImages` for prewarming. This does not mean all data blocks are cached.
- To measure warm creation latency, fix the template and Fastlet, warm them up, and use a gRPC client that reuses its connection. The `grpcurl` commands above open a new connection for each call and are intended for functional checks.
- See the [FastPath integration guide](pause-resume-snapshot-integration.md) for the full pause/resume state model and concurrency fences.
