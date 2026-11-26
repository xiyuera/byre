# 文档索引

站点[首页](index.md)是入口：byre 是什么、五分钟快速开始，以及四种 provision 的区别。本页是从那之后的地图。

## 开始使用

| 页面 | 回答什么 |
|---|---|
| [快速开始](getting-started/quickstart.md) | 五分钟装出一个能用的网关 |
| [选择 backend](getting-started/backends.md) | 四种 provision 形态里哪种适合你的机器 |
| [Kubernetes](getting-started/kubernetes.md) | 按需创建 sandbox，端到端 |

## 手册

| 页面 | 回答什么 |
|---|---|
| [架构](handbook/architecture.md) | 各个部件怎么拼起来——identity、lease、池、backend、就绪判据——以及四种看起来显然的修法为什么都不成立 |
| [backend 详解](handbook/backends.md) | 四种 provision 的细节：该选哪种、每个字段、就绪判据、失败行为，以及 `command` 的契约 |
| [运维](handbook/operations.md) | 怎么跑：首次安装、不在 overlay 里的那条授权、host key 轮换、metrics、命令行参数、升级、卸载 |
| [排错](handbook/troubleshooting.md) | 每种故障在客户端和网关日志里分别长什么样，先查什么 |

## 示例

| 示例 | 形态 | 是什么 |
|---|---|---|
| [静态 endpoints](examples/static-endpoints.md) | `endpoints` | 一组固定的实验机，一次租给一个 identity |
| [本机快速开始](examples/local-quickstart.md) | `endpoints` | 快速开始落成文件：一台机器，不需要集群 |
| [任何会 SSH 的镜像](examples/pod-image.md) | `workloads` | 一个最小的 sshd 镜像，以及据此建 pod 的 backend |
| [Kubernetes starter kit](examples/kubernetes-sandbox.md) | `claims` | 一个模板、一个热池、从池里领 instance 的 backend，以及跑它所需的 RBAC 与部署 |
| [Docker starter kit](examples/docker-sandbox.md) | `command` | 整个网关一个进程，sandbox 是 docker 容器——完全不需要集群 |
| [Helm chart](examples/helm-chart.md) | `claims` | Kubernetes 形态打进一个 chart，含 CRD 与热池 |

## 设计决策

| 页面 | 回答什么 |
|---|---|
| [决策记录](adr/index.md) | 每个决策，以及「什么条件下会反转它」 |
| [路线图](roadmap.md) | v1 明确不做什么，以及它们按什么顺序到来 |
| [限制](limits.md) | 已知做错的、已知不做的，直说 |
| [集成](integrations/README.md) | 宿主运行时（agent 平台、CI）如何对接网关 |

## 参与贡献

[参与贡献](contributing.md) 讲分支与合并：提 PR 之前该跑什么，发布流程对一个改动期待什么。

读代码从[架构](handbook/architecture.md)开始；怎么跑从[运维](handbook/operations.md)开始；
已经在跑但不对，从[排错](handbook/troubleshooting.md)开始。

## 翻译状态

首页、快速开始、Kubernetes、架构、限制、为什么不是跳板机以及本页在 `/zh/` 下有中文版。其余页面回落
英文原文，不是 404——一个页面只存在于一种语言时，另一种语言里不会出现坏链接。
