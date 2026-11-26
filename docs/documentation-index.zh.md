# 文档索引

站点[首页](index.md)是入口：byre 是什么、五分钟快速开始，以及四种 provision 的区别。本页是从那之后的地图。

## 手册

| 文档 | 回答什么 |
|---|---|
| [handbook/architecture.md](handbook/architecture.md) | 各个部件怎么拼起来——identity、lease、池、backend、就绪判据——以及四种看起来显然的修法为什么都不成立 |
| [handbook/backends.md](handbook/backends.md) | 四种 provision 的细节：该选哪种、每个字段、就绪判据、失败行为，以及 `command` 的契约 |
| [handbook/operations.md](handbook/operations.md) | 怎么跑：首次安装、不在 overlay 里的那条授权、host key 轮换、metrics、命令行参数、升级、卸载 |
| [handbook/troubleshooting.md](handbook/troubleshooting.md) | 每种故障在客户端和网关日志里分别长什么样，先查什么 |

## 可直接跑的示例

这些是仓库里的文件，本站不为它们单独出页面，所以下面每个链接都会把你带到 GitHub。

| 示例 | 形态 | 是什么 |
|---|---|---|
| [examples/local-quickstart/](https://github.com/xiyuera/byre/blob/main/examples/local-quickstart/README.zh-CN.md)（在仓库里，有中文版） | `endpoints` | 快速开始落成文件：一台机器，不需要集群 |
| [examples/static-endpoints/](https://github.com/xiyuera/byre/blob/main/examples/static-endpoints/README.md)（在仓库里） | `endpoints` | 一组固定的实验机 |
| [examples/pod-image/](https://github.com/xiyuera/byre/blob/main/examples/pod-image/README.md)（在仓库里） | `workloads` | 一个最小的 sshd 镜像，以及据此建 pod 的 backend |
| [examples/kubernetes-sandbox/](https://github.com/xiyuera/byre/blob/main/examples/kubernetes-sandbox/README.md)（在仓库里） | `claims` | 一个模板、一个热池，以及从池里领 instance 的 backend |
| [examples/docker-sandbox/](https://github.com/xiyuera/byre/blob/main/examples/docker-sandbox/README.md)（在仓库里） | `command` | 整个网关一个进程，sandbox 是 docker 容器——完全不需要集群 |
| [charts/byre/](https://github.com/xiyuera/byre/blob/main/charts/byre/README.md)（在仓库里） | `claims` | Kubernetes 形态打进一个 Helm chart，含模板与热池 |

## 决策与记录

| 文档 | 回答什么 |
|---|---|
| [adr/](adr/index.md) | 每个决策，以及「什么条件下会反转它」 |
| [acceptance/](acceptance/index.md) | 真实集群上的验收记录（命令 + 输出） |
| [roadmap.md](roadmap.md) | v1 明确不做什么，以及它们按什么顺序到来 |
| [release.md](release.md) | 什么时候发版、流水线产出什么 |
| [tag-map.md](tag-map.md) | 历史重写后每个 tag 指向哪里，以及两个不再存在的已发布 commit hash |
| [contributing-from-agents.md](contributing-from-agents.md) | agent 贡献者的一角色一账号模型、凭据处理与 token 纪律 |
| [integrations/](integrations/README.md) | 宿主运行时（agent 平台、CI）如何对接网关 |
| [rename-to-byre.md](rename-to-byre.md) | 改名历史、碰撞证据，以及尚未执行的迁移 |
| [i18n.md](i18n.md) | 哪些页面有翻译，以及检查器凭什么判定一个翻译过期 |

读代码从 `handbook/architecture.md` 开始；怎么跑从 `handbook/operations.md` 开始；已经在跑但不对，从 `handbook/troubleshooting.md` 开始。

`handbook/backends.md`、`handbook/operations.md`、`handbook/troubleshooting.md`、`release.md`、`i18n.md`、`integrations/` 以及五个示例里的四个目前只有英文——这些页面在中文站里回落到英文原文，不是 404；[`i18n.md`](i18n.md) 是清单，`scripts/check-i18n.sh` 让这件事一直可见。
