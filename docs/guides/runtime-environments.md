# Runtime environments

Runtime definitions describe stable Fast Sandbox behavior. Runtime environments
bind those definitions to the container runtime installation on a class of
nodes. This keeps host-specific namespaces, roots, sockets and selectors out of
SandboxPool and out of generic runtime drivers.

The production manifests install
`fast-sandbox-system/fast-sandbox-runtime-environments`. Its data key is
`runtime-environments.yaml`:

```yaml
environments:
  default:
    containerd:
      socket: /run/containerd/containerd.sock
      namespace: k8s.io
      snapshotter: overlayfs
      root: /var/lib/containerd
    kubelet:
      root: /var/lib/kubelet
    runtimes:
      container: {}
      gvisor: {}
      kata-qemu: {}
      kata-clh: {}
      kata-fc: {}
      boxlite: {}
```

An operator can bind a runtime to another node environment without adding
branches to Controller, Fastlet, the containerd driver, Infra delivery or
NodeJanitor:

```yaml
environments:
  custom:
    nodeSelector:
      runtime.example.com/enabled: "true"
    containerd:
      socket: /run/containerd/containerd.sock
      namespace: custom
      snapshotter: overlayfs
      root: /srv/containerd/root
    kubelet:
      root: /srv/kubelet
    runtimes:
      container:
        handler: io.containerd.custom.v2
        runtimePath: /opt/custom/bin/containerd-shim-custom-v2
```

Defining a runtime under a new environment moves that runtime from its previous
environment. One runtime may have only one active environment binding.

The Pool Controller merges the selected environment with the built-in runtime
definition and creates an immutable per-Pool ConfigMap named like:

```text
<pool>-runtime-<revision>
```

The plan revision participates in the Fastlet Pod template hash. Updating the
platform ConfigMap therefore creates a new Fastlet generation and uses the
existing surge-and-drain lifecycle; running Sandboxes are not silently mutated.

NodeJanitor reads the same configuration at startup and scans every configured
containerd namespace. Restart the NodeJanitor DaemonSet after changing the
platform ConfigMap so cleanup and newly rolled Fastlets use the same environment
set.

The configuration is privileged platform input. Do not expose it as a
tenant-editable SandboxPool field: it controls node selectors, host paths,
runtime binaries and containerd namespaces.
