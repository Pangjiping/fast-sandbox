# Sandbox Actions 使用手册

Sandbox Actions 把每个 Sandbox 的有序业务配置交给 Fastlet Pod 内的
Action Handler，并在两个本地生命周期检查点通知 Handler。Handler 不是用户
Sandbox 内部插件，而是 Pool 管理员部署的 Pod-local 扩展进程。

## 1. Quick Start

在开发环境启用 Actions 示例：

```bash
make quickstart ACTIONS=demo
```

该命令会额外构建独立的 `sandbox-action-fixture` 测试镜像，创建带 `egress`
Handler 的示例 Pool，并打印创建、更新、查看和删除 Sandbox 的命令。Fixture
不在 Fastlet 生产镜像中。

## 2. 在 SandboxPool 声明 Handler

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: actions-pool
spec:
  capacity: {poolMin: 1, poolMax: 1, bufferMin: 0, bufferMax: 1}
  maxSandboxesPerPod: 8
  runtime: container
  sandboxResources:
    cpu: 500m
    memory: 512Mi
    pids: 128
  actionHandlers:
  - name: egress
    targetHTTPPort: 18080
    hooks:
    - sandbox.runtime-ready
    - sandbox.data-plane-ready
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: fast-sandbox/fastlet:dev
      - name: egress-handler
        image: example.com/egress-handler:v1
        readinessProbe:
          httpGet:
            host: 127.0.0.1
            port: 18080
            path: /_fastlet/v1/actions/status
```

Handler 名称和端口在 Pool 内唯一。`targetHTTPPort` 总是通过 Pod loopback
访问。空 `hooks` 表示配置型 Handler，只接收 Binding 同步和终态清理。

当前支持：

- `sandbox.runtime-ready`：runtime 创建完成，私网身份可用；
- `sandbox.data-plane-ready`：Infra Components 就绪且 proxy route 已发布。

未知 Hook 会被拒绝，不会静默忽略。

## 3. 实现 Handler HTTP 协议

### 3.1 状态端点

```text
GET /_fastlet/v1/actions/status
```

```json
{
  "apiVersion": "sandbox.fast.io/actions/v1",
  "ready": true,
  "instanceId": "egress-process-7"
}
```

`instanceId` 标识 Handler 进程 incarnation，每次进程重启必须变化。Fastlet
发现变化后会对当前 Sandbox 重放最新 `SetBinding`，再按顺序补发已经到达且
该 Handler 订阅的 Hook。

### 3.2 调用端点

```text
POST /_fastlet/v1/actions
Content-Type: application/json
```

示例 `SetBinding`：

```json
{
  "apiVersion": "sandbox.fast.io/actions/v1",
  "operation": "SET_BINDING",
  "invocationId": "sha256:...",
  "sandbox": {
    "uid": "7f...",
    "name": "agent-a",
    "namespace": "default"
  },
  "revision": {
    "specGeneration": 3,
    "runtimeInstanceId": "runtime-2",
    "attachmentId": "sha256:...",
    "routeGeneration": 4
  },
  "attachment": {
    "network": {
      "ip": "172.30.0.2",
      "gateway": "172.30.0.1",
      "privateCidr": "172.30.0.0/24",
      "hostVeth": "fh..."
    }
  },
  "binding": {
    "input": "{\"defaultAction\":\"deny\"}"
  }
}
```

协议只有三种正交操作：

- `SET_BINDING`：同步完整 input；`input` 非空指针时允许值为 `""` 或字面量
  `"null"`，JSON `null` 表示从仍存活的 Sandbox 移除该 Binding；
- `LIFECYCLE_HOOK`：通知已到达的检查点，不重复携带业务 input；
- `REMOVE_BINDING`：整个 Sandbox 的终态清理，无 input。

只有 HTTP 200 表示成功。调用是 at-least-once；同一逻辑操作重试会复用稳定
的 `invocationId`，Handler 必须幂等。成功的 `SetBinding` 必须表示配置已经对
Sandbox 当前所处生命周期阶段生效，因此普通 input 更新不会重放 Hook。

## 4. 创建 Sandbox

CRD 使用有序原子数组：

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: agent-a
spec:
  poolRef: actions-pool
  image: python:3.13
  command: ["python", "-m", "http.server", "8080"]
  actionBindings:
  - handler: egress
    input: '{"defaultAction":"deny"}'
```

