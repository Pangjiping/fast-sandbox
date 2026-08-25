# Fastlet Pod Port routes

Fast Sandbox routes Sandbox application traffic through the data plane
(`Sandbox Proxy -> Fastlet Proxy -> Sandbox`). Some control-plane workloads,
however, must reach a service that runs **on the Fastlet Pod itself** - for
example a control-plane sidecar co-located with Fastlet - and never touch the
Sandbox runtime.

Fastlet Pod Port routes cover this case:

```text
client -> FastPath ResolveEndpoint(pod_port_name)
       -> direct http://<Fastlet Pod IP>:<port>
       -> sidecar on the Fastlet Pod
```

The route is a direct Fastlet Pod address, independent of `access_mode`, and
does not pass through `Sandbox Proxy` or `Fastlet Proxy`.

## Declare the Pod Port allowlist

Pod Ports are declared on the Pool. FastPath refuses to resolve any port that
is not in this allowlist. Platform-owned ports on the Fastlet Pod are rejected
(`5758` Fastlet control API, `5780` Fastlet Proxy data plane).

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: control-pool
  namespace: fast-sandbox
spec:
  podPorts:
    - name: sidecar
      port: 9000
  fastletTemplate:
    spec:
      containers:
        # The controller injects FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY into the
        # Fastlet and fastlet-proxy containers. Set the same value on the
        # sidecar container so it can verify route credentials.
        - name: fastlet
          image: fast-sandbox/fastlet:dev
        - name: control-sidecar
          image: registry.example.com/control-sidecar:v1
          env:
            - name: FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY
              value: "<base64 Ed25519 public key of the controller>"
          ports:
            - containerPort: 9000
              name: control
  capacity:
    poolMin: 1
    poolMax: 2
  maxSandboxesPerPod: 5
  runtime: container
  sandboxResources:
    cpu: 250m
    memory: 256Mi
    pids: 128
```

The allowlist is a platform gate: the sidecar container itself is declared
through `fastletTemplate` like any other pod change, and the controller
validates `podPorts` on every Pool reconcile.

## Resolve the route

The client resolves the sidecar address through FastPath:

```go
resolver := &sandboxclient.EndpointResolver{
    Control:          fastpathClient, // fastpathv2.FastPathServiceClient
    DefaultNamespace: "fast-sandbox",
}

route, err := resolver.Resolve(ctx, sandboxclient.SandboxRef{Name: "sandbox-a"}, sandboxclient.PodPortTarget("sidecar"))
if err != nil {
    return err
}

request, err := http.NewRequestWithContext(ctx, http.MethodGet, route.RequestURL("/control/status", nil).String(), nil)
if err != nil {
    return err
}
route.ApplyHeaders(request) // adds X-Fast-Sandbox-Route-Credential
```

FastPath returns `http://<Fastlet Pod IP>:<port>` and an instance-fenced
credential. The credential binds the Fastlet Pod UID, assignment attempt, and
route generation: a replaced or reassigned Fastlet Pod invalidates every
outstanding credential immediately.

## Verify on the sidecar

The sidecar must verify the credential before serving any request, and fail
closed. The Fastlet Pod already receives the verification public key; read it
from the same environment variable:

```go
func newVerifier() (*routeauth.Verifier, error) {
    keys, err := routeauth.ParsePublicKeySet(os.Getenv("FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY"))
    if err != nil {
        return nil, err
    }
    return routeauth.NewVerifierSet(keys, time.Now)
}

// ownPodUID comes from the Kubernetes downward API (metadata.uid).
func authorize(verifier *routeauth.Verifier, ownPodUID string, header http.Header) error {
    token := header.Get(dataplane.HeaderRouteCredential)
    claims, err := verifier.VerifyFastletPortCredential(token, ownPodUID, 9000)
    if err != nil {
        return err
    }
    // claims.SandboxUID identifies the Sandbox whose assignment produced the
    // credential; use it for per-sandbox authorization decisions.
    _ = claims
    return nil
}
```

`VerifyFastletPortCredential` checks the signature and expiry, requires
`targetKind=fastlet-port`, and matches the Fastlet Pod UID and target port.
Sandbox-level claims (Sandbox UID, assignment attempt, route generation) are
available on the returned `Claims` for finer-grained authorization.

## Security model

- only FastPath can mint credentials (Ed25519 private key held by the control
  plane);
- only ports declared in `podPorts` can be resolved;
- credentials are short-lived and fenced to the exact Fastlet Pod instance;
- the sidecar verifies every request and fails closed;
- Pod Port routes never enter the Sandbox network namespace.

See [API reference](../reference/api.md) for the wire contract.
