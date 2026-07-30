# NetworkPolicy sample

`default.yaml` demonstrates ingress isolation for the default split deployment:

- platform components in `fast-sandbox-system`;
- SandboxPools and Fastlet Pods in `fast-sandbox`.

It is intentionally not part of `config/default`.

Before applying it:

1. label authorized client Pods with `fast-sandbox.io/control-plane-client=true` or `fast-sandbox.io/data-plane-client=true`;
2. label Prometheus Pods with `fast-sandbox.io/metrics-client=true`;
3. copy the Fastlet policy into every namespace that contains SandboxPools and adjust the namespace selectors for the control plane and proxy;
4. confirm that the CNI enforces NetworkPolicy for Pod-to-Pod traffic.

This sample restricts ingress only. Sandbox egress policy depends on DNS, registry, metadata-service, and tenant policy requirements and must be defined by the deployment owner.
