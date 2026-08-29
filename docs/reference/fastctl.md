# fastctl reference

`fastctl` owns lifecycle commands, platform diagnostics, endpoint discovery,
and hand-off to protocol-specific Infra Component SDKs.

## Build and configuration

```bash
make build COMPONENT=fastctl
```

The binary is written to `bin/fastctl`.

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--config` | — | `./.fastctl/config.json` | Explicit configuration file |
| `--endpoint` | `FAST_SANDBOX_ENDPOINT` | `localhost:9090` | FastPath gRPC endpoint |
| `--namespace`, `-n` | — | `fast-sandbox` | Resource namespace |
| `--proxy-endpoint` | `FAST_SANDBOX_PROXY_ENDPOINT` | resolved by FastPath | Host-visible Sandbox Proxy override |

Precedence is changed command-line flag, environment variable, local
configuration file, then built-in default.

```json
{
  "endpoint": "127.0.0.1:9090",
  "namespace": "fast-sandbox",
  "proxy-endpoint": "http://127.0.0.1:18080"
}
```

`make quickstart` creates `.fastctl/config.json` only when it is absent. It
never modifies an existing file.

## Lifecycle

Create the complete initial intent in one call:

```bash
fastctl run my-sandbox \
  --image docker.io/library/alpine:latest \
  --pool default-pool \
  --expires-at 1785373200 \
  --metadata owner=team-a,tier=dev \
  -- /bin/sleep 3600
```

The Sandbox name is the request ID and idempotency key. `-f <file>` accepts the
equivalent YAML fields.

Inspect:

```bash
fastctl list
fastctl get my-sandbox -o yaml
fastctl diagnostics sandbox my-sandbox --limit 100
```

Diagnostics show CRD state, assignment identity, Fastlet reachability, and
bounded lifecycle events. They do not depend on an Infra Component and do not
show user process stdout.

Update:

```bash
fastctl update my-sandbox --expire-time 1785376800
fastctl update my-sandbox --expire-time 0
fastctl update my-sandbox --metadata owner=team-b
fastctl update my-sandbox --delete-metadata tier
fastctl update my-sandbox --failure-policy AutoRecreate
fastctl update my-sandbox --recovery-timeout 120
```

Reset and delete are declarative:

```bash
fastctl reset my-sandbox
fastctl delete my-sandbox
```

## OpenSandbox adapter

`opensandbox` selects the official OpenSandbox Execd SDK. The inherited
`--component` option selects the Pool component implementing that protocol and
defaults to `execd`.

```bash
fastctl opensandbox exec my-sandbox -- ls -la
fastctl opensandbox exec my-sandbox \
  --component custom-execd -- ls -la
```

The grammar is:

```text
fastctl opensandbox exec <sandbox> [adapter options] -- <remote argv>
```

`--component` must appear before `--`. fastctl resolves the named component
from the assigned Fastlet's live state; resolution is non-blocking and requires
aggregate Ready. It does not hard-code port 44772 for route selection.

File operations:

```bash
fastctl opensandbox cp ./local.txt my-sandbox:/tmp/remote.txt
fastctl opensandbox cp my-sandbox:/tmp/remote.txt ./local.txt

fastctl opensandbox files stat my-sandbox /tmp/remote.txt
fastctl opensandbox files list my-sandbox /tmp
fastctl opensandbox files read my-sandbox /tmp/remote.txt
fastctl opensandbox files write my-sandbox /tmp/remote.txt ./local.txt
fastctl opensandbox files mkdir my-sandbox /tmp/example
fastctl opensandbox files rm my-sandbox /tmp/remote.txt
```

Fast Sandbox resolves and authenticates the route. Execd and the OpenSandbox
SDK define command and file semantics. Execd runs without its optional access
token.

## Local port forwarding

An in-cluster Service is not resolvable from a development host. Keep:

```bash
make quickstart-forward
```

running and use the Quick Start config or:

```bash
export FAST_SANDBOX_ENDPOINT=localhost:9090
export FAST_SANDBOX_PROXY_ENDPOINT=http://localhost:18080
```
