# SandboxTemplate 产物分发 OCI 化：rootfs 与 memory 走 OCI + OverlayBD，vmstate 留 S3

> 文档类型：设计提案
>
> 日期：2026-09-08
>
> 说明：本提案改变 SandboxTemplate 构建产物的**分发方式**（存储布局与拉取链路），
> 不改变产物的生成语义（快照内容、兼容性元组、readiness 校验）与消费语义
> （节点缓存布局、reflink、restore 路径均保持不变）。

- [Summary](#summary)
- [Motivation](#motivation)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [产物布局](#产物布局)
  - [镜像格式](#镜像格式)
  - [builder 构建流程](#builder-构建流程)
  - [controller 与 status](#controller-与-status)
  - [节点消费链路](#节点消费链路)
  - [原子性与一致性](#原子性与一致性)
  - [认证](#认证)
- [设计依据](#设计依据)
- [演进路径：内存按需加载](#演进路径内存按需加载)
- [Risks and Mitigations](#risks-and-mitigations)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Upgrade & Migration Strategy](#upgrad--migration-strategy)
<!-- /toc -->

## Summary

当前 SandboxTemplate 的全部构建产物（rootfs.ext4、memory.snap、vmstate.snap、
overlaybd 层、manifest.json、SHA256SUMS）发布到 S3 兼容对象存储，节点通过自建的
index/manifest/digest 校验链拉取。大文件（rootfs、memory）在该链路上没有压缩、
没有 P2P、没有按需加载，且整套分发逻辑（aws-cli 上传、S3 下载客户端、index
指针、SHA256SUMS）均为自建维护。

本提案按**数据大小与访问模式**重划分发通道：

| 产物 | 大小 | restore 后访问模式 | 分发通道 |
|------|------|--------------------|----------|
| `rootfs.ext4` | 逻辑 30Gi（稀疏） | 随机读 + 写（root drive） | **OCI registry**（单层 LSMT） |
| `memory.snap` | = vm 内存（2Gi+） | 随机读（COW，见下） | **OCI registry**（单层 LSMT） |
| `vmstate.snap` | MB 级 | 只读、一次性读入 | **S3**（保留现链路） |
| `manifest.json` | KB 级 | 元数据 | **S3**（保留现链路） |

rootfs 与 memory 两个大文件以 OverlayBD LSMT 层打包为标准 OCI 镜像，通过
streamingvolume（`gitlab.alibaba-inc.com/sbu/streamingvolume`）的进程内嵌入
service 完成打包与推送，获得 registry 生态（认证、复制、P2P、zstd 压缩、
按需取段），并为后续内存按需加载（直连块设备 restore）铺路。vmstate 与
manifest 是小文件、一次性消费，继续走现有 S3 链路，避免为它们引入额外的
OCI 打包流程。

节点缓存布局与 restore 路径**零改动**：runtime-agent 从 registry Attach 出
块设备后拷出文件、从 S3 拉 vmstate，落盘到现有缓存目录，之后的 reflink 与
`snapshot load` 逻辑不变。

## Motivation

1. **大文件分发效率**：memory.snap（≥2Gi）与 rootfs 实数据是节点冷拉的主要
   瓶颈。S3 裸传无压缩、无跨节点复用；OCI + OverlayBD 提供 zstd 压缩、
   blob 级 content-addressing 去重、P2P 分发与按需取段。
2. **删自建**：index 指针、SHA256SUMS、digest namespace、aws-cli 依赖、
   S3 客户端中大文件分支，均可由 registry 的 manifest/digest 语义承担。
3. **对齐 OverlayBD 生态**：产物带上 OverlayBD 层标注后，可被
   accelerated-container-image / streamingvolume 生态识别，是后续
   「内存按需 restore」（memory 直连块设备、缺页触发按需拉取）的结构前提，
   该方向与 [overlaybd-on-demand-loading.md](overlaybd-on-demand-loading.md)
   的规划一致。
4. **不激进**：vmstate/manifest 留在 S3，避免「为两个小文件打包 OCI 镜像」
   的额外复杂度；S3 侧存量链路（index、GC、digest 校验）继续工作。

## Goals

- rootfs / memory 以单层 LSMT 的 OCI 镜像发布到标准 registry，digest-pin
  引用。
- vmstate.snap + manifest.json 继续发布到 S3，布局与校验语义不变。
- builder、runtime-agent 通过进程内嵌入 streamingvolume service 完成打包、
  推送、拉取，不在 fast-sandbox 内手写 OCI manifest/blobs 代码。
- 节点缓存布局（`rootfs.img / vmstate.snap / memory.snap / manifest.json`）
  与 restore 路径（reflink + snapshot load）保持不变。
- `SandboxTemplate.status` 记录全部产物引用，作为跨双存储的原子提交点。

## Non-Goals

- 不改变 SandboxTemplate 的 spec schema 与构建执行模型（controller 驱动
  build Pod）。
- 不改变快照产物本身的语义（兼容性元组、guestNetwork、readiness 校验）。
- 不在本期实现内存按需加载（Mode 2，见[演进路径](#演进路径内存按需加载)），
  仅保证结构上可平滑演进。
- 不迁移 vmstate / manifest 离开 S3；不删除 S3 存储通道。
- 不引入跨模板的 base 镜像层叠（增量构建去重）；每 build 产物自包含。

## Proposal

### 产物布局

构建产物按通道拆分：

```
OCI registry:
  <registry>/<repo>/<template>-rootfs:<tag>@sha256:...   # 单层 LSMT(rootfs)
  <registry>/<repo>/<template>-mem:<tag>@sha256:...      # 单层 LSMT(memory)

S3（沿用现有 <manifest-digest>/ 目录，内容缩减）:
  <manifest-digest>/
  ├── vmstate.snap          # 保留
  ├── manifest.json         # 保留，schema 扩展（见下）
  └── SHA256SUMS            # 仅覆盖 S3 侧文件（或删除，见 Alternatives）
  index/<sha256(image-ref)>.json   # 保留，指向 manifest.json
```

不再发布到 S3 的文件：`rootfs.ext4`、`memory.snap`、`overlaybd/` 目录、
转换阶段的 `overlaybd-import-raw` 步骤整体移除（LSMT 封装由 streamingvolume
的 commit 流程完成）。

`manifest.json` schema 扩展：

```json
{
  "schemaVersion": 1,
  "sourceImage": "...",
  "kernel": {"name": "...", "digest": "..."},
  "compatibility": {"firecrackerVersion": "...", "hostKernel": "...", "cpuModel": "..."},
  "machine": {"vcpu": "1", "memory": "2Gi"},
  "guestNetwork": {"ip": "172.30.0.3", "...": "..."},
  "format": "oci",
  "artifacts": {
    "rootfs": {"ref": "registry/repo/t-rootfs:tag@sha256:..."},   # 新增
    "memory": {"ref": "registry/repo/t-mem:tag@sha256:..."},      # 新增
    "vmstate": {"key": "<manifest-digest>/vmstate.snap",
                "sha256": "...", "sizeBytes": 123}                # 原 files 迁移
  }
}
```

### 镜像格式

两个镜像均为 streamingvolume 标准 commit 产物（artifact manifest）：

```text
<template>-rootfs:<tag>
└── manifest (artifactType: application/vnd.alibaba.overlaybd.layer.zfile)
    ├── config: {}（空 JSON）
    └── layer[0]: LSMT blob
        mediaType: application/vnd.docker.image.rootfs.diff.tar
        annotations: OverlayBDBlobDigest / OverlayBDBlobSize / OverlayBDVersion
        内容 = rootfs.ext4 的裸块数据（字节级一致）
```

关键约束：

- **卷内容 = 裸块镜像**。Attach 出来的设备内容必须与快照时刻的 rootfs /
  memory 完全一致（guest 恢复依赖块内容不变），因此写入方式是 `dd` 裸写到
  设备，不是挂载文件系统后拷贝文件树（后者会重建 ext4，inode/UUID 漂移）。
- **一镜像一设备**。OverlayBD 镜像的层是同一个块设备的数据段栈；rootfs 与
  memory 是两个独立的块设备对象，必须两个镜像。vmstate 不是块设备，不参与。
- memory 卷的虚拟容量 = vm 内存大小；rootfs 卷 = rootfsSize。LSMT 只存已
  写入块：rootfs 稀疏空洞不占存储，memory 接近全量（zstd 压缩缓解）。

### builder 构建流程

构建阶段前置不变（pull OCI → oci2rootfs → 冷启动验证 → snapshot →
manifest 生成），打包与发布阶段替换：

```
1. 初始化 streamingvolume service（进程内，root = emptyDir /build/strmvol，
   首次启动执行 PurgeStale 清理遗留卷）
2. mem 镜像:
     Attach 空裸卷(memSize, writable)  → /dev/ublkN
     dd memory.snap → /dev/ublkN
     Detach → Commit(target-ref=<t>-mem:<tag>) → Push
3. rootfs 镜像: 同上，dd rootfs.ext4 → Commit/Push <t>-rootfs:<tag>
4. S3: 上传 vmstate.snap（现有链路）
5. S3: manifest.json 最后上传（保持「manifest 最后 = 提交点」语义）
6. Pod annotations 回写（现有机制），controller 更新 status
```

实现要点（来自 streamingvolume 的使用约束）：

- **单实例约束**：一个 root 只允许一个 service 实例持有（`.owner.lock`
  flock），builder 每次构建使用独立 emptyDir root，随 Pod 销毁。
- **Fetch 先于 Attach 仅对 registry ref 需要**；空卷 ref（`snapshot.volume.base`
  前缀，可选 `:<sizeGB>` 后缀）不需要 Fetch。
- `VirtualSize` 参数只对可写卷合法；readonly Attach 会被上游拒绝。
- Commit 与 Push 分离；相同内容重复 Commit 产出相同 digest（content-addressed，
  幂等）。
- builder 镜像需补齐 OverlayBD 完整工具链（`overlaybd-create/commit` 等，
  现有仅 `overlaybd-import-raw`）并内置 streamingvolume 依赖；build Pod
  hostPath 增加 `/dev/ublk-control`（ublk 需宿主机内核 ≥ 5.19，旧内核回退
  `/dev/loop-control` + `/dev/loop*` 的 loop 模式）。

### controller 与 status

`SandboxTemplate.status` 扩展：

```go
type SandboxTemplateStatus struct {
    // 现有字段
    Phase          string
    ArtifactDigest string   // manifest.json 的 sha256（不变，跨产物总 digest）
    ManifestRef    string   // S3 manifest.json URI（不变）
    // 新增
    RootfsImageRef string   // registry/repo/t-rootfs:tag@sha256:...
    MemoryImageRef string   // registry/repo/t-mem:tag@sha256:...
}
```

builder 通过 Pod annotations（`sandbox.fast.io/rootfs-image-ref` 等）自报，
controller 读取后统一更新 status。**status 更新即提交点**：三处产物
（两镜像 + S3）全部上传成功后 status 才指向新 digest 集；此前任何失败，
status 停留在旧一代，warm pool 继续消费旧产物。

`SandboxPool.spec.warmImages` 消费侧不感知细节，仍按 image ref 解析
（index → manifest → artifacts）。

### 节点消费链路

runtime-agent 的 pull 链改造为双通道：

```
image ref
  → S3 index/<sha256(image)>.json → manifest.json（digest 校验，现有逻辑）
  → [registry 通道] Fetch + Attach(-mem 镜像, readonly)  → /dev/ublkA
                    cp /dev/ublkA → <cache>/memory.snap（此时按需取段）
                    Detach
                    同法产出 <cache>/rootfs.img（来自 -rootfs 镜像）
  → [S3 通道]     vmstate.snap 下载（现有逻辑，digest 校验）
  → manifest.json 落盘（commit point，最后写，现有语义）
```

- **缓存布局不变**：`<StateRoot>/images/<sha256(image-ref)>/` 下仍是
  `rootfs.img / vmstate.snap / memory.snap / manifest.json`，driver 的
  reflink、jailer hardlink、`snapshot load` 全部零改动。
- **memory 当前为 COW 读**（Firecracker File backend 私有映射，脏页不回写
  文件，见 `internal/runtime/firecracker/launcher.go` 的共享快照隔离注释），
  因此缓存中的 memory.snap 可继续 hardlink 共享、reflink 语义不变；readonly
  Attach 拷出即可。
- streamingvolume service 同样以进程内嵌入 runtime-agent（或节点常驻
  daemon 共享 blob 缓存，二选一，默认嵌入 agent，部署面最小）。
- DART P2P 网关与 registry 通道互为补充：registry blob 下载可走节点侧
  HTTP 代理/P2P（streamingvolume 原生支持 `experimental.p2p`）。

### 原子性与一致性

- 每个镜像 digest 由 registry 内容寻址保证；vmstate/manifest 由 S3 侧
  digest（manifest.files / SHA256SUMS 收缩版）保证。
- 跨产物一致性由 `ArtifactDigest`（manifest.json 的 digest）承担：节点拉齐
  三者后校验 manifest 内记录的 ref/digest 与实际一致。
- 上传顺序固定：两镜像 → vmstate → manifest.json（最后）→ annotations →
  status。任何一步失败即整体失败，不留半代产物引用。

### 认证

- registry：新增 `spec.output.registrySecretRef`（docker config JSON，
  streamingvolume 的 `secret.type=dockerAuth` 直接消费）；与现有
  `publishSecretRef`（S3）并存。
- 节点侧：runtime-agent 的 registry 凭证经 SandboxPool / runtime 配置下发，
  复用 dockerAuth 格式。

## 设计依据

**为什么 vmstate 不进 OCI**：vmstate 是 MB 级、只读、restore 时一次性顺序
读入 Firecracker 进程内存的文件，S3 全量拉取已是最优；为它打包 OCI 需要
额外的 attach-文件卷-commit 流程，收益为零。它是 restore 关键路径上最小的
前置必需品，留在最简单、最可靠的通道上。

**为什么 memory 进 OCI 而 rootfs 不和 memory 合并**：OverlayBD 镜像的层
必须是同一块设备的数据段栈；rootfs 与 memory 是两个独立块设备对象（root
drive 与 memfile），合并即破坏「设备内容 = 快照内存 / 快照根盘」的恢复
契约。同时两者生命周期演进不同（memory 未来走直连按需加载）。

**为什么 memory 现阶段只读拷出也值得**：即使暂不启用按需加载，OCI 通道
已带来 zstd 压缩（内存快照压缩比可观）、blob 去重、P2P 与 registry 生态，
且后续 Mode 2 只需把「Attach 后 cp 出来」换成「Attach 直连当 memfile」，
发布格式不变。

## 演进路径：内存按需加载

结构上本方案是 Mode 2 的直接前置：

```
Phase 1（本方案）: Attach(-mem, ro) → cp 出 memory.snap → 现有 restore
Phase 2 (Mode 2):  Attach(-mem, rw) → /dev/ublkN 直接作为 mem_file_path
                   restore 缺页 → overlaybd 按需取段（registry/P2P）
                   写落本地 upper，不回源
                   rootfs 同法直连（root drive 支持块设备）
```

Mode 2 的前置验证（另行立项）：Firecracker `mem_file_path` 指向块设备的
mmap 恢复行为；jailer chroot 内暴露 ublk 设备节点；缺页风暴的 prefetch
（利用快照验证阶段的访问 trace 制作加速层）。

## Risks and Mitigations

| 风险 | 缓解 |
|------|------|
| registry 单 blob 上限 / 大内存镜像推送慢 | LSMT zstd 压缩；streamingvolume 并发推层 + 断点重试；确认内部 registry blob 上限（一般 ≥5Gi） |
| 宿主机内核无 ublk（<5.19） | OverlayBD loop 模式回退；build Pod 与节点均需纳入安装前置检查 |
| builder 引入内部 Go 模块依赖 | 走内部 Go 代理拉取 `gitlab.alibaba-inc.com/sbu/streamingvolume`，锁定版本 |
| 可变 tag 被覆盖导致引用漂移 | status 一律 digest-pin（`ref@sha256`），tag 仅为人读便利 |
| 双存储（S3 + registry）运维复杂度 | S3 侧收缩为小文件；两通道凭证/生命周期独立；产物 GC 沿用现有 S3 GC，registry 侧按 tag/digest 生命周期另行配置 |
| streamingvolume 行为变更（Fetch-before-Attach、空卷语法等版本约束） | 锁定模块版本；集成测试覆盖 Attach/Commit/Push 关键路径 |
| 拷出模式下 memory 全量读取仍慢 | Phase 1 可保留 DART/S3 旧通道做灰度回退；Mode 2 落地后消除 |

## Test Plan

- **单元**：manifest schema 扩展（artifacts.ref 序列化/校验）；status 字段
  回读；S3 pull 链裁剪后的文件集合断言。
- **集成（Kind）**：integration-env 增加 registry 容器；构建 → 双镜像推送 →
  S3 小文件 → warm pull → 缓存布局断言 → restore 冒烟。断言：镜像层携带
  OverlayBD 三标注；vmstate 走 S3；`overlaybd/` 目录与 rootfs.ext4/memory.snap
  不再出现在 S3。
- **E2E（真实 KVM）**：沿用 sandboxtemplate-e2e / firecracker-chain-e2e 骨架，
  全链路构建 → 发布 → 拉取 → restore → 隔离性（多实例共享缓存）验证。
- **失败注入**：镜像推送失败 / S3 上传失败 / 中途 Kill build Pod，断言
  status 不前进、无半代引用。

## Drawbacks

- 双存储系统并存：凭证、GC、监控两套（S3 侧显著收缩）。
- builder 与节点新增 OverlayBD/ublk 依赖与内核版本约束。
- streamingvolume 成为构建与拉取的关键路径依赖（内部模块，需锁版本治理）。

## Alternatives

1. **三镜像（vmstate+manifest 也打 OCI）**：被否决。为两个小文件引入
   attach-文件卷-commit 流程复杂度，且 vmstate 是关键路径最小前置品，
   S3 直拉更简单可靠。
2. **全部留 S3（现状）**：无压缩/P2P/按需加载，自建分发链持续维护成本。
3. **rootfs/memory 裸 blob 手搓 OCI manifest（oras-go 自写）**：重复实现
   streamingvolume 已有的 commit/push/retry/P2P 逻辑，违背「不自建」原则。
4. **vmstate 塞进 rootfs 或 mem 镜像**：破坏「一镜像一设备」契约（见
   设计依据），不可行。

## Upgrade & Migration Strategy

1. **Phase A（双写）**：builder 同时发布旧 S3 全量布局与新 OCI 布局；status
   增加 image refs。节点默认走旧链路，灰度按节点切换到新链路（feature
   gate）。
2. **Phase B（切换）**：全量节点走新链路；停止向 S3 发布 rootfs.ext4 /
   memory.snap / overlaybd 目录；S3 GC 清理存量大文件。
3. **Phase C（收敛）**：移除 builder 的 `overlaybd-import-raw` 与 S3 大文件
   上传代码、agent 的 S3 大文件下载分支；`spec.output.format` 字段标记
   deprecated（新布局下无意义）。
4. 回滚：任一阶段可退回上一阶段（Phase A/B 期间旧链路始终可用）。
