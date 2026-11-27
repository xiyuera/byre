---
description: byre 刻意不做什么——单副本、没有自动化端到端闸门、不做多租户隔离——以及已知做错的地方。
---

# 限制，以及已知不成立的地方

写在这里，而不是等你在生产里发现。如果其中任何一条挡到了你，那值得开一个 issue。

## 单副本

一个进程持有 lease 表，所以第二个副本会让一个 identity 拿到两个 instance。多副本的设计写在[路线图](roadmap.md)里；两个都没实现。

lease 表可以搬出进程——`lease.store: crd` 把它放在 etcd 里的一个 `ByreLease` 对象里，这正是「重启后接管存活 lease 而不是从干净状态开始」的原因。那是持久性，不是并发性：写者仍然只有一个。第二个副本必须满足的前置条件在 [ADR-0010](adr/0010-multi-replica-preconditions.md) 里。

## `/healthz` 不代表「我能发出一个 instance」

它回答的是「进程活着」，并打印编译进二进制的版本。一个缺了跨命名空间授权的网关是绿的、永远不会预热它的池、对每次 checkout 都回 403。这是这个项目里最具误导性的故障，也是[运维](handbook/operations.md)把授权检查放在一切之前的原因。

## `provision: command` 只接管它能归因的东西

重启时，它接管 `list` 报告里带 `annotations.byre.io/identity` 的 instance，其余的带着一条警告保留下来——这是刻意选择泄漏而不是做破坏性清理，这样没写 annotation 的 lister 会留下热容量，而不是销毁存活的 lease。在 `acquire` 里打印了 annotation 但 `list` 里没有（或者反过来），会得到一个被列出来却永远匹配不上重连 identity 的 instance。

## 没有自动化的端到端闸门

那些集群实跑是手工执行并记录的。harness 是 `BYRE_CFG=config.yaml BYRE_KEY=<客户端密钥> test/e2e/run.sh [--ttl-ok]`；它对着一个已经在跑的网关断言，缺这两个变量时以退出码 2 结束。基于 `kind` 的 CI 实跑是路线图里的一项。

## `endpoints` 扛不住重启

那个 driver 把归属放在内存里，所以重启后的网关重新轮询，重连的客户端会落在下一台 endpoint 上。`claims` 和 `workloads` 扛得住，因为 identity 写在对象本身上。

## `grace: 0s` 不代表「没有 grace」

`ApplyDefaults` 把显式的零值换成 60 秒，而且没有办法关掉。相比之下，`pool_size: 0` 就是字面意思：没有热容量。

## 池空时首次连接可能抢跑

用 `claims` 且热池被抽干时，几个 identity 同时连接可能跑在 controller 前面：claim 已存在但 pod 还没 `Ready`，落在这个窗口里的连接会被关掉（`Connection ... closed by remote host`）。lease 照样会被创建，所以一秒后重试就成功。把 `SandboxWarmPool.replicas` 提到你预期的并发首次连接数之上可以避免这件事。

## 落在哪个节点不是 byre 的决定

如果你的集群里有速度差别很大的节点，一个 identity 可能落在慢节点上，并整个 lease 都待在那儿——byre 不会重新调度。在一个混合集群上实测：同一个负载在快节点上用了 1.7 秒，在慢节点上用了 10.5 秒。如果这件事对你在意，在 `SandboxTemplate` 上用 `nodeSelector` 或 `affinity`。

## v1 里没有的

多副本一致性、动态 backend 均衡、instance 内的状态持久化、多租户、UI。任何特定于某个产品的东西属于一个集成示例，不属于网关。
