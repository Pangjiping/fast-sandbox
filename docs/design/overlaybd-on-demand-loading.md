# OverlayBD 按需加载消费侧设计

> 文档类型：设计
>
> 日期：2026-08-24
>
> 范围：SandboxTemplate（OSEP-0023）产物（OverlayBD LSMT commit layer）的**消费侧**——
> 在节点上把 layer 挂成块设备并按需从远端加载。生产侧（转换/打包）见 OSEP-0023。

## 1. 背景与目标

SandboxTemplate 的 `package` 阶段把 `rootfs.ext4` 与 `memory.snap` 转换为
OverlayBD LSMT commit layer（`layer.lsmt`，索引 + 数据段，uncompressed）。
转换只是"准备好了数据面"，真正实现按需加载需要消费侧两块能力：

1. **OverlayBD 运行时设备挂载**：把 layer 挂成只读块设备，Firecracker 的
   virtio-blk / file-backed memory 指向它；
2. **远端 range read**：guest 读 block → 设备 miss → 按 digest+offset 从
   远端（S3/OSS/Registry）拉数据段，落本地 bounded cache。

本设计给出组件清单、来源与抽取边界。参考实现为
[kvcache-ai/AgentENV](https://github.com/kvcache-ai/AgentENV)（MIT，
生产运行于大规模 agent 环境：150 万镜像按需加载、ublk 设备、
快照 <100ms、共享宿主 page cache）。

## 2. 按需加载链路

```
guest 读 block
  → virtio-blk / file-backed memory
  → ublk 设备（OverlaybdTarget）
  → overlaybd ImageFile（上游库：layer 索引 + 本地 block cache）
  → cache miss → 远端 repo range read（digest+offset）
  → 数据段落 cache → 返回
```

eager 的只有 manifest / vmstate（小）；rootfs 与 memory 的大数据段全部 lazy。
缓存层次：guest page cache → OverlayBD block cache → 节点共享只读设备
（多 sandbox 共享同一 layer 设备与宿主 page cache）→ S3/OSS/Registry。

## 3. 组件清单与来源

| 层 | 组件 | 来源 | 说明 |
|----|------|------|------|
| 上游库 | `containerd/overlaybd`（`ImageFile` / `ImageService`） | **直接依赖** | range read、层读取、本地 block cache 都在这里实现；AgentENV 同样直接使用 |
| 设备挂载 | `storage/ublk` crate（tokio + io_uring ublk server）+ `overlaybd_target.rs`（`UVMUblkTarget` impl） | **直接依赖（MIT）或参考重写** | AgentENV 已解耦为独立 workspace member，仅依赖 overlaybd + tokio/io_uring；`OverlaybdTarget` 把块 I/O 转成 `ImageFile` 读 |
| 服务化 | `storage/ublk-daemon`（RPC 创建/删除设备） | 参考 | runtime-agent 内做设备生命周期管理 |
| 缓存编排 | `src/image/cache/*`（graph/gc/store/service + 并发锁） | 参考结构 | bounded cache、容量治理、GC |
| 快照存储 | `src/snapshot/repository/backends/oss`（`object-store-operator`） | 参考 | S3 兼容客户端（含凭证刷新）；overlaybd repo 统一 `s3://` scheme |
| 远端 repo | overlaybd global config | 配置 | 指向 S3/OSS/Registry，按 range 访问 |

**不采用的组件（AgentENV 平台面）**：orchestrator / scheduler / gateway /
API / 租户 / 密钥管理 / envd / warm-pool / uffd-core（E2B 备选路径）。
这些与 OpenSandbox 控制面构成双控制面，不在数据面抽取范围。

## 4. 抽取边界

- **必须自研**：runtime-agent 的设备编排（对应 ublk-daemon 职责）、缓存
  容量管理（参考 `src/image/cache`）、快照对接（复用 OpenSandbox
  `SnapshotService` 语义）、manifest/layer 引用与 GC。
- **可直接依赖**：overlaybd 上游库、AgentENV `storage/ublk`（MIT，cargo
  依赖即可；若 license/维护策略不允许，参考其 ~400 行 `overlaybd_target.rs`
  重写成本可控）。
- **固定版本**：overlaybd、ublk 相关 crate 按 commit 锁定，独立 E2E 保障
  升级可验证。

## 5. 落地路径与验收

```
runtime-agent
  ├─ 依赖：containerd/overlaybd + storage/ublk
  ├─ 自研：设备编排、缓存管理、快照对接
  └─ repo 配置：s3://（S3/OSS/registry 兼容）
```

验收（对应按需加载方案 Phase 0）：

- 空缓存恢复 4 GiB memory + 9 GiB rootfs；
- MinIO/对象存储观测只有 Range Read，下载字节显著小于 logical size；
- memory/rootfs LSMT layer 与源文件 byte-for-byte 一致；
- 多 sandbox 共享只读设备，宿主 page cache 复用；
- 远端故障注入：无 silent corruption、已缓存 VM 可继续、未缓存块明确
  失败而非 stall 伪装正常。

## 6. 与 SandboxTemplate 的衔接

SandboxTemplate `package` 阶段产物（`layer.lsmt`）即 overlaybd `ImageFile`
直接消费的 commit layer——模板负责"造"，runtime-agent + storage/ublk 负责
"用"，两端在 overlaybd 格式上闭合。产物 manifest（digest、logicalSize、
layers）即消费侧缓存 key 与校验依据。
