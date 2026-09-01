# Egress 集成施工任务清单（转交实现）

> 文档类型：施工任务清单（转交实现）
>
> 日期：2026-08-29
>
> 方案：见 [egress-integration-plan.md](egress-integration-plan.md)
> （本清单每一项对应方案章节；分歧以方案文档为准）。
>
> 依赖：OpenSandbox PR #1678（egress 实现，未合并）；fast-sandbox #30
> （Actions 协议/CRD，已合入）。**egress 镜像固定为
> `docker.io/opensandbox/egress:latest`（需求方提供，直接写死使用）**，
> 本清单不包含镜像构建。

---

## 任务总览

| # | 任务 | 产出物 | 依赖 |
|---|------|--------|------|
| 1 | 协议交叉验证 | 核对结论（文档/测试） | — |
| 2 | A1：host-process 交付模式 | catalog + sandbox-init 生成逻辑改动 | — |
| 3 | A2+A3：proxy host upstream + UID 头 | fastlet-proxy 改动 | — |
| 4 | A4：egress route parsing | fastlet-proxy 改动 | 3 |
| 5 | Pool 样例 + egress 容器部署形态 | `pool-firecracker-egress.yaml` + fastletTemplate 更新 | 2,4 |
| 6 | 集成测试（verify-egress） | integration-env.sh 扩展 | 5 + egress 镜像 |
| 7 | 回归与清理 | 全量测试绿 + 文档 | 全部 |

---

## 任务 1：协议交叉验证（集成测试第一项的前置）

**背景**：fast-sandbox `internal/protocol/action` 与 OpenSandbox
`components/egress/pkg/actionhandler` 是两侧独立实现的同一协议
（`sandbox.fast.io/actions/v1`）——必须先核对字节级一致性。

- [ ] 拉取 egress 源码：`Pangjiping/OpenSandbox` @ `feat/egress-actions-handler`
  （pin `460b1cb`），路径 `components/egress/pkg/actionhandler/`
- [ ] 逐项核对（产出核对表写入 `docs/guides/egress-protocol-verification.md`）：
  - [ ] `apiVersion` 常量与校验（`sandbox.fast.io/actions/v1`）
  - [ ] Operation 枚举：`SET_BINDING` / `LIFECYCLE_HOOK` / `REMOVE_BINDING`
    （fast-sandbox `internal/protocol/action/types.go`）
  - [ ] LifecycleHook 枚举：`sandbox.runtime-ready` /
    `sandbox.data-plane-ready`
  - [ ] 请求字段：`invocationId`、`sandbox{uid,name,namespace}`、
    `revision{specGeneration,runtimeInstanceId,attachmentId,routeGeneration}`、
    `attachment.network{ip,gateway,privateCidr,hostVeth}`
  - [ ] `binding.input` 语义（null vs 字符串"null"——两侧 pointer 处理）
  - [ ] 错误语义：未知 handler / 无 pending 策略的 data-plane-ready（409）等
  - [ ] `GET /_fastlet/v1/actions/status` 的 `HandlerStatus` 字段
    （apiVersion/ready/instanceId/message）
- [ ] 差异项全部记录（如字段命名/大小写/可选性差异——实现者不修改协议，
  只记录，交回评审决策）
- [ ] 快速构建验证：egress 侧 `go build ./components/egress/...` 通过
  （不产出镜像，仅确认代码可编译——镜像由需求方提供）

**验收**：核对表完成；无未记录的协议差异；两侧独立实现可互操作
（留待任务 6 实测）。

## 任务 2：A1 host-process 交付模式

**位置**：`internal/catalog/runtime/catalog.go` +
`internal/fastlet/`（sandbox-init supervisor 生成逻辑）

- [ ] `InfraDeliveryMode` 新增 `InfraDeliveryHostProcess`（"host-process"）
- [ ] 语义：host-process 组件进入 Pool revision（编译进 infra plan），但
  **不出现在 in-sandbox 的 sandbox-init supervisor 配置**（guest 侧不
  感知）；readiness 探测目标 = **Pod-netns listener**（非 sandbox IP）
- [ ] 定位 sandbox-init supervisor 配置生成处（`internal/fastlet/infra/`
  或相关），host-process 组件排除逻辑
- [ ] 定位 readiness 探测目标解析（`internal/fastlet/sandbox/readiness.go`
  或 infra lifecycle），host-process 组件改为探测 Pod-netns
- [ ] 单测：
  - host-process 组件不进 supervisor 配置（生成断言）
  - readiness 探测目标解析为 Pod-netns（不 dial sandbox IP）
  - 非 host-process 组件行为不变（回归）

**验收**：单测绿；无 host-process 声明时行为与现状完全一致。

## 任务 3：A2+A3 proxy host upstream + UID 头

**位置**：`internal/dataplane/fastletproxy/proxy.go`

- [ ] A2：egress 目标类型——proxy 对 egress route 转发到
  **Pod-netns listener（127.0.0.1:18080）**（区别于现有 DirectIP/
  LocalForward 目标语义）
- [ ] A3：proxy 注入 `X-Fast-Sandbox-Uid` 头（值为 sandbox 标识）；
  **不参与 `stripRouteHeaders`**（route 剥离逻辑不删该头）
- [ ] 单测（`proxy_test.go`）：
  - egress 目标转发到 127.0.0.1:18080（fake upstream 断言目标与头）
  - UID 头注入正确（sandboxId）；剥离逻辑不剥离 UID 头
  - 非 egress 路由（DirectIP/LocalForward）行为不变（回归）

**验收**：单测绿；既有 proxy 测试全绿。

## 任务 4：A4 egress route parsing

**位置**：`internal/dataplane/fastletproxy/proxy.go`（`parseTarget`）

