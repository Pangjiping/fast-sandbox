# Egress 集成方案：fast-sandbox × OpenSandbox（OSEP-0022 / PR #1678）

> 文档类型：技术方案（集成 + 测试）
>
> 日期：2026-08-29
>
> 依赖：OpenSandbox PR #1678（egress fleet profile 改造，待合并）；
> fast-sandbox #30（Sandbox Actions 协议与 CRD，已合入 master）。
>
> 关联：OSEP-0022（多沙箱 egress 控制面）、opensandbox-group/OpenSandbox#1582、
> fast-sandbox #28（per-clone netns，egress source-IP dispatch 前置）。

---

- [背景与 PR #1678 要点](#背景与-pr-1678-要点)
- [现状盘点：fast-sandbox 已具备 / 缺失](#现状盘点fast-sandbox-已具备--缺失)
- [集成架构](#集成架构)
- [工作分解](#工作分解)
- [测试方案](#测试方案)
- [验收标准](#验收标准)
- [风险与依赖](#风险与依赖)
- [待决策项](#待决策项)

## 背景与 PR #1678 要点

OSEP-0022 原方案：egress 通过**观察 fastlet slot-store 文件**（
`/run/fast-sandbox/network/*.json`）驱动 subject 生命周期。PR #1678 将
该通道改为 **fastlet Sandbox Actions Handler 协议**
（`sandbox.fast.io/actions/v1`）：

- fastlet 经 Pod-loopback HTTP 投递 `SET_BINDING` / `LIFECYCLE_HOOK` /
  `REMOVE_BINDING`，egress 进程作为 Handler；
- `SET_BINDING`：注册 subject 并 deny-first（attachment 提供
  IP/gateway/veth/CIDR）；null input 回退 deny-first；
- `LIFECYCLE_HOOK`：`sandbox.runtime-ready` 确认、`sandbox.data-plane-ready`
  应用策略 → active；无 pending 策略 fail-closed（409）；
- `REMOVE_BINDING`：终态清理（fence 不匹配忽略）；
- 删除文件观察（pkg/slotsource/sandboxnft/resolvrewrite 共 -3133 行）；
- 凭据通道不变：仍走 proxy route `/credential-vault`（memory-only）。

**fast-sandbox 侧要求（PR #1678 Notes）**：
1. Pool `actionHandlers` 声明 egress（`targetHTTPPort 18080`，hooks：
   runtime-ready + data-plane-ready）；
2. OSEP-0022 的"四内部新增"（credential 通道）：host-process 交付模式、
   fastlet-proxy host upstream、UID 传播、egress route parsing。

## 现状盘点：fast-sandbox 已具备 / 缺失

### 已具备（#30 已合入 master）

| 项 | 位置 | 说明 |
|----|------|------|
| Actions 协议 | `internal/protocol/action` | 与 PR #1678 同一 `sandbox.fast.io/actions/v1`：SET_BINDING/LIFECYCLE_HOOK/REMOVE_BINDING、NetworkAttachment、HandlerStatus |
| Pool CRD | `api/v1alpha2/sandboxpool_types.go` | `spec.actionHandlers`（targetHTTPPort/hooks/校验：名称不可删改） |
| fastlet 投递 | `internal/fastlet/action/manager.go` + `sandbox/actions.go` | SetBinding 在 runtime 创建前投递；RecordHook/ReachHook（runtime-ready/data-plane-ready）；Handler 重启重放；bindings 状态机（Pending→…） |
| data-plane-ready 触发 | `internal/fastlet/sandbox/infra_lifecycle.go` / `admission.go` | 数据面就绪时投递 hook（sequence 2） |
| attachment 网络信息 | `sandbox/actions.go:actionAttachment` | 从 SandboxMetadata（slot）取 IP/gateway/veth/CIDR |
| 错误码 | `fastletapi.ErrorActionUnavailable` 等 | 协议映射就绪 |

### 缺失（本方案工作）

| 项 | 说明 | 归属 |
|----|------|------|
| **host-process 交付模式** | `InfraDeliveryMode` 新增 `host-process`：Pool revision 携带、但不进 in-sandbox 的 sandbox-init supervisor 配置；egress daemon 由 FastletTemplate 部署；readiness 探测 Pod-netns listener | fast-sandbox |
| **fastlet-proxy host upstream** | egress route 转发到 Pod-netns listener（`127.0.0.1:18080`）而非 sandbox Access 地址 | fast-sandbox |
| **UID 传播** | proxy 注入 `X-Fast-Sandbox-Uid`（subject 标识） | fast-sandbox |
| **egress route parsing** | `parseTarget` 增加 `/v1/sandboxfleets/{sandboxId}/egress/*` 分支（凭据校验 + 目标 egress） | fast-sandbox |
| **egress 实现/部署** | OpenSandbox fleet egress（PR #1678）+ 集成环境部署（host 域组件，共享 slot-store 不需要了——actions 驱动） | OpenSandbox |
| **firecracker 衔接验证** | data-plane-ready 时机（restore + 网络就绪）；clone 网络下 egress 流量 src=slot.IP（#28 已保证唯一性） | 集成测试 |

## 集成架构

```text
┌─ Kind 集群（Pod netns = fastlet Pod 网络）────────────────────────────┐
│                                                                       │
│  fastlet pod                                                          │
│   ├─ fastlet（特权）：                                                │
│   │    runtime-ready / data-plane-ready ──actions HTTP──▶ egress      │
│   │    SET_BINDING（slot attachment）──────┐          │              │
│   │                                         │          ▼              │
│   ├─ fastlet-proxy ──/v1/sandboxfleets/{id}/egress/*──▶ egress       │
│   │    （UID 头注入 + host upstream :18080）│    (Handler，Pod netns)  │
│   │                                         │          │              │
│   └─ egress（host-process 交付，Pod netns）◀─┘          │              │
│        ├─ deny-first（SET_BINDING 即装 nft）◀───────────┘              │
│        ├─ policy push → active（data-plane-ready）                    │
│        ├─ DNS proxy（:53 网关绑定）+ nft per-subject set              │
│        └─ credential vault（proxy route，memory-only）                │
│                                                                       │
│   sandbox 流量：guest ──► slot netns（#28：SNAT 后 src=slot.IP 唯一）  │
│                  ──► host forward hook（egress 主 enforcement）        │
└───────────────────────────────────────────────────────────────────────┘
```

数据流（对应 OSEP-0022 Sequences，通道变为 actions）：

```text
创建：fastlet 预投递 SET_BINDING（deny-first）
      → runtime-ready hook（确认）
      → data-plane-ready hook（策略应用 → active）
策略/凭据：server → fastlet-proxy（route credential + UID 头）→ egress
删除：REMOVE_BINDING（fence 校验）→ 卸载
```

## 工作分解

### A. fast-sandbox 四内部新增

| 子项 | 改动 | 测试 |
|------|------|------|
| A1 host-process 交付模式 | `internal/catalog/runtime/catalog.go` 新增 `InfraDeliveryHostProcess`；`sandbox-init` supervisor 生成时排除 host-process 组件；readiness 探测目标为 Pod-netns listener（egress 组件特殊化） | 单测：plan 编译/排除/探测目标 |
| A2 proxy host upstream | `internal/dataplane/fastletproxy/proxy.go`：egress 目标类型（host 转发 `127.0.0.1:18080`，非 DirectIP/LocalForward） | 单测：转发目标选择 |
| A3 UID 传播 | proxy 注入 `X-Fast-Sandbox-Uid`（对 egress route；不参与 stripRouteHeaders） | 单测：头注入 + 不剥离 |
| A4 egress route parsing | `parseTarget` 增加 `/v1/sandboxfleets/{id}/egress/*` 分支：解析 sandbox 路由、校验 route credential、目标 egress listener | 单测：路由匹配/凭据/目标 |

### B. Pool 声明（集成环境样例）

```yaml
spec:
  actionHandlers:
  - name: egress
    targetHTTPPort: 18080
    hooks: [sandbox.runtime-ready, sandbox.data-plane-ready]
```

- 新增 `config/samples/pool-firecracker-egress.yaml`（在 pool-firecracker
  基础上 + actionHandlers + egress 容器进 fastletTemplate）；
- fastlet 已有投递逻辑（#30），声明即生效。

### C. egress 侧（OpenSandbox，跨仓库）

- 依赖 PR #1678 合并；egress 二进制/镜像（fleet profile）由 OpenSandbox
  侧产出；
- 集成环境部署：egress 容器进 fastlet pod（host-process 交付，共享 Pod
  netns），监听 18080 + DNS 网关端口。

### D. firecracker 衔接验证

- **data-plane-ready 时机**：restore 完成 + 网络就绪（guest eth0 + slot
  netns 数据面 #28）后投递——确认 infra 生命周期在 firecracker 模式下
  的触发点正确；
- **egress dispatch**：guest 出网经 netns SNAT（#28）→ host forward 处
  src=slot.IP 每实例唯一 → egress 按 source IP 分 subject（OSEP-0022
  前置的实测证明）；
- **兄弟隔离**：egress 的 nft per-subject 策略 vs netns FORWARD 隔离
  （#28）双保险。

## 测试方案

### 单元测试（fast-sandbox 侧）

- **A1**：host-process 组件不出现在 sandbox-init supervisor 配置；
  readiness 探测目标解析为 Pod-netns listener；
- **A2**：egress 目标的 proxy 转发到 127.0.0.1:18080（fake upstream
  断言目标/头）；非 egress 路由行为不变；
- **A3**：UID 头注入（sandboxId 正确）；route 剥离逻辑不剥离 UID 头；
- **A4**：route parsing 的路径匹配/凭据校验/错误分支（未知 sandbox、
  凭据不匹配、非法路径）。

### 集成测试（Kind 集成环境，扩展 integration-env.sh）

新增 `verify-egress` 阶段（复用现有 up 环境）：

1. **部署**：egress 容器进 fastlet pod（host-process），Pool 声明
   actionHandlers；
2. **生命周期断言**：
   - Sandbox 创建：SET_BINDING → deny-first（创建后、策略推送前
     sandbox 无法出网）；
   - runtime-ready / data-plane-ready hooks 送达（egress 侧日志/状态）；
   - 策略推送（proxy route + UID 头）→ active → sandbox 出网恢复；
3. **per-subject 隔离**：同一 fastlet 上两个 sandbox 不同策略（allow
   A / deny B）分别验证 DNS + 出网；
4. **firecracker 模式**：egress 流量 src=slot.IP（`nft trace`/计数）；
   两个 clone sandbox 各自受控；
5. **fail-closed 时序**：策略推送晚于绑定（人为延迟）→ 期间 deny；
   推送后恢复；无 pending 策略时 data-plane-ready → 409（fail-closed）；
6. **删除**：REMOVE_BINDING → 规则卸载、subject 释放；重复删除幂等；
7. **重启恢复**：egress 重启 → fastlet 重放（Handler restart replay）；
   ApplyReset 后重新 deny → server 重推 → active；
8. **清理**：全部 sandbox 删除后 egress 无残留规则（nft list 断言）。

### 回归

- 无 egress 声明的既有 Pool 行为不变（actions 未配置时零影响）；
- container/gvisor/kata 的既有 egress（sidecar profile）不变
  （OSEP-0022：sidecar 模式 untouched）。

## 验收标准

1. fast-sandbox 四内部新增单测全绿（-race + vet）；
2. `verify-egress` 在集成环境全绿：生命周期/隔离/firecracker 模式/
   fail-closed/删除/重启 8 项断言通过；
3. firecracker egress 流量 src=slot.IP 每实例唯一（nft 观测证据）；
4. 无 egress 声明的 Pool 与既有 egress sidecar 回归全绿；
5. 日志/回收要求沿用集成环境既有规范（logs/、down 无残留）。

## 风险与依赖

| 项 | 影响 | 缓解 |
|----|------|------|
| OpenSandbox PR #1678 未合并 | 集成联调阻塞 | 先用 fast-sandbox 侧 mock egress（actions handler 的 Go mock，验证四内部新增与 fastlet 投递），PR 合并后替换真 egress |
| 四内部新增跨两个子系统（catalog/proxy） | 改动面 | 单测先行，每项独立提交 |
| data-plane-ready 在 firecracker 模式的时机语义 | egress 生效窗口 | 集成测试断言（fail-closed 窗口验证） |
| firecracker clone 网络与 egress dispatch 联调 | source-IP 唯一性 | #28 已保证；集成测试 nft 观测确认 |

## 待决策项

1. **egress 联调归属**（设计待决策项 1）：mock egress（fast-sandbox 侧）
   先行 vs 等 PR #1678 合并后直接联调（推荐：mock 先行，不阻塞）；
2. **四内部新增的落地顺序**：A1→A4 顺序 vs 按 egress 集成优先级；
3. **egress 容器形态**：进 fastlet pod（共享 Pod netns，host-process）
   vs 独立 DaemonSet——host-process 交付模式定义后定。
