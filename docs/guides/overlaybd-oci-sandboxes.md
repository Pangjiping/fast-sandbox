# OverlayBD OCI quick start

This guide uses the FastPath gRPC API to create, pause, and resume Sandboxes on an existing Firecracker cluster.

**Enable this path with a Firecracker Pool, an OCI-capable Fastlet image, and an `oci://` template.** `output.format: overlaybd` requires `output.publish: oci://...`; OverlayBD + S3 is unsupported. To retain S3-compatible storage, explicitly use `output.format: native` and rebuild the template. Old OverlayBD + S3 artifacts, including cached manifests, are rejected.

## 1. Prepare the Pool

Deploy OpenSandbox images that include OCI support, on nodes with KVM and ublk support. Replace `<version>` below with a release that includes this feature.

| Component | Image |
| --- | --- |
| Controller / FastPath | `opensandbox/fsb-controller:release-<version>` |
| Runtime agent | `opensandbox/fsb-firecracker-runtime:release-<version>` |
| Builder | `opensandbox/fsb-sandboxtemplate-builder:release-<version>` |
| Fastlet | `opensandbox/fsb-fastlet:release-<version>` |
| execd | `opensandbox/execd:release-<version>` |

The standard Fastlet image includes the OCI runtime tools. OCI support is enabled by default (`FAST_SANDBOX_OCI_DISABLED=false`). The entrypoint starts ublkd, waits for its readiness probe, and supervises it alongside Fastlet; startup failure or an unexpected daemon exit terminates the container. Fastlet selects OCI or native storage from the template reference. Set `FAST_SANDBOX_OCI_DISABLED=true` in the Pool Fastlet environment when OCI is unnecessary or the host lacks ublk; this skips daemon startup and explicitly rejects OCI template and checkpoint requests, including cached ones. Remove the former opt-in environment variable when upgrading; it no longer controls startup. The tags above illustrate the release naming and do not assert that a particular release has been published. To build and publish your own Fastlet:

```bash
# All Go and native-tool compilation runs inside Docker.
make images COMPONENT=fastlet FASTLET_IMAGE=opensandbox/fsb-fastlet:dev
docker push opensandbox/fsb-fastlet:dev
```

When OCI is disabled, OCI creation is rejected with an explicit error. Resume is asynchronous: the rejection is reported in the Sandbox `status.runtime.message` and `Ready=False` condition; `GetSandbox` may report `Unavailable` while no runtime is assigned.

Select that image in the Pool. `build/Dockerfile.fastlet` compiles the runtime tools inside Docker, using the same OverlayBD build script as the Builder. Docker manages rebuilds when sources or patches change; no host C/C++ toolchain or prebuilt `.build/` artifacts are required for the Fastlet image.

Configure these fields on an existing Firecracker Pool, retaining its node settings, agent socket, and storage mounts. This is a configuration fragment; see the [Firecracker sample](../../config/samples/pool-firecracker.yaml) for a complete Pool.

```yaml
spec:
  runtime: firecracker
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: opensandbox/fsb-fastlet:release-<version>
        env:
        - name: FAST_SANDBOX_OCI_DISABLED
          value: "false"
```

The examples below assume a Pool named `firecracker-pool` in namespace `fast-sandbox`, with a per-Sandbox profile of 1 CPU and 512Mi memory.

## 2. Configure registry credentials

OCI registry access supports anonymous or username/password authentication. Docker config `auth` entries are supported; identity-token-only credentials are rejected. A token supplied as a username/password credential is treated as a password. Existing public credential fields and other runtimes are unchanged.

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

OCI templates require `output.publishSecretRef` with a non-empty Secret name and a `.dockerconfigjson` key. Missing references are rejected before a build Pod is created. The builder preserves an existing `/etc/hosts` in the source image and only supplies defaults when it is absent.

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

### Registry artifact format

New publications contain exactly two image tags, following fsb-vm's existing naming rule:

```text
repository:<id>_rootfs
repository:<id>_snapfiles
```

No third metadata image is published. Both images describe their own layers in their OCI manifests. The snapfiles filesystem retains `vmstate.bin`, `memfile`, and `metadata.json`. Its OCI manifest carries these annotations:

- `io.fast-sandbox.template.id`: the shared tag prefix used to find `<id>_rootfs` in the same repository.
- `io.fast-sandbox.metadata`: the small platform metadata JSON (machine, compatibility, guest network, policy and lineage), available before mounting snapshot data.
- For checkpoints, `io.fast-sandbox.sandbox.uid`, `io.fast-sandbox.sandbox.namespace`, and `io.fast-sandbox.checkpoint.id`: owner and runtime snapshot ID. The latter is the runtime snapshot UUID used as the tag prefix; the control-plane checkpoint task ID is unchanged.

The publisher uploads rootfs first and snapfiles last. It rejects replacing an existing tag with different content; configure registry tag immutability as well, since the two images are paired by ID rather than a third manifest containing both digests. Use a fresh ID for every new checkpoint. A retry with identical contents is allowed.

`manifestRef` and `artifactDigest` identify the snapfiles manifest. On first resolution, the runtime resolves `<id>_rootfs` and caches the pair as digest references. A logical `oci://repository:<id>` input also resolves the pair. Previously published three-image metadata entries remain readable, but new publications only write the two-image format. Existing publications are not rewritten or deleted. Upgrade the Controller/FastPath and runtime-agent readers before enabling new publications from upgraded Fastlets or Builders. Older readers do not understand the new snapfiles entry format.

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
