# Firecracker clone 网络：per-clone netns 数据面改造

> 文档类型：技术方案（设计）
>
> 日期：2026-08-28
>
> 状态：草案（提交评审）
>
> 关联：opensandbox-group/fast-sandbox#26（restore 网络模型与 slot DNAT 漂移）；
> [OSEP-0022](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0022-multi-sandbox-egress-control-plane.md)
> （多沙箱 egress 控制面，source-IP dispatch 前置假设）。
>
> 配套：[firecracker-on-demand-loading.md](firecracker-on-demand-loading.md)
> 的启动路径/数据面设计（本方案是其网络数据面的改造）。

---

- [问题](#问题)
- [社区调研结论](#社区调研结论)
- [方案评估](#方案评估)
- [总体设计：per-clone netns（方案 2）](#总体设计per-clone-netns方案-2)
- [与其他 runtime 的兼容性](#与其他-runtime-的兼容性)
- [与 OSEP-0022（多沙箱 egress 控制面）的兼容性](#与-osep-0022多沙箱-egress-控制面的兼容性)
- [改动面清单](#改动面清单)
- [测试计划](#测试计划)
- [风险与已知问题](#风险与已知问题)
- [待决策项](#待决策项)

## 问题

golden snapshot restore 成为唯一启动路径后（PR #25），guest 网络身份
（IP/MAC）**baked 在快照里**（v1.16 restore 从 vmstate 恢复 NIC 与 guest
网络栈），而 fastlet slot 数据面按 `slot.IP + 1` 推导 guest 地址并安装
host 侧 DNAT。两个模型只在第一个 slot 巧合对齐：

- **多实例 / 非首 slot**：host DNAT 目标 `slot.IP+1` 与 baked guest IP
  错位 → ingress 不可达；
- **共享 baked MAC/IP 挂同一桥**：并发 restore 多个 clone 时 ARP 冲突
  （E2E `TestFirecrackerDriverE2EConcurrent` 被迫放弃 per-instance
  reachability 断言）；
- **fastlet-proxy ingress**（`AccessKindDirectIP` dial slot.IP 依赖 DNAT）
  对每个非首实例失效。

## 社区调研结论

Firecracker 官方 clone 网络模型（`docs/snapshotting/network-for-clones.md`，
v1.16.1）是**唯一被验证的路径**：

1. 每个 clone 的 **firecracker 进程 + tap 运行在独立 netns**（`--netns`
   / jailer 参数）——tap 同名无冲突（netns 隔离），**同 guest MAC/IP 无
   ARP 冲突**（ARP 域按 netns 隔离）；
2. netns 内 veth 接 host，**NAT 解决"同 guest IP"**（netns 内 DNAT/
   MASQUERADE）；
3. 官方明确：**共享桥不隔离的方案不可行**（ARP 冲突）；v1.16 restore
   不热插拔 NIC，guest 侧重配（udev 注入）不触发。

## 方案评估

| 方向 | 结论 | 理由 |
|------|------|------|
| 1. 对齐 DNAT 到 baked guest IP（数据面最小改） | **否决** | 只解决首 slot 巧合；共享 IP/MAC 仍挂同一桥，ARP 冲突无解；egress source-IP dispatch 仍不可区分 |
| 2. **per-clone netns**（上游模型） | **采用** | 官方唯一验证路径；fastlet slot netns 即现成的 per-clone 隔离域（见下） |
| 3. restore 后 guest 侧重配网络 | **否决** | v1.16 restore 不重触发 NIC 热插拔，无 guest 侧机制 |

## 总体设计：per-clone netns（方案 2）

**关键洞察**：fastlet 的 slot netns（`LinuxNetNSDriver` 每 sandbox 一个
`fsb<hex>`）就是现成的 per-clone 隔离域——container runtime 本来就是
"进程进 slot netns"。firecracker 从"host 侧桥接 tap + host DNAT"改为
"进程经 **jailer `--netns`** 进 slot netns + netns 内数据面"，与 container
同构。

> **载体确认（真机验证，v1.16.1）**：`firecracker --help` 完整输出确认
> firecracker 二进制**没有** `--netns` flag——上游文档
> （network-for-clones.md）的 `--netns` 是 **jailer** 参数
> （"using the `--netns` jailer parameter"）。因此进程进 netns 的唯一官方
> 路径是 jailer：
>
> ```text
> jailer --id <id> --netns <slotNetNSPath> --uid 0 --gid 0 \
>        --chroot-base <StateRoot>/jails --exec-file <firecracker> \
>        -- <fireccker 参数>
> ```
>
> 顺带激活设计里预留的 `FirecrackerConfig.JailerPath` 字段（chroot 隔离
> 的生产安全收益一并拿到）。jailer 路径语义见"jailer 适配"小节。

```text
改造前（现状）                       改造后（per-clone netns）
┌─ host netns ───────────────┐    ┌─ slot netns（已存在）────────────┐
│ bridge fsb0                │    │ eth0 = slot.IP（172.30.0.2/24）   │
│   ├─ hostVeth ── netns     │    │   ├─ 默认路由 via 桥网关（不变）  │
│   ├─ tap fc<hex> ── VM     │    │   ├─ OUTPUT 隔离规则（不变）      │
│ DNAT slot.IP→slot+1 (host) │    │   ├─ vmtap0（netns 内创建）── VM  │
│ firecracker 进程在 host     │    │   ├─ PREROUTING DNAT slot.IP→guest│
│ (guest MAC/IP 挂桥共享)     │    │   ├─ POSTROUTING MASQ guest→slot.IP│
└────────────────────────────┘    │   └─ FORWARD 隔离（兄弟禁止）     │
                                  │ jailer --netns <slotNetNSPath>    │
                                  └───────────────────────────────────┘
```

### jailer 适配（新）

- **启动**：launcher 改为 `jailer --id <id> --netns <slotNetNSPath>
  --uid 0 --gid 0 --exec-file <firecracker> --chroot-base-dir
  <StateRoot>/jails -- <firecracker 参数>`（参数含 `--api-sock`；`--id`
  由 jailer 自动传递，**不得重复传入**）；
- **chroot 目录结构（真机验证，v1.16.1）**：
  `<chroot-base-dir>/<exec-file basename>/<id>/root/<路径>`——即
  `<StateRoot>/jails/firecracker/<id>/root/`。firecracker 在 chroot 内
  创建的 API socket/日志文件，宿主侧为上述路径前缀——driver 的
  `state.APIAddress` 按此转换（`firecracker/` 中间目录来自 exec-file
  basename，不是固定值）；
- **cwd 语义**：jailer chroot 后进程 cwd 在 chroot 内（`/`）——restore
  依赖的"相对 rootfs.img 经 cwd 解析"需要改为**绝对路径**（jailer 下
  相对路径语义不可靠）：快照 bake 的驱动路径改为 `<chroot>/rootfs.img`
  的绝对形式，或 driver 在 chroot 内准备实例副本（见风险/待决策）；
- **uid/gid**：`--uid 0 --gid 0`（Fastlet 特权容器）或专用 uid（生产
  加固，后续）；
- **NodeJanitor 匹配**：jailer 的 `--id` 与 firecracker 的 `--id` 一致，
  residual-process 匹配（`--id <sandboxID[:32]>`）保持成立（匹配参数
  含 jailer 进程本身）；
- **验证项（真机，施工前）**：jailer `--netns` 完整行为——API socket
  路径转换、chroot 内 cwd、netns 内 tap 打开、`--uid/--gid 0` 运行。

### 数据面语义（保持不变的部分）

- `AccessDescriptor` 仍是 slot.IP（ingress dial slot.IP，fastlet-proxy
  语义不变）；
- guest 仍持有 baked 地址（172.30.0.3，来自快照）；
- `network_overrides` 仍只换 tap 名（改为 netns 内固定名 `vmtap0`）；
- 出网仍走桥 + host POSTROUTING MASQUERADE；
- builder / 快照产物 / agent 零改动。

### 新增/移动的数据面

1. **tap 进 netns**：`GuestVMNetNSDriver.Prepare` 改为
   `ip netns exec <ns> ip tuntap add dev vmtap0 mode tap` +
   `ip -n <ns> addr add <guestIP>/32 dev vmtap0` + up（不再挂 host 桥）；
2. **netns 内 PREROUTING DNAT**：`slot.IP → guestIP`（保留现状 DNAT
   语义，从 host 移入 netns）+ FORWARD ACCEPT（guest 转发）+
   `net.ipv4.ip_forward=1`（netns 内）；
3. **netns 内 POSTROUTING MASQUERADE（必做，egress 兼容）**：
   guest 出网 src=baked 共享 IP → 出 netns 前 MASQUERADE 成 slot.IP——
   使 host forward 点 source IP 每实例唯一（对齐 OSEP-0022 dispatch
   前置，见下节）；
4. **netns 内 FORWARD 兄弟隔离**：`-d privateCIDR REJECT`（guest 转发
   流量不经 OUTPUT 链，现状 OUTPUT 隔离规则对 firecracker 模式不生效
   ——顺带补上现状 firecracker 在 host 桥上的隔离缺口）；
5. **launcher**：firecracker 进程加 `--netns <slotNetNSPath>`
   （v1.16 原生 flag，需参考机 `--help` 验证；不可用则走 jailer
   `--netns`）。netns 只隔离网络：API socket / cwd / 日志路径不受影响。

### 每实例与共享

| 项 | 归属 |
|----|------|
| netns（fsb<hex>）、eth0=slot.IP、vmtap0、DNAT/MASQUERADE/FORWARD 规则 | per-instance（现有 slot 生命周期） |
| guest IP（172.30.0.3）、guest MAC | 共享（baked，netns 内安全） |
| 桥 fsb0、host MASQUERADE、hostVeth | 共享（现有） |

## 与其他 runtime 的兼容性

| Runtime | 数据面 | 影响 |
|---------|--------|------|
| container / gvisor | 进程进 slot netns，eth0=slot.IP | **零影响**——`LinuxNetNSDriver` 不动；firecracker 新增的 tap/DNAT/MASQ 只在 firecracker 模式存在 |
| kata（GuestNetNS） | slot netns 挂给 guest NIC | **零影响**——netns 结构不变 |
| boxlite | gvproxy 隧道 | 零影响 |
| firecracker | host 桥接 → netns 内数据面 | 与 container 同构，slot netns 成为统一数据面边界 |

改动收敛在 `GuestVMNetNSDriver`（firecracker 专用扩展）+ launcher，不碰
`LinuxNetNSDriver`（container/kata 路径零改动）。

## 与 OSEP-0022（多沙箱 egress 控制面）的兼容性

OSEP-0022 的核心前置假设：**egress 按 source IP 区分 subject**
（`ip saddr` dispatch；`SubjectKey = NetNSPath + SourceIP`；enforcement 在
Pod netns `hook forward`，"MASQUERADE happens at POSTROUTING, source IP
intact"）。container 模式天然成立（src=slot.IP）；**现状 firecracker 破坏
它**（guest 出网 src=共享 baked IP，host forward 处不可区分，与 bwrap
"无自有 IP"问题同构）。

**方案 2 + netns 内 MASQUERADE 恰好维持该不变式**：

| OSEP-0022 依赖 | 方案 2 下 | 结论 |
|---|---|---|
| dispatch key = source IP | netns MASQUERADE 后 = slot.IP，每实例唯一 | ✅ 需配套（本方案必做项 3） |
| slot store 字段（ip/netns/hostVeth/gateway/privateCidr/dnsPath） | 全部不变 | ✅ |
| hostVeth `iifname` 防 spoofing | 入口接口仍为 hostVeth→桥路径 | ✅ |
| Pod netns `hook forward` 主 enforcement | firecracker 流量同样过 forward（src=slot.IP） | ✅ |
| per-sandbox netns OUTPUT（防御） | 对 firecracker 空转（guest 走 FORWARD）——不冲突，覆盖由 netns FORWARD 补上 | ⚠️ 记为设计差异 |
| DNS proxy 绑 `<gateway>:53` + resolv.conf 重写 | guest DNS 流量 MASQUERADE 后 src=slot.IP → REDIRECT 正常 | ✅ |
| Kata（TAP 同 forward surface） | 不受影响 | ✅ |

egress 集成测试应包含 firecracker runtime 用例（N sandbox 不同 policy
在一 Pod 内，其中含 firecracker 模式），验证 source-IP dispatch 对
clone 同样工作。

## 改动面清单

| 文件 | 改动 |
|------|------|
| `internal/fastlet/network/guest_vm_linux_driver.go` | Prepare/Destroy：tap 移入 netns（固定名 `vmtap0`，不再挂桥）；netns 内 DNAT/FORWARD/MASQUERADE/ip_forward 规则安装与清理（含 Validate 更新） |
| `internal/runtime/firecracker/launcher.go` | 改用 **jailer** 启动（`--netns <slotNetNSPath>` + `--chroot-base` + `--uid/--gid`），激活 `JailerPath`/`ChrootBase` 字段；slot 的 `NetNSPath` 传入 launchConfig |
| `internal/runtime/firecracker/driver.go` | `configureRestoreVM` 的 `network_overrides` tap 名 → `vmtap0`；`state.APIAddress` 的 **chroot 路径转换**（宿主侧 `<chroot-base>/<id>/root/<path>`）；rootfs 驱动路径改为 chroot 内绝对路径 |
| `internal/runtime/firecracker/e2e_test.go` | 拓扑更新（tap 不再挂桥）；**Concurrent 用例恢复 per-instance reachability 断言**（issue 核心验收） |
| `scripts/firecracker-e2e.sh` / `firecracker-chain-e2e.sh` | 清理逻辑（netns 内 tap 随 netns 删除自动消失；jailer chroot 目录清理）、断言更新 |
| `docs/guides/firecracker-runtime-e2e.md` | 网络拓扑章节重写（netns 内数据面 + jailer） |

## 测试计划

- **单元（guest_vm_linux_driver）**：Prepare 命令序列（netns 内 tuntap/
  addr/route、DNAT/MASQ/FORWARD 规则存在性）、Destroy 清理、Validate
  （netns 内 tap 存在性）；
- **单机集成**：单实例 restore → host ping slot.IP 可达（ingress 路径
  走 netns DNAT）；guest 出网（MASQUERADE 后 src=slot.IP）；
- **E2E Concurrent（核心）**：5 VM 从同一快照集并发 restore →
  **每实例 per-slot reachability 断言恢复**（ARP 无冲突、DNAT 各自
  正确）；
- **egress 兼容（与 OSEP-0022 对齐）**：host forward 点观察 firecracker
  流量 src=slot.IP（`nft trace` 或 iptables 计数）；netns FORWARD 兄弟
  隔离生效；
- **回归**：container/gvisor/kata 的既有网络测试全绿（`LinuxNetNSDriver`
  零改动保证）。

## 风险与已知问题

| 风险 | 影响 | 缓解 |
|------|------|------|
| jailer `--netns` 的路径/chroot 语义（API socket 转换、cwd） | 方案基础 | 施工前真机验证（验证项 2）；cwd 依赖改为 chroot 内绝对路径 |
| jailer uid/gid 与特权容器、日志路径 | 启动细节 | `--uid 0 --gid 0` 起步，专用 uid 后续加固 |
| netns 内 DNAT/conntrack 语义差异 | ingress 行为 | 单机集成先行验证；与现有 host DNAT 行为对比 |
| netns 删除时 tap/规则残留 | 资源泄漏 | Destroy 顺序（先删 tap/规则再删 netns）+ E2E 清理断言 |
| egress 集成未含 firecracker 用例 | OSEP-0022 覆盖缺口 | egress 测试计划补充（见上） |
| 共享 guest MAC 的跨 clone 影响面缩小后仍有 DNS/应用层身份共享 | 产品语义 | 接受（clone 模型固有），文档记录 |

## 验证项（真机，施工前）

1. ~~firecracker `--netns` 可用性~~ **已验证（v1.16.1）：不支持**——载体现
   状为 jailer `--netns`（上游官方路径）；
2. **jailer `--netns` 完整行为**（施工前必做）：`jailer --id x --netns
   /var/run/netns/fctest --uid 0 --gid 0 --chroot-base <dir> --exec-file
   <firecracker> -- --api-sock /api.sock ...` 启动后断言：
   - 进程 netns inode == 目标 netns；
   - 宿主侧 socket 出现在 `<chroot-base>/x/root/api.sock`；
   - netns 内 tap（先创建）能被 firecracker 打开（`network_overrides`
     或启动参数引用）；
   - NodeJanitor 参数匹配（`--id` 可见性）。

## 待决策项

1. **netns 内 MASQUERADE 的粒度**：仅 firecracker 模式启用（推荐，按
   `GuestVMNetNSDriver` 是否配置）vs 统一开启；
2. **tap 命名**：固定 `vmtap0`（简单，netns 内无冲突）vs 沿用现有
   `GuestTap` 字段（slot 生命周期管理）；
3. **与 OSEP-0022 的联调归属**：egress 集成测试由 egress 侧补 firecracker
   用例（推荐）vs 本方案同步补（需跨仓库协作）。
