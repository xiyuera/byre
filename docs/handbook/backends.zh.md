---
description: "四种 provision backend 的每个字段、就绪判定规则、失败时各自的行为，以及 `command` backend 必须遵守的契约：只在 stdout 输出一个 JSON 对象、靠 list 让重启后 adopt 生效、timeout 只约束直接子进程，并附一个 20 行的完整示例和一张失败行为表。"
---

# backend

backend 是获得 instance 的一种方式，也是到达它的一种方式。先选形状，再填它的字段。

## 选哪一个 —— 看手上已有什么

| 手上有什么 | `provision` | 原因 |
|---|---|---|
| 已经在跑 `sshd` 的主机（lab、构建机、一台大机器） | `endpoints` | 什么都不创建；gateway 只决定谁拿哪台主机 |
| 有 claim 形状 CRD 和/或热池 的集群 | `claims` | controller 负责创建和删除；byre 只负责请求 |
| 一个集群加一个跑 `sshd` 的镜像，没有 CRD | `workloads` | byre 为每个 lease 创建一个 Pod 并删除它 |
| 云主机、Incus/LXD、CMDB —— 任何藏在 CLI 后面的东西 | `command` | 你的程序负责 provision 并描述 instance |

| `provision` | 能否活过 gateway 重启 | 有无热池 | 是否需要集群访问 | 是否在真实集群上测过 |
|---|---|---|---|---|
| `endpoints` | 否 —— 归属关系在内存里 | 无（未实现 `Create`） | 否 | 是 |
| `workloads` | 是 —— pod 上的 identity annotation | 有 | 需要（create/delete/patch pod） | 用假客户端跑的单元测试 |
| `claims` | 是 —— 对象上的 identity annotation | 有 | 需要（配置的 GVK） | 是（有记录的运行） |
| `command` | 是 —— 当 `list` 报出 `annotations.byre.io/identity` 时 | 无 | 否 | 单元测试 |

## 配置块

