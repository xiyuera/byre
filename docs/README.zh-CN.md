# 文档索引

仓库根目录的 [README](../README.zh-CN.md) 是入口：byre 是什么、五分钟 Quick start，以及四种 provision
的区别。本页是从那之后的地图。

## 手册

| 文档 | 回答什么 |
|---|---|
| [handbook/architecture.md](handbook/architecture.md) | 各个部件怎么拼起来——身份、租约、池、后端、就绪判据——以及四种看起来显然的修法为什么都不成立 |
| [handbook/backends.md](handbook/backends.md) | 四种 provision 的细节：每个字段、就绪判据、失败行为，以及怎么写一个 `command` 后端 |
| [handbook/operations.md](handbook/operations.md) | 怎么跑：首次安装、不在 overlay 里的那条授权、host key 轮换、metrics、升级、卸载 |
| [handbook/troubleshooting.md](handbook/troubleshooting.md) | 每种故障在客户端和网关日志里分别长什么样，先查什么 |

## 决策与记录

| 文档 | 回答什么 |
|---|---|
| [adr/](adr/) | 每个决策，以及“什么条件下会反转它” |
| [acceptance/](acceptance/) | 真实集群上的验收记录（命令 + 输出） |
| [roadmap.md](roadmap.md) | v1 明确不做什么，以及它们按什么顺序到来 |
| [release.md](release.md) | 什么时候发版、流水线产出什么 |
| [integrations/](integrations/) | 宿主运行时（agent 平台、CI）如何对接网关 |
| [rename-to-byre.md](rename-to-byre.md) | 改名历史、碰撞证据，以及尚未执行的迁移 |
| [i18n.md](i18n.md) | 哪些页面有翻译，以及检查器凭什么判定一个翻译过期 |

读代码从 `handbook/architecture.md` 开始；怎么跑从 `handbook/operations.md` 开始；已经在跑但不对，
从 `handbook/troubleshooting.md` 开始。
