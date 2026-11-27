---
description: "给一直开机的机器用：实验室的机器、构建 runner、一台很强的主机。gateway 在这里既不创建也不销毁任何东西，它同一时刻只把一个 endpoint 作为一个 identity 的 lease，用完再还回去；静态 endpoint 不可丢弃，所以每个 endpoint 钉一个 identity，清理归主机。"
---

# 示例：静态 endpoints（完全不需要编排器）

给那些一直开机的机器：实验室的机器、构建 runner、一台很强的主机。在这里 gateway 既不创建也不销毁任何东西——
它同一时刻只把一个 endpoint 作为某一个 identity 的 lease，用完再交还。

这一段是一个**片段**：它放在一份完整配置的 `backends:` 下，而完整配置还需要
`listen`、`host_key` 和一个 `identities:` 列表（见 [../local-quickstart/config.yaml](../assets/files/config.yaml)）。
单独运行它，进程会以退出码 2 结束，报 `at least one identity is required`。

```yaml
backends:
  - name: lab
    provision: endpoints
    endpoints:
      - "192.0.2.11:22"
      - "192.0.2.12:22"
    ssh:
      user: "<remote-user>"
      private_key: /etc/byre/keys/id_key
      host_key_policy: strict     # 长期存在的主机应该写进 known_hosts
```

端口是每个 `endpoints` 条目的一部分（`host:port`）。`ssh.port` 能被解析，但没有 driver 读它，所以把
`2202` 写进 `ssh.port`、把 `192.0.2.11` 写进 `endpoints`，启动时会失败并报
`endpoint "192.0.2.11" must be host:port`。

## 主机需要什么

1. `sshd` 能从 gateway pod 访问到；
2. gateway 的 pool 公钥在 `user` 的 `~/.ssh/authorized_keys` 里；
3. 别的什么都不需要——没有 agent、没有容器运行时、没有集群访问权限。

## 行为差异

| 方面 | `claims` / `workloads` | `endpoints` |
|---|---|---|
| 创建 | gateway 创建工作负载 | 预先存在 |
| release | 删除对象 | endpoint 被标记为空闲 |
| 重启后的接管 | 靠 annotation | 没有——归属关系在内存里，轮转从头开始 |
| 坏客户端的影响范围 | 一个可丢弃的 instance | 一台共享机器——每个 endpoint 钉住一个 identity |

因为静态 endpoint 不可丢弃，把它当例外对待：给它自己的 backend 名字、自己的 identity 映射，以及
`host_key_policy: strict`。identity 之间的清理是主机的责任——gateway 不会擦除一台不是它创建的机器。