```yaml
backends:
  - name: <label used in logs and by identities[].backend>
    provision: claims | workloads | endpoints | command    # 一个 instance 如何产生
    # …provision 需要的字段（每种形状一张表，见下）…
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

`provision` 是唯一必需的判别字段。`type` 是 0.3 之前的写法：
设置它得到的是一条指出替代字段的配置错误，而不是未知字段错误。

## 文件：backend 之外的字段

下面的 provision 块不独立存在；它们和四个顶层键并列。一个文件能表达的全部内容，都在这里：

| 字段 | 必填 | 默认 | 含义 |
|---|---|---|---|
| `listen` | 否 | `:2222` | SSH 监听地址；`-listen` 可在单次运行中覆盖它 |
| `host_key` | 否 | 空 —— 首次启动时生成 | gateway 自己的 host key 路径，客户端 pin 的就是它；留空则重启会换 key，所有用 `strict` 校验的客户端都会失败 |
| `lease.ttl` | 否 | `360s` | instance 被回收前的空闲时间；必须大于客户端自己的空闲超时 |
| `lease.grace` | 否 | `60s` | 断开后的重连窗口，窗口内同一个 identity 仍能拿回它的 instance |
| `lease.pool_size` | 否 | `2` | 热 instance 数量，只对实现了 `Create` 的 driver 有效；显式写 `0` 会被读成“未设置”并变成 2，所以不存在零大小的 pool |
| `backends[]` | 是 | — | 一个或多个 provision 块 —— `name`、`provision`、该形状的字段、`ssh` |
| `identities[]` | 是 | — | 谁可以 acquire：`fingerprint`（必填，会做规范化）、`backend`（必填，必须是上面某个块的名字）、`aliases`（可选，只作日志标签，从不参与路由） |

## `ssh` 块：四种形状共用

| 字段 | 必填 | 默认 | 含义 |
|---|---|---|---|
| `ssh.user` | 是 | — | gateway 在 instance 上使用的登录名；不是客户端的用户名 |
| `ssh.private_key` | 是 | — | gateway 自己用来连 instance 的私钥，需对 gateway 进程可读 |
| `ssh.port` | 否 | `22` | 会被接受并做范围校验，但 **没有 driver 读它**：端口取 `addr` 里的，或每条 `endpoints` 里的 |
| `ssh.host_key_policy` | 否 | `accept-new` | `strict`（需要 `known_hosts`）、`accept-new`（首次信任，在进程内 pin 住）、`insecure`（不校验） |

`ssh.port` 是陷阱：它看起来像是通往非标准端口 instance 的路，其实不是。
监听 2202 的主机在 `endpoints` 里写成 `"host:2202"`，
或者由 `command.acquire` 返回 `"addr":"host:2202"`。
`endpoints: ["192.0.2.11"]` 配 `ssh.port: 2202` 会让 `-check-config` 失败，
报 `endpoint "192.0.2.11" must be host:port`。

`private_key` 是普通路径，**`~` 不会被展开** —— `~/keys/id` 能过 `-check-config`，
然后在首次拨号时失败：`read key ~/keys/id: no such file or directory`。用绝对路径。

这里指定的 key 必须在 instance 的 `authorized_keys` 里。byre 从不安装它：
`endpoints` 由你负责，`workloads` 必须预置在镜像里，
`claims` 得靠 controller 的 template 完成，`command` 则要你的程序在打印记录之前做好。

## 就绪判定：一条规则，两种实现

就绪判定回答的是“现在可以交给客户端了吗”，而对 `claims` backend，
答案永远是“某个地址以一个完整的 SSH banner 作出了应答”。绝不是“某个 status 字段存在”
——claim 的 `status.sandbox.serviceFQDN` 由 controller 写入，
比 Service 的 DNS 记录生效早几秒；而在 fake-IP 网络里，
不存在的名字也能解析成功（ADR-0005）。也绝不只是“端口接受了连接”：
socket 激活的 sshd 在能说话之前就接受连接，而一个发出半个 banner 就停住的 peer，
正是这个探针要抓的半启动进程（ADR-0005 修订）。

| `provision` | 就绪条件 | 间隔 | 超时 |
|---|---|---|---|
| `endpoints` | 无 —— endpoint 按写的那样直接用 | — | — |
| `workloads` | pod 有 `podIP` **且** `PodReady`，phase 不是 `Failed` | 250 ms | `provider.ready_timeout` (120s) |
| `claims` | 第一个应答的地址上出现完整 SSH banner（`SSH-2.` 行，CRLF 结尾） | 250 ms | `provider.ready_timeout` (120s) |
| `command` | 无 —— `addr` 按写的那样直接用 | — | `command.timeout` 约束的是程序本身 |

对 `claims`，候选地址按这个顺序试：`provider.endpoint_field` 指向的路径（默认
`status.sandbox.serviceFQDN`），然后 `status.sandbox.podIPs` 的每一项。
裸主机名会被规范化为 `host:22`。

## `endpoints`

```yaml
  - name: lab
    provision: endpoints
    endpoints: ["instance-1.example.com:22", "instance-2.example.com:22"]
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: strict}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `endpoints` | 是 | `host:port` 形式的条目；启动时按此校验；重复项会被拒绝 |

不创建任何东西，也不销毁任何东西。`Acquire` 返回该 identity 已持有的 endpoint，
否则按轮询顺序返回下一个空闲的。容量是 `len(endpoints)`；
超出后 `Acquire` 返回 `no capacity`，客户端看到的就是一次失败的 session 打开。

**没有 pool。** driver 未实现 `Create`，所以 `pool_size` 无效，
`byre_pool_warm{backend="lab"}` 一直是 0。

**活不过重启。** 归属关系只存在于 driver 的内存里。重启之后每个 endpoint 看起来都空闲，
重连的客户端会被重新轮询（实测：两个 identity 分别在 2200 和 2201，重启一次，
第二个 identity 回到了 2200）。如果 lease 必须比 gateway 进程活得久，
用 `claims` 或 `workloads`。