`input` 是不透明字符串，不要求 JSON。以下值语义不同且都会逐字节保留：

```yaml
input: ""
input: "null"
input: |-
  default: deny
  allow: [example.com]
```

Fastctl 等价形式：

```bash
fastctl run agent-a \
  --pool actions-pool \
  --image python:3.13 \
  --action 'egress={"defaultAction":"deny"}'
```

FastPath `CreateCompletion` 有两个边界：

- 默认 `READY`：Runtime、DataPlane、Infra Components 和全部 Bindings 都
  Ready 后返回；
- `RUNTIME_READY`：runtime 创建成功后返回，其他部分可能仍在收敛。

等待发生在 Fastlet 本地，不依赖 CRD Status watch，也没有
`WaitSandboxReady` RPC。

## 5. 观察状态

```yaml
status:
  observedGeneration: 1
  runtime:
    state: Ready
    generation: 1
  dataPlane:
    state: Ready
    routeGeneration: 1
  actionBindings:
  - handler: egress
    state: Ready
  conditions:
  - type: Ready
    status: "True"
    observedGeneration: 1
```

Binding Ready 表示当前 input 已 `SetBinding`，且该 Handler 订阅并已到达的
Hook 全部成功。失败不会回滚已经创建的 runtime、Infra 或 route，但整体
Ready 保持 False，Fastlet 在本地持续重试。

FastPath `GetSandbox` 会调用 assigned Fastlet，返回实时 `SandboxInfo`。
`UpdateSandbox` 返回 `committed_generation`；调用方可轮询 Get，直到
`applied_generation >= committed_generation` 且相关 Binding Ready。

## 6. 更新和移除 Binding

更新替换完整有序数组：

```bash
fastctl update agent-a \
  --action 'audit=tenant=demo' \
  --action 'egress={"defaultAction":"allow"}'
```

或直接 patch CRD：

```bash
kubectl patch sandbox agent-a --type=merge -p='{
  "spec":{"actionBindings":[
    {"handler":"egress","input":"{\"defaultAction\":\"allow\"}"}
  ]}
}'
```

新/保留 Binding 按新数组顺序执行 `SetBinding`。从列表移除的 Binding 按旧
数组逆序执行 `SetBinding(input=null)`，这是 Ready barrier，失败会重试。

清空全部 Bindings：

```bash
fastctl update agent-a --clear-actions
```

这不是终态 `RemoveBinding`：Sandbox 仍然存活，之后可以重新绑定。

## 7. 顺序、重启和删除

- 同一 Sandbox + Handler 的 Set/Hook/Remove 串行；不同 Sandbox 可并发；
- Set 和 Hook 按 Binding 声明顺序；解除和终态删除按逆序；
- Hook 到达早于当前 generation 的 Set 成功时，先 Pending，Set 成功后补发；
- Handler 重启后由 Fastlet 重放 Set 和已到达 Hook；
- Fastlet 重启后由 Controller 重新下发 desired Bindings，Hook 仍只由 Fastlet
  根据本地 checkpoint 发送。

删除时，Fastlet 先原子标记 Sandbox 为 Terminating，禁止新的 Set/Hook，再
对全部 Handler 逆序执行 `RemoveBinding`。所有 Handler 共享固定 5 秒总
deadline；错误只写诊断，不阻塞 route、runtime 和 network teardown。

## 8. ResolveEndpoint

`ResolveEndpoint` 不承担等待语义。它读取 assigned Fastlet 的实时状态，确认
整体 Ready、目标 component route Ready，再签发带 `routeGeneration` fence 的
短期凭证。未 Ready 返回 `FailedPrecondition`，调用方自行退避重试。

缓存已同步时，该调用不访问 Kubernetes apiserver；如果调用方携带的
`expected_generation` 高于本地 cache，则回退一次直接 GET。

## 9. 测试

单元测试：

```bash
go test ./internal/fastlet/action \
  ./internal/fastlet/sandbox \
  ./internal/controlplane/orchestrator \
  ./internal/controlplane/reconciler \
  ./internal/controlplane/fastpath
```

Actions E2E：

```bash
make e2e SUITE=sandboxactions E2E_TEST_TIMEOUT=15m
```

E2E 覆盖 opaque input、空字符串/`"null"`、声明顺序、两个 Hook、Ready
barrier、Binding 更新和解除、Handler/Fastlet 重启重放，以及逆序终态清理。
