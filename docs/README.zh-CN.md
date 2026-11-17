# 文档索引

英文文档是权威版本；本页与各 `*.zh-CN.md` 是同步翻译，规则见 [i18n.md](i18n.md)。

| 文档 | 回答什么 |
|---|---|
| [architecture.md](architecture.md) | 身份、租约、池、后端、就绪判据怎么拼起来 |
| [operations.md](operations.md) | 怎么跑：配置、首次安装、不在 overlay 里的那条授权、轮换、症状排查 |
| [release.md](release.md) | 什么时候发版、流水线产出什么、GitHub 怎么被镜像 |
| [roadmap.md](roadmap.md) | v1 明确不做什么，以及它们按什么顺序到来 |
| [adr/](adr/) | 每个决策，以及"什么条件下会反转它" |
| [acceptance/](acceptance/) | 真实集群上的验收记录（命令 + 输出要点） |
| [integrations/](integrations/) | 宿主运行时（agent 平台、CI）如何对接网关 |
| [rename-to-byre.md](rename-to-byre.md) | 改名历史、碰撞证据，以及尚未执行的下线/迁移 |

读代码从 `architecture.md` 开始；跑起来从 `operations.md` 开始。
