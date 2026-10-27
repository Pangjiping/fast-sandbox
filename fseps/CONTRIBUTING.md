---
title: FSEP 贡献指南
description: 先讨论、按需写提案，重点评审兼容性、测试和回滚。
---

# FSEP 贡献指南

FSEP（Fast Sandbox Enhancement Proposal）记录需要共同确认的重大设计。流程借鉴 [Go Proposal Process](https://github.com/golang/proposal) 的先讨论、按需写设计，以及 [Kubernetes KEP 模板](https://github.com/kubernetes/enhancements/blob/master/keps/NNNN-kep-template/README.md) 对兼容性、测试和回滚的关注；目录与编号方式沿用 [OpenSandbox OSEP](https://github.com/opensandbox-group/OpenSandbox/tree/main/oseps)。

## 什么时候需要

重大功能、跨组件架构调整，以及影响公共接口、核心生命周期或安全隔离的变更，先开 Issue，用几句话说明问题、拟议方案和影响。维护者与作者在 Issue 中判断是否需要 FSEP：涉及复杂取舍、兼容性或跨组件协调时，再写完整提案；Issue 已足以说明的变更可直接进入实现 PR，并保留决策理由。

小型缺陷修复、文档修正、内部重构和兼容的局部改进直接提交 PR，无需额外 Issue 或 FSEP。已有充分讨论的重大设计也可以直接提交 FSEP PR，不必补开 Issue。

## 最小流程

1. **讨论与设计**：按需使用 [模板](fsep-template.md.template) 创建 FSEP，提交提案和 [索引](README.md) 更新的 PR，关联已有讨论。内容以能支持决策为准，不要求篇幅；额外细节按需补充。
2. **评审与决定**：受影响组件的维护者在 PR 中确认方案，重点检查下表三项。批准后设为 `accepted`；不采纳或撤回时设为 `closed`，在正文保留原因。合入 `draft` 仅表示保存草稿，不表示设计获批。
3. **实现与完成**：实现 PR 关联 FSEP，进度直接在 Issue 或 PR 中跟踪。约定范围、测试和相关 `docs/` 文档完成后，在实现 PR 中更新为 `implemented`。获批不代表已排期，落地不代表已进入 GA。

| 必须说明 | 最低要求 |
|----------|----------|
| 兼容性 | 对现有 API、CRD、SDK、CLI、配置及已有资源或快照有何影响；涉及组件混合版本时说明兼容范围。破坏性变更需明确列出影响和迁移路径，并获得维护者明确同意。 |
| 测试 | 列出可执行的验证方法和验收标准，按风险覆盖正常路径、失败恢复及兼容性。涉及性能时注明测量边界、环境和缓存状态；仅选择相关的测试层级。 |
| 发布与回滚 | 如何启用或分步上线、观察哪些信号、何时回滚、怎样禁用或降级；说明已有资源、数据或快照在回滚后如何处理。无法安全回滚时，写明限制和恢复方案。 |

三项均保留；不适用时用一句话说明原因。普通维护者 PR 评审即可，不另设委员会、固定等待期或发布审批表。重大方案变化需重新评审，并同步更新提案和索引。

## 状态

通常为 `draft` → `accepted` → `implemented`，未落地的提案也可转为 `closed`。不单设方向认可、待实现或实现中等中间状态。

| 状态 | 含义 |
|------|------|
| `draft` | 正在讨论，尚未批准。 |
| `accepted` | 维护者已批准设计，可以实现；进度见关联 Issue 或 PR。 |
| `implemented` | 约定范围、测试和相关用户文档已完成。 |
| `closed` | 不采纳、撤回或停止推进，正文记录原因及替代提案链接（如有）。 |

## 创建与维护

在仓库根目录运行：

```bash
./fseps/init-fsep.sh "Snapshot Distribution"
./fseps/init-fsep.sh --author "@username" --slug snapshot-distribution "快照分发改进"
./fseps/init-fsep.sh --help
```

脚本填入元数据，默认状态为 `draft`，在 `fseps/` 中按最大编号生成下一个 `NNNN-english-slug.md`。中文等非 ASCII 标题需显式提供英文 `--slug`。默认作者取仓库 Git 的 `user.name`，其次取 `user.email`，最后取当前用户名；脚本可从任意工作目录调用。

新增提案时更新索引；内容或状态变化时同步更新 `last-updated` 和索引。并行 PR 编号冲突由后合入者调整，已合入的编号不复用，关闭的提案也保留。

`fseps/` 保存计划和决策，`docs/` 描述当前产品行为，实现时同步更新相关文档并从提案链接过去。临时笔记仍放在 Git 忽略的 `.dev/`，正式讨论不能依赖其中的文件。涉及 OpenSandbox 集成时可关联 OSEP，各仓库分别评审自己的变更。