**identity 之间没有清理。** 释放后的 endpoint 仍然留着上一个 identity 的文件，
所以给每个 identity 分配独立 endpoint——或者用自己的工具擦掉主机——是唯一安全的默认做法。
这里值得为 `host_key_policy: strict` 维护 `known_hosts`，正是因为这些主机是长期存在的。

## `workloads`

```yaml
  - name: adhoc
    provision: workloads
    namespace: "<namespace>"
    provider:
      image: "<registry>/<image>:<tag>"
      start_command: ["/usr/sbin/sshd", "-D", "-e"]
      ready_timeout: 180s
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `namespace` | 是 | — | pod 创建在哪个 namespace |
| `kubeconfig` | 否 | 集群内 | kubeconfig 路径，用于在集群外运行 |
| `provider.image` | 是 | — | 必须在 22 端口跑 `sshd`，并信任 pool 的 key |
| `provider.start_command` | 否 | 镜像 entrypoint | 覆盖 entrypoint，例如 `["/usr/sbin/sshd","-D","-e"]` |
| `provider.ready_timeout` | 否 | `120s` | 冷拉取镜像层时 180s 是更合理的默认值 |

pod 以 `restartPolicy: Never` 创建，只有一个名为 `instance` 的容器，
带 `byre` 的 managed-by 标签，没有 volume —— 任何需要持久化的东西都不该放在 pod 里。
`Release` 用 `gracePeriodSeconds: 0` 删除 pod；pod 已经不在了不算错误。

就绪超时时，pod 会在 acquire 失败之前被删除，所以坏镜像不会留下一串 Pending 的 pod。RBAC：
`pods: get, list, watch, create, delete` **外加`patch`**（identity annotation 正是 adopt 生效的前提）。

## `claims`

```yaml
  - name: pooled
    provision: claims
    namespace: "<namespace>"
    provider:
      template: "<template-name>"
      warm_pool: "<warm-pool-name>"
      api_group: "<api-group>"
      api_version: "<api-version>"
      resource: "<plural-resource>"
      object_kind: "<Kind>"
      endpoint_field: ["status", "sandbox", "serviceFQDN"]
      ready_timeout: 120s
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `namespace` | 是 | claim 对象创建在哪个 namespace |
| `kubeconfig` | 否 | 空 = 集群内的 service account |
| `provider.template` | 两者之一 | controller 实例化的具名 template |
| `provider.warm_pool` | 两者之一 | 从哪个 pool 里 claim；claim API 各不相同，所以两个 selector 都在 |
| `provider.api_group`、`api_version`、`resource`、`object_kind` | 是 | GVK，原样发出；没有任何内置值 |
| `provider.endpoint_field` | 否 | 对象内地址的路径，默认 `status.sandbox.serviceFQDN` |
| `provider.ready_timeout` | 否 | 120s |

对象按上面的字段创建，打上 identity annotation，并在 release 时删除。
一个在 `ready_timeout` 之内始终没有应答的 claim，会在 acquire 失败之前被删除。

这是唯一有真实集群运行记录的形状（`docs/acceptance/`）。
它针对的部署用的是 `extensions.agents.x-k8s.io` 的 `SandboxClaim` 加热池 ——
`api_group: extensions.agents.x-k8s.io`、`resource: sandboxclaims`、
`object_kind: SandboxClaim`，
`api_version` 取决于你的集群提供什么（`kubectl api-resources | grep sandboxclaims`）。
这些值属于你的配置，不属于这个二进制。

## `command`

