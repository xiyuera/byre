---
description: "把 byre 装成一个二进制或一个容器，指向 localhost:22，然后看一条 lease 在断线重连之后仍然属于你。这是最小可用的网关，五分钟内能跑起来，也是判断它是否适合你的最快方式：不需要集群、不需要 RBAC，一条命令就能看到租约跟着身份走。"---

# 快速开始

两条路。第一条只需要 Docker；第二条只需要一台你本来就能 `ssh` 过去的机器。

## 三十秒，用 Docker

```sh
git clone https://github.com/xiyuera/byre && cd byre/examples/quickstart
./run.sh
```

脚本会生成一把客户端密钥和一把池密钥、渲染一份配置、构建一个 sandbox 镜像、把网关和 sandbox 作为两个容器起起来，然后打印你需要的那一条 `ssh` 命令。接着：

```sh
# 把打印出来的命令跑两次
ssh -o StrictHostKeyChecking=no -p 2222 -i ./keys/id_ed25519 sandbox@127.0.0.1 hostname
ssh -o StrictHostKeyChecking=no -p 2222 -i ./keys/id_ed25519 sandbox@127.0.0.1 hostname
```

两次打印同一个 hostname。这就是这个产品。等过 120 秒的 ttl，instance 就被释放——再连一次你拿到的是另一个，那是 lease 到期，不是 bug。

那个目录里有什么、怎么从它走到真实部署，在 [`examples/quickstart/README.md`](../examples/quickstart.md)（在仓库里，不在本站）。

## 五分钟，不用 Docker

最简单的 backend 是 `provision: endpoints`：不创建任何东西，网关只决定谁拿哪台主机。为此你需要一台带 `sshd` 的 Linux 机器——可以就是你正坐着的那台。

**1. 构建。**

```sh
go build -o byre ./cmd/byre
./byre -version          # dev (none, unknown)
```

**2. 写一份配置。**

```yaml
listen: ":2222"
host_key: /var/lib/byre/hostkey/id_ed25519

lease:
  ttl: 360s
  grace: 60s

backends:
  - name: lab
    provision: endpoints
    endpoints: ["instance-1.example.com:22", "instance-2.example.com:22"]
    ssh:
      user: "<remote-user>"
      private_key: /etc/byre/keys/id_key
      host_key_policy: strict

identities:
  - fingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    backend: lab
```

把用户、主机名和密钥路径换掉。其余先别动。

指纹是**客户端的**，不是网关的；`host_key` 是网关自己的密钥——客户端要 pin 的就是它。把 `host_key` 指到持久存储上：它不存在时会在首次启动时创建，而这个字段留空会给你一把临时密钥，每次重启都变（那会毁掉 pinning）。

```sh
ssh-keygen -lf ~/.ssh/id_ed25519.pub -E sha256
# 256 SHA256:ihSSTfidE8FBRGaLem2sknNk2tDW5LM67WReyPggABU you@laptop (ED25519)
```

**3. 启动之前先校验。** 一份坏配置，值得在它变成一次失败的连接之前就被抓住：

```sh
./byre -check-config -config ./config.yaml
```

**4. 跑起来，然后连两次。**

```sh
./byre -config ./config.yaml &
ssh -p 2222 -i ~/.ssh/id_ed25519 you@127.0.0.1 hostname
ssh -p 2222 -i ~/.ssh/id_ed25519 you@127.0.0.1 hostname
```

两次同一个 hostname。用 `provision: endpoints` 时，分配只在进程活着期间稳定——见[限制](../limits.md)。

## 接下来去哪

| 你想 | 页面 |
|---|---|
| 了解四种 provision 形态，以及该选哪种 | [选择后端](backends.md) |
| 在集群里按需创建 sandbox | [Kubernetes](kubernetes.md) |
| identity、lease、池、就绪判据到底怎么拼起来 | [架构](../handbook/architecture.md) |
| 它跑着但不对 | [排错](../handbook/troubleshooting.md) |