- [ ] `parseTarget` 增加 `/v1/sandboxfleets/{sandboxId}/egress/*` 分支：
  - 解析 sandbox 路由（sandboxId 定位）
  - 校验 route credential（与现有分支一致）
  - 目标 = egress listener（127.0.0.1:18080，host 转发语义见任务 3）
- [ ] 与 `ResolveEndpoint` 的 egress 目标语义对齐（两侧必须一致——
  OpenSandbox 侧 `fastpath_client.resolve_endpoint` 的 egress target）
- [ ] 单测：路径匹配、凭据校验、错误分支（未知 sandbox/凭据不匹配/
  非法路径）、非 egress 前缀行为不变

**验收**：单测绿；`/v1/sandboxes/`、`/v2/sandboxes/` 既有分支回归。

## 任务 5：Pool 样例 + egress 容器部署形态

- [ ] 新增 `config/samples/pool-firecracker-egress.yaml`（基于
  pool-firecracker）：
  - `actionHandlers: [{name: egress, targetHTTPPort: 18080,
    hooks: [sandbox.runtime-ready, sandbox.data-plane-ready]}]`
  - egress 容器进 fastletTemplate（host-process，共享 Pod netns，
    镜像 **`docker.io/opensandbox/egress:latest`** 直接写死）
  - egress 的 host-process 组件声明（infra 组件 + DeliveryMode）
- [ ] 确认 fastlet 对 actionHandlers 的投递在 firecracker runtime 下生效
  （#30 已实现，声明即用——验证 `recordActionHook` 的 data-plane-ready
  在 restore + 网络就绪后触发）
- [ ] 集成环境文档同步：`docs/guides/firecracker-integration-environment.md`
  补 egress 部署步骤

**验收**：Pool apply 后 egress 容器运行、actionHandlers 状态 Pending
（等待 sandbox 创建触发 SET_BINDING）。

## 任务 6：集成测试（verify-egress，扩展 integration-env.sh）

**前置**：`docker.io/opensandbox/egress:latest`（需求方提供）；
`kind load docker-image` 进集群（或在 fastlet pod 中直接可拉取）。

- [ ] `integration-env.sh` 新增 `verify-egress` 阶段（复用现有 up 环境）：
  - **0. 协议一致性实测**：SET_BINDING 被 egress 正确解析（egress 日志/
    状态断言）——任务 1 核对表的实测闭环
  - **1. 生命周期**：Sandbox 创建 → SET_BINDING（deny-first）→
    runtime-ready → 策略推送（proxy route + UID 头）→ data-plane-ready
    → active → sandbox 出网恢复
  - **2. fail-closed 窗口**：策略推送前 sandbox 无法出网（DNS
    NXDOMAIN/转发 drop）；推送后恢复
  - **3. per-subject 隔离**：同一 fastlet 两个 sandbox 不同策略
    （allow A / deny B），DNS + 出网分别验证
  - **4. firecracker 模式**：egress 流量 src=slot.IP（nft trace/计数）；
    两个 clone sandbox 各自受控
  - **5. 删除**：REMOVE_BINDING → 规则卸载、subject 释放；重复删除幂等
  - **6. 重启恢复**：egress 重启 → fastlet 重放（Handler restart
    replay）；ApplyReset 后重新 deny → server 重推 → active
  - **7. 清理**：全部 sandbox 删除后 egress 无残留规则（nft list）
- [ ] 日志/回收沿用既有规范（logs/、failure 落盘、down 无残留）

**验收**：`verify-egress` 全绿（8 项断言）。

## 任务 7：回归与清理

- [ ] `go build ./...`、`go test ./internal/... -count=1 -race`、
  `go vet ./...` 全绿
- [ ] 无 egress 声明的既有 Pool 行为不变（集成环境回归：不带 egress 的
  pool-firecracker 全链路仍绿）
- [ ] egress sidecar profile（OpenSandbox 既有模式）不受影响
  （跨仓库回归说明）
- [ ] 更新方案文档/集成环境文档：实测值、协议核对结论、egress 镜像
  tag、已知差异

**验收**：全量测试绿；文档同步。

---

## 交付物汇总

| 产出 | 路径 |
|------|------|
| 协议核对表 | `docs/guides/egress-protocol-verification.md` |
| A1 | `internal/catalog/runtime/catalog.go` + sandbox-init 生成改动 + 单测 |
| A2+A3 | `internal/dataplane/fastletproxy/proxy.go` + 单测 |
| A4 | `internal/dataplane/fastletproxy/proxy.go`（parseTarget）+ 单测 |
| Pool 样例 | `config/samples/pool-firecracker-egress.yaml` |
| 集成测试 | `scripts/integration-env.sh`（+verify-egress） |
| 文档 | 集成环境指南 + 方案文档实测更新 |

## 验收标准（整体）

1. 任务 1-7 勾选完成，每项验收通过；
2. 协议交叉验证闭环（核对表 + 实测）；
3. `verify-egress` 8 项断言全绿（含 firecracker src=slot.IP 观测）；
4. 无 egress 声明的 Pool 与既有行为零影响；
5. 日志/回收要求沿用集成环境既有规范。

## 参考

- 方案：`docs/design/egress-integration-plan.md`
- egress 源码：`Pangjiping/OpenSandbox @ feat/egress-actions-handler`
  （pin `460b1cb`，`components/egress/`；镜像由需求方构建提供）
- fast-sandbox 已有：#30（`internal/protocol/action`、`ActionHandlers`
  CRD、fastlet 投递/重放）、#28（per-clone netns，egress source-IP
  dispatch 前置）、#29（集成环境 `integration-env.sh`）
- 协议：OSEP-0022（fast-sandbox 副本
  `docs/design/egress-integration-plan.md` 关联；OpenSandbox 侧 OSEP 原文）