```yaml
  - name: cloud
    provision: command
    command:
      acquire: ["/usr/local/bin/provision-ci-vm"]
      release: ["/usr/local/bin/destroy-ci-vm"]
      list: ["/usr/local/bin/list-ci-vms"]
      timeout: 120s
      env: ["CLOUD_PROFILE=ci"]
    ssh: {user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `command.acquire` | 是 | argv，不是 shell 字符串 —— 没有 glob、没有管道、没有 `$VAR` |
| `command.release` | 否 | argv；没有它时，release 是一个只记日志的空操作 |
| `command.list` | 否 | argv；重启时会 adopt 带 `annotations.byre.io/identity` 的条目，其余保留（见下） |
| `command.timeout` | 否 | 每次调用 120s；约束的是程序本身，不是它的子进程 |
| `command.env` | 否 | `KEY=VALUE`，**追加**到继承来的环境上，从不替换 |

### 契约

`acquire` 在 stdout 上只打印一个 JSON 对象，
并以 0 退出。stdout 上没有别的东西 —— 诊断信息走 stderr，byre 会捕获它，
并在退出码非零时放进错误消息。stdout 上多出来的输出是致命的：
`acquire printed 58 bytes that are not one JSON object: invalid character 'p' …`（`internal/backend/command.go:121-124`）。

```json
{"id":"vm-17","addr":"192.0.2.17:22","user":"ci","private_key":"/etc/keys/ci"}
```

| 键 | 必填 | 说明 |
|---|---|---|
| `id` | 是 | 稳定，在 backend 内唯一，出现在日志和 annotation 里；不要把 identity 材料放这里 |
| `addr` | 是 | gateway 拨号的 `host:port` |
| `user` | 否 | 回退到 `ssh.user`（`command.go:104`） |
| `private_key` | 否 | 回退到 `ssh.private_key`（`command.go:105`） |
| `annotations` | 否 | 持久提示，identity 绑定也在其中；必须在 `acquire` **和** `list` 里都出现，adopt 才能对上（`command.go:106-126`） |

`id` 和 `addr` 在解码之后检查：`id` 或 `addr` 为空会让 acquire 失败，
报 `returned an unusable instance: Addr is empty`（`internal/backend/backend.go:35-45`）。
`ssh.user` / `ssh.private_key` 回退值仍然必须配置，即使每条记录都自带：
`NewCommand` 拒绝没有它们的 backend（`command.go:65-67`），
因为省略了它们的记录无处可回退。

每次调用时的环境变量：

| 变量 | 出现于 | 值 |
|---|---|---|
| `BYRE_IDENTITY` | `acquire` | 提出请求的客户端 fingerprint（base64 主体，无 `SHA256:` 前缀，无填充） |
| `BYRE_INSTANCE_ID` | `release` | 创建它的那次 acquire 返回的 `id` |
| `BYRE_INSTANCE_ADDR` | `release` | 那次 acquire 返回的 `addr` |

`list` 完全拿不到 `BYRE_*` 变量（`command.go:144`），打印的是同样对象的 JSON **数组**；
校验失败的条目会被跳过并给一条警告，而不是让整个 list 失败（`command.go:148-161`）。实测：
用一个把环境变量 dump 到文件的 lister，文件是空的。

**失败**是非零退出加 stderr 上的一行；
这一行会进到 gateway 的错误里（`run "…/acquire.sh": exit status 1 (stderr: quota exceeded for project ci)`，`command.go:87-89`）。客户端自己只会看到 `Connection to <host> closed by remote host`。

**`timeout` 约束的是程序本身，不是它的进程树。** 截止时间用 `exec.CommandContext` 强制执行，它只杀直接子进程（`command.go:79-81`）。
一个留下后台进程占着 stdout 的脚本，会让等待越过超时 —— 用 `timeout: 2s` 实测：
以 `sleep 4 & sleep 4` 结尾的脚本被等了 4s，
而同样内容用 `exec sleep 6` 的脚本在 2s 被切断。如果你的 provisioner 会派生 helper，
让它用 `exec` 跑真正的工作，或者 `wait` 自己的子进程。

**重启时靠 `list` 来 adopt，认不出来的就留着。** `Manager.Adopt` 在 pool 预热之前运行：
记录里带 `byre.io/identity` 的 instance 会被放回 lease 表（`internal/lease/manager.go:225-302`）。一个*没有 annotation* 的 instance 是否被释放，
取决于 driver，只有做出这个承诺的 driver 才享有这个待遇：
`kubernetes` 和 `endpoints` 实现了 `backend.StaleCapacity`，
把它们 lease 出去的每个 instance 都报成带 identity，
所以匿名的是它们自己的过期容量（`backend.go:87-99`）；`command` 没有实现它，
所以没有 annotation 的 instance 会被**保留**并记日志：

```
{"msg":"instance kept at startup","instance_id":"vm-anon","backend":"cloud","reason":"no byre.io/identity annotation and the driver cannot promise that anonymous instances are stale; destroying it could kill a live lease"}
```

这就是 ADR-0009，它是刻意的泄漏：销毁正在使用的东西无法挽回，留下热容量则可以。
即使什么都没 adopt，也会打印 `adoption finished`。要让 adopt 生效，
在 `acquire` 和 `list` **两处**都打印
`"annotations":{"byre.io/identity":"<fingerprint>"}`（`command.go:162-163`）
—— fingerprint 就是 `BYRE_IDENTITY` 携带的那个 base64 主体。

### 一个完整示例（20 行）

```sh
#!/bin/sh
# provision-ci-vm：创建一台 VM，打印记录，退出 0。假定用 provider 自己的 CLI。
set -eu
set -f                      # 对我们没写的东西不做 glob
name="ci-$(date +%s)-$$"
mytool vm create --name "$name" --profile "${CLOUD_PROFILE:-default}" >/dev/null
ip=$(mytool vm ip "$name")
key=/etc/byre/keys/ci
printf '{"id":"%s","addr":"%s:22","user":"ci","private_key":"%s"}\n' "$name" "$ip" "$key"
```

`release` 会拿到 `BYRE_INSTANCE_ID` 和 `BYRE_INSTANCE_ADDR`。如果希望重启后 adopt，
就写一个 `list`：在 `acquire` 和 `list` 里都打印
`"annotations":{"byre.io/identity":"<fingerprint>"}`，
重连的 identity 就能拿回它的 instance。没有 `list`，
你的程序创建的 instance 会比 gateway 活得久，byre 里再没有任何东西会注意到它们 —— 清理是你的 runbook 的责任。

## 各形状的失败行为

| 故障 | 客户端看到什么 | gateway 做什么 |
|---|---|---|
| endpoint 不可达 | `Connection to … closed by remote host` | 日志里 `cannot reach instance`；lease 作废；下一条连接拿到新的 instance |
| instance 拒绝 session channel | 静默退出 255 | `channel failed type=session … open session on target` |
| claim 始终未就绪 | 连接在 `ready_timeout` 后关闭 | 对象被删除；acquire 失败；下次尝试从头再来 |
| pod 镜像拉不下来 | 同上 | pod 被删除；pool 尝试预热期间 `pool refill failed` 反复出现 |
| pool 耗尽（`endpoints`） | `no capacity` | acquire 立即失败，不排队 |
| `command` 非零退出 | 连接关闭 | 程序的 stderr 被放进错误里 |
| `command` 输出格式错误的 JSON | 连接关闭 | `acquire printed N bytes that are not one JSON object` |

## 增加第五种形状

`internal/backend/backend.go` 定义接口 —— `Acquire`、`Release`、`List`、`Adopt`、`Name`，
可选还有 `Bind`（把 identity 记到 instance 上）和 `Create`（在被请求之前就造出容量，
这正是让 driver 拥有 pool 的东西）。新增一种形状需要：
`internal/config/config.go` 里的一个常量、它的校验、一个 driver、
一份 `examples/<shape>/` manifest，以及本文里的一节。
客户端契约不变：gateway 仍然按公钥认证，仍然每个 identity 发一个 instance。
