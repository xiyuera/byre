---
description: "把 README 的五分钟落成一份文件：用 provision: endpoints 在一台已经存在的机器上跑起 byre，生成 gateway 密钥与客户端密钥，改两个值、跑四条命令，然后连两次，看两条连接因为 lease 挂在客户端公钥上而落到同一个 instance；也写了这份配置刻意没有什么。"
---

# 示例：在一台机器上跑通（不需要集群）

README 里那五分钟，落成文件：本目录的 `config.yaml` 是一份完整配置，用 `provision: endpoints`——
一台已经存在的主机，它就可以是你正坐着的这台机器。

```sh
ssh-keygen -t ed25519 -N '' -f ~/.ssh/byre_pool     # gateway 连到 instance 用的那把密钥
cat ~/.ssh/byre_pool.pub >> ~/.ssh/authorized_keys  # instance 要信任它（这一步是你的，不是 byre 的）
ssh-keygen -t ed25519 -N '' -f ~/.ssh/byre_client   # 客户端用来认证的密钥
ssh-keygen -lf ~/.ssh/byre_client.pub -E sha256     # 这个 SHA256: 值填进 `identities`
```

改掉 `config.yaml` 里两处——`<your-user>` 与那个指纹——然后：

```sh
go build -o byre ./cmd/byre
./byre -check-config -config examples/local-quickstart/config.yaml   # rc=0，只说明文件自洽
./byre -config examples/local-quickstart/config.yaml
ssh -p 2222 -i ~/.ssh/byre_client -o IdentitiesOnly=yes anything@127.0.0.1 'hostname; id -un'
```

连两次：gateway 日志里两条连接都指向同一个 instance，因为 lease 挂在客户端公钥上。不在 `identities` 里的
密钥在认证阶段就被拒（`Permission denied (publickey)`），这就是全部的访问控制模型。

这份文件**没有**的部分：没有 `ssh.port`（端口写在 `endpoints` 里——没有任何 driver 读 `ssh.port`）、
没有 `pool_size`（`endpoints` backend 不创建任何东西，没有可预热的）、没有 `namespace`（那是给
Kubernetes 两种形态的，见 `examples/kubernetes-sandbox/` 与 `examples/pod-image/`）。

`-check-config` 通过并不等于配置可用：它不打开密钥文件、不连主机，也不知道 `<your-user>` 还是占位符。
真正的检查是第一次连接。

`endpoints` backend 熬不过重启——归属关系只在内存里，重启后的 gateway 会重新轮转。这一点写在
`docs/handbook/backends.md` 里，与那些确实能跨重启存活的形态放在一起。
