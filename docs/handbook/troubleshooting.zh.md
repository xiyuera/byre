---
description: "每一种 byre 故障在客户端和 gateway 日志里分别是什么样，以及先查哪一个；按各原因出现的频率排序，给出每个症状的第一条命令、判定依据，加上容易误判成配置问题的地方，最后是 -check-config 每个返回值的含义与重启后 endpoint 重新轮询的原因，以及 host key 变更后该改哪一侧。"
---

# 故障排查

按顺序往下走。每一步回答一个问题，并把下一步的范围缩小：
客户端认证过了吗，gateway 能不能到达 instance，instance 接不接受 session，lease 行为是否正常。

gateway 说的每句话都是 stdout 上的一行一个 JSON 对象。identity 以 fingerprint 的前 16 个字符出现 —— 足够关联，不足以重建一把 key。

```sh
journalctl -u byre -f                                  # 或：kubectl -n <ns> logs deploy/byre -f
curl -s :8099/metrics | grep '^byre_'                  # 存在的全部计数器
./byre -check-config -config config.yaml; echo rc=$?   # 只查配置，不碰集群
```

## 症状总表

| 你看到的现象 | 含义 | 第一条命令 |
|---|---|---|
| `Permission denied (publickey)` | 客户端的 key 不在 `identities` 里 | `ssh-keygen -lf key.pub -E sha256`，与配置对照 |
| `Connection to … closed by remote host`（rc 255） | gateway 认证了客户端，然后到不了 instance | 在日志里 grep `cannot reach instance` |
| ssh 以 255 退出，没有任何消息 | instance 拒绝了 channel | 在日志里 grep `channel failed` |
| 连上了，但上一条命令留下的文件不见了 | lease 过期了，或者 identity 变了 | `byre_leases_idle`，以及两条 `session open` 上的 `instance` 字段 |
| `REMOTE HOST IDENTIFICATION HAS CHANGED` | gateway 的 host key 变了 | 见 [host key](#gateway-的-host-key-变了) |
| 连接挂起 `ready_timeout` 之后关闭 | backend 造不出就绪的 instance | grep `pool refill failed` 和 `no instance for identity` |
| `/healthz` 正常，但每个 session 都 403 | 缺跨 namespace 的授权 | `auth can-i patch … --as system:serviceaccount:…` |
| `/readyz` 一直是 503 | gateway 无法 provision：pool 空了，最近一次填充也失败了 | grep `pool warm failed` 和 `byre_pool_fill_errors_total`；先修创建路径，再怀疑探针 |

## 客户端认证不上

```
anything@127.0.0.1: Permission denied (publickey).
```

gateway 只提供一种认证方式 —— 公钥 —— 且只对 `identities` 里列出的 key 提供。
按 gateway 的计算方式打印 fingerprint，然后对照：

```sh
ssh-keygen -lf ~/.ssh/id_ed25519.pub -E sha256
# 256 SHA256:ihSSTfidE8FBRGaLem2sknNk2tDW5LM67WReyPggABU you@laptop (ED25519)
```

`SHA256:` 前缀和 base64 填充会被规范化掉，
所以 `SHA256:ihSS…ABU` 和 `ihSS…ABU` 是同一个 identity。
不同的 fingerprint 就是不同的 identity —— 没有通配，没有按 backend 的默认 identity，
也没有匿名访问。

两个客户端出示同一把 key，在设计上就是一个 identity，会争抢同一个 instance。

## gateway 到不了 instance

客户端看到连接被关闭，rc 255。说明原因的是 gateway 的那一行：

```
{"level":"WARN","msg":"cannot reach instance","identity":"ihSSTfidE8FBRGaL","instance":"127.0.0.1:2299","error":"dial 127.0.0.1:2299: dial tcp 127.0.0.1:2299: connect: connection refused"}
```

目前见过的三个原因，按值得检查的顺序：

1. **endpoint 上没有监听** —— 那个 `host:port` 上什么都没有，或者 gateway 和它之间有防火墙。检查方法是在 gateway 自己的 network namespace 里跑 `nc -z <host> <port>`。
2. **`private_key` 以 `~` 开头** —— 它不会被展开，而 `-check-config` 照样通过，因为文件要到首次拨号时才打开：
   ```
   {"level":"WARN","msg":"cannot reach instance",…,"error":"read key ~/keys/id: open ~/keys/id: no such file or directory"}
   ```
   用绝对路径。
3. **key 不在 instance 的 `authorized_keys` 里** —— 拨号成功，握手失败：
   ```
   {"level":"WARN","msg":"cannot reach instance",…,"error":"handshake with 127.0.0.1:2210: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"}
   ```
   `endpoints` 由你安装；其他形状由你的镜像、template 或 provisioner 完成。

## instance 接受 SSH 却拒绝 session

客户端以 255 退出，自己没有消息；有些客户端会打印
`Session open refused by peer`。gateway 记的是 channel，不是连接：

```
{"level":"INFO","msg":"session open","identity":"ihSSTfidE8FBRGaL","backend":"lab","instance":"127.0.0.1:2210"}
{"level":"WARN","msg":"channel failed","type":"session","error":"open session on target: ssh: rejected: connect failed (open failed)"}
{"level":"INFO","msg":"session closed","identity":"ihSSTfidE8FBRGaL","instance":"127.0.0.1:2210"}
```

`session open` 意味着认证*和*上游 SSH 握手都成功了，所以问题在 instance 上：
`MaxSessions 0`、`MaxStartups` 满了、一个拒绝所请求命令的 forced command，
或者 `sshd` 用完了文件描述符。用 gateway 自己的 key 直接测 instance —— 如果以同样方式失败，那就与 byre 无关：

```sh
ssh -i <ssh.private_key> -p <port> <ssh.user>@<instance-addr> true
```

只有 `session` 和 `direct-tcpip` 的失败是这样记的；
`direct-tcpip` 失败就是坏掉的 `ssh -L` 的样子。

## lease 没有被释放

回收时钟是 `ttl` + `grace`，由一个每 30 秒运行一次的 reaper 检查。没有东西会被提前释放，
断开时也不会释放 —— grace 窗口就是干这个的。

在 `ttl: 10s`、`grace: 5s` 的实验 gateway 上实测：客户端断开后，lease 仍被计为空闲，
`byre_leases_idle` 在约 25s 后回到 0（15s 时钟加上下一次 reaper tick；
两次运行分别测得 24s 和 25s）。

```sh
curl -s :8099/metrics | grep -E '^byre_leases_(active|idle)'
```

如果 `byre_leases_idle` 在远超 `ttl` + `grace` + 30s 之后仍大于零：

- 显式的 `grace: 0s` 并不表示“没有 grace”：`ApplyDefaults` 会把 0 换成 60s，而且没有办法关掉。（`pool_size: 0` 从 v0.1.3 起不同：它按写的生效。）日志在启动时打印生效值 —— 先读 `configuration accepted` 那一行，再假设你的设置生效了；
- 一个什么都不发的客户端也能把 channel 举着；`byre_leases_active` 说明是否还有东西挂着；
- 对 `endpoints`，一个从未释放的 lease 在重启后是不可见的，因为归属关系活在已经死掉的那个进程里。

## pool 预热不起来

`byre_pool_warm{backend="…"}` 一直是 0，日志反复出现 `pool refill failed`。

1. **只有 `claims` 和 `workloads` 有 pool。** `endpoints` 和 `command` 造不出容量，所以 `pool_size` 对它们无效，计数器按设计一直是 0。
2. **缺少授权。** 一个无权在 instance namespace 里 create 或 patch 的 gateway 是活的，`/healthz` 也是绿的，然后每次 checkout 都用 403 拒绝：
   ```sh
   kubectl -n <instances-ns> auth can-i patch <plural-resource> --as system:serviceaccount:<gateway-ns>:byre
   ```
   `patch` 是关键：identity annotation 是用 patch 写的，所以只有 `create`/`delete` 不够。
3. **镜像或 template 有问题** —— `ImagePullBackOff`、template 名字不存在、配额。日志会指出 backend 和 API 错误；`kubectl -n <ns> get pods` 能看到起不来的那个 pod。

## gateway 的 host key 变了

```
WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!
```

所有开了 `StrictHostKeyChecking` 的客户端现在都失败。两个原因，其中一个是配置错误：

- **`host_key` 是空的。** gateway 每个进程生成一把 key，并记日志 `no host_key configured: using an ephemeral host key; clients cannot pin this gateway`。改配置，不要改客户端。
- **`host_key` 指向的存储没活下来** —— 没有 PVC、挂载路径错了、重建的容器没有 volume。启动时出现 `generated a new host key and persisted it` 这一行，就是写了*新* key 的证据；在有持久存储的情况下重启，它不该出现。

刻意的轮换见 [运维](operations.md#主机密钥)。

## `-check-config` 的行为

```sh
./byre -check-config -config config.yaml; echo rc=$?
```

- **rc=0** —— 文件能解析，默认值被应用，每个 driver 都能构建。它*不*意味着集群会接受这些对象、instance 上的 key 存在、或者 `endpoints` 可达。它甚至不意味着文件被改过：`deploy/config.example.yaml` 里的 `<placeholder>` 值和全零 fingerprint 都能通过，因为校验的只是形状。
- **rc=2** —— 至少一处违规，**全部一次性打印**，在 `error` 字段里用分号分隔。未知字段、未知或缺失的 `provision`、0.2.x 遗留的 `backend.type`、重复的 fingerprint、格式错误的 endpoint（`must be host:port`）、缺失的 `ssh.user` 或 `ssh.private_key`、一个指向不存在 backend 的 identity。

把列出的每一项都修掉。只修一个再重跑，就是一个拼写错误能活过三次部署的方式。

## `endpoints` 在重启后发的是另一个 instance

这是预期行为。driver 只把归属关系放在内存里，
所以重启后的 gateway 没有记录哪个 endpoint 属于哪个 identity，会从头开始轮询。实测：
两个 identity 分别在 2200 和 2201，重启一次，第二个 identity 回到了 2200。

如果 lease 必须比进程活得久，用 `claims` 或 `workloads`：identity 被写到对象上，
启动时读回来（日志里 `adopted existing leases count=N`）。

## 2222 上没人监听

前端在配置被接受之后才绑定，所以日志里有 `configuration accepted` 却没有
`ssh front end listening`，意味着绑定失败（`listen …: address already in use`）
或进程已经退出。`-listen` 覆盖配置里的地址，
这也是测试时两个本地 gateway 抢同一个端口的常见原因。
