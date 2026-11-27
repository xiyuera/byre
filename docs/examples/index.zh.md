---
description: "六种可运行的形态，每种 provision 模型一个，外加端到端的起步套件。每页都是那种形态的完整配方：配置、主机需要提供什么，以及它特有的失败模式；第一次接触就从本地快速开始，已有集群则去 Kubernetes 起步套件，引用的文件都留在仓库的 examples/ 与 charts/ 下，页面负责解释。"
---

# 示例

六种可运行的形态，每种 provision 模型一个，外加端到端的起步套件。每一页都是那种形态的完整配方——
配置、主机需要提供什么，以及它特有的失败模式。

| 形态 | 示例 | 你能得到什么 |
|---|---|---|
| `endpoints` | [静态 endpoints](static-endpoints.md) | 一直开机的机器：实验室的机器、构建 runner。gateway 把它们租出去再还回来；从不创建或销毁任何东西。 |
| `endpoints` | [本地快速开始](local-quickstart.md) | 快速开始落成文件——一台机器，`localhost:22`，不需要集群。能跑起来的最小配置。 |
| `workloads` | [任何能说 SSH 的 image](pod-image.md) | 一个最小的 sshd image，以及把它按需变成 pod 的 backend。 |
| `claims` | [Kubernetes 起步套件](kubernetes-sandbox.md) | 完整的集群形态：template、热 pool、从它们里面 claim 的 backend、RBAC 与部署。 |
| `command` | [Docker 起步套件](docker-sandbox.md) | 整个 gateway 在一个进程里，sandbox 就是 docker 容器。完全不需要集群。 |
| `claims` | [Helm chart](helm-chart.md) | 与上面相同的 Kubernetes 形态，但做成 chart，含 CRD 与热 pool。 |

第一次接触？从 [本地快速开始](local-quickstart.md) 开始——它只要一分钟，能端到端看到 lease 的行为。
如果你已经有集群，想要按需的 sandbox，直接去 [Kubernetes 起步套件](kubernetes-sandbox.md)。

每个示例引用的文件（template、RBAC manifest、docker 脚本）在仓库的 `examples/` 与 `charts/` 下；
这里的页面解释它们，仓库保存它们。
