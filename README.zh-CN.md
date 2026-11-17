# byre

[English](README.md) · **中文**

byre 是给 agent 沙箱用的 SSH 网关层：租约挂在**身份**（客户端的公钥）上，而不是挂在一条连接上，所以一个
agent 在租约有效期内独占一个实例。

一个 agent 同步工作树、构建、把产物拷回去、再留一条端口转发跑测试，这件事是四个 SSH 连接。byre 让这四个
连接落到同一台机器上，客户端既不用保持 control master，也不用记住任何标识符：它认证时用的那把公钥，就是
地址。

> 状态：**0.3.0，1.0 之前**。身份、租约、热池、重启后的接管在真实集群上验证过（`docs/acceptance/`）；四种
> provision 里只有 `claims` 有集群实测。单副本、没有自动化端到端门禁，镜像与清单部分只做了服务端 dry-run
> 校验。见 [限制](#限制与已知问题)。

> 由 `keypod` 改名而来（见 `docs/rename-to-byre.md`）。与任何同名项目无关，未获其背书，也不派生自它们；
> 这个名字指牛棚，许可证不授予任何商标权。

## 这是什么

```
agent ──ssh(pubkey)──▶ byre :2222 ──ssh(pool key)──▶ instance :22
        身份                  │                        容器、虚拟机、
        = 租约               │                        实验机——任何跑着
                            └─ 内存里的租约表         sshd 的东西
                               ttl + grace，热池
```

网关只用公钥认证客户端，不看别的。它从这把公钥推导出一个身份，这个身份在持续连接期间独占一个实例。结束
租约的是空闲时间：`ttl`（默认 360s）加 `grace`（60s）之后实例回池。grace 窗口正是为了掩盖客户端自己的
空闲超时（`ssh` 配 `ControlPersist` 时是 300s）——重连落在窗口内，拿到同一个实例。

byre 自己不创建容器。它向**后端**要一个实例，拿回一条 `{id, addr, user, key}` 记录，所以"怎么通过 SSH
够到实例"这部分由四种后端共享；用哪种后端，是
[配置](#配置后端的-sshd-从哪来)一节的内容。两个监听、没有数据库、没有 sidecar：`2222` 给 SSH 客户端，
`8099` 给 `/healthz` 与 `/metrics`。租约表在内存里，Kubernetes 后端的实例身上带一个注明身份的注解，这
正是重启能活下来的原因——新进程先接管活着的租约，再去预热。

三个值得先知道的推论。共享私钥就是共享实例，所以密钥分发本身就是访问控制，配置里的别名只影响日志标签。
空闲按墙上时钟计算，想事情超过 `ttl` + `grace` 的 agent 回来会看到新实例。实例不是存储：任何必须比租约活得
久的东西都得放到别处。

## 为什么不是普通跳板机

跳板机把一条连接转发到目标。按连接分配实例看起来是一回事，但会在三个地方翻车：

| 发生什么 | 客户端看到什么 | 真正坏在哪 |
|---|---|---|
| 第二条连接落到实例 B | 每条命令都成功了 | 上传的工作树在 A 上 |
| 两条命令之间租约到期 | `cd: no such file or directory` | 实例在任务中途被回收了 |
| 两个 agent 共用一把钥匙 | 一个能看到另一个的文件 | 问题在密钥分发，不在网关 |

中间那种最贵，因为每一条单独的命都成功了：客户端记账簿描述的是*一个*文件系统，而网关悄悄给了它三个。
修法是让分配挂在比连接活得久的东西上，而认证之后的 SSH 连接上恰好有这样一样东西——客户端证明自己持有的
那把公钥。那就是身份，租约挂在它上面。

四种看起来显然的修法——源地址、会话 id、按连接容器的实例、强制客户端保持 `ControlMaster`——以及它们各自
为什么不行，见 [`docs/handbook/architecture.md`](docs/handbook/architecture.md)。

## 快速开始

需要 Go 1.23+，以及至少一台你能 SSH 连到的机器。最简单的后端是 `provision: endpoints`：它什么都不创建，
网关只决定谁拿到哪台主机。下面这五分钟里，那台机器可以是任何跑着 `sshd` 的 Linux，包括你正坐着的这台。

**1. 构建。**

```sh
go build -o byre ./cmd/byre
./byre -version          # dev (none, unknown)
```

**2. 写配置。** 两个端点、一个客户端身份。把 `<remote-user>`、主机名和密钥路径换掉，其余先别动。

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
      port: 22
      user: "<remote-user>"
      private_key: /etc/byre/keys/id_key
      host_key_policy: strict

identities:
  - fingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    backend: lab
```

指纹是客户端的，不是网关的；`host_key` 才是网关自己的密钥，也就是客户端要 pin 的那把。把 `host_key`
指向可持续的存储：文件不存在时它会在首次启动时被创建，而把这个字段留空会得到一把每次重启都变的临时密钥。

```sh
ssh-keygen -lf ~/.ssh/id_ed25519.pub -E sha256
# 256 SHA256:ihSSTfidE8FBRGaLem2sknNk2tDW5LM67WReyPggABU you@laptop (ED25519)
```

**3. 启动前先校验。** 它读取文件、套用默认值、把每个 driver 都构建一遍（不碰集群），任何违规都返回 2，
并一次列出全部问题：

```sh
./byre -check-config -config config.yaml; echo "rc=$?"
```

```
{"level":"INFO","msg":"configuration accepted","listen":":2222","host_key":"/var/lib/byre/hostkey/id_ed25519","backends":1,"identities":1,"ttl":"6m0s","grace":"1m0s","pool_size":2,"version":"dev (none, unknown)"}
rc=0
```

**4. 启动。**

```sh
./byre -config config.yaml
```

```
{"level":"INFO","msg":"configuration accepted","listen":":2222","host_key":"/var/lib/byre/hostkey/id_ed25519","backends":1,"identities":1,"ttl":"6m0s","grace":"1m0s","pool_size":2,"version":"dev (none, unknown)"}
{"level":"WARN","msg":"generated a new host key and persisted it","path":"/var/lib/byre/hostkey/id_ed25519","fingerprint":"SHA256:VdUlcld0d1sqsRaX5qTtINtES2iIIFsLiYBxVmAuzC0","note":"clients that pinned a previous key must be updated"}
{"level":"INFO","msg":"ssh front end listening","addr":"[::]:2222","identities":1}
{"level":"INFO","msg":"health endpoint listening","addr":":8099"}
```

那条 `WARN` 只在首次启动出现一次，之后不会再有：密钥被写进 `host_key` 并复用。上面这些日志去掉了每行都带的
`time` 字段。

**5. 连上去。** 用户名被忽略——决定一切的是密钥——所以 `anything` 和别的写法一样。连两次，然后看网关日志：
两行的 `instance` 相同。

```sh
ssh -p 2222 -i ~/.ssh/id_ed25519 \
    -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new \
    anything@127.0.0.1 'hostname; ls'
```

```
{"level":"INFO","msg":"session open","identity":"ihSSTfidE8FBRGaL","backend":"lab","instance":"instance-1.example.com:22"}
{"level":"INFO","msg":"session open","identity":"ihSSTfidE8FBRGaL","backend":"lab","instance":"instance-1.example.com:22"}
```

第二个身份（`identities` 里再加一把钥匙）拿到另一个端点。没登记过的钥匙在认证阶段就被拒：
`Permission denied (publickey)`。

**6. 看两个 HTTP 端点。** 可观测性只有这些：

```sh
curl -s localhost:8099/healthz                  # ok dev (none, unknown)
curl -s localhost:8099/metrics | grep '^byre_'  # byre_up 1, byre_leases_idle 1, byre_pool_warm{backend="lab"} 0
```

没有 `/readyz`。`/healthz` 回答的是"进程活着"，从来不是"我能发实例"。另外 `pool_size` 对 `endpoints`
无效，因为没有东西可以预热。

## 配置：后端的 sshd 从哪来

后端回答两个问题：实例怎么产生，怎么通过 SSH 够到它。第一个是 `provision`，取四个值，没有一个值写的是产品名。

| `provision` | 实例从哪来 | byre 需要什么 | 什么时候用 |
|---|---|---|---|
| `endpoints` | 你给的一批已存在的机器 | 什么都不需要，纯 TCP 拨号 | 机器自己管，或者只想先跑起来 |
| `workloads` | 在 Kubernetes 里创建 Pod | 在该 namespace 创建和删除 pod | 每个租约都该是一个新容器 |
| `claims` | 通过 claim API 创建对象 | 读写那个 CRD | 你已经在跑 claim 形状的控制器 |
| `command` | 你写一个程序，byre 调它 | 什么都不需要，进程执行 | 云主机、Incus/LXD、CMDB |

四种共有、并且刻意放在 provision 块之外的字段：`ssh.port`、`ssh.user`、`ssh.private_key`（网关自己的、装
在实例上的钥匙）、`ssh.host_key_policy`（长命主机用 `strict`，一次性实例用 `accept-new`），以及就绪超时。
无论哪种形态，"实例信任网关的公钥"这条都得你自己满足——byre 没法替你往 `endpoints` 主机上装，`workloads`
则要求它烘进镜像里。

下面四段是 backend 片段：放在 `backends:` 下，与快速开始里的 `listen`、`lease`、`identities` 并列，或者放进
`deploy/config.example.yaml`。

### endpoints——已存在的机器

```yaml
  - name: lab
    provision: endpoints
    endpoints: ["instance-1.example.com:22", "instance-2.example.com:22"]
    ssh: {port: 22, user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: strict}
```

不创建也不销毁：一个端点同一时间租给一个身份，在空闲端点里轮转，释放时标记为空闲。没有池，因为没有东西可以
提前创建——`pool_size` 只对能创建容量的 driver 有效。

两个后果。端点数量就是并发身份的容量上限，超了 `Acquire` 返回 `no capacity`。还有，释放后的端点里还留着上
一个身份的东西，清理是主机自己的事，所以一个身份一个端点才是唯一安全的默认。见
[`examples/static-endpoints/`](examples/static-endpoints/README.md)。

### workloads——一个租约一个 pod

```yaml
  - name: adhoc
    provision: workloads
    namespace: "<namespace>"
    provider:
      image: "<registry>/<image>:<tag>"
      start_command: ["/usr/sbin/sshd", "-D", "-e"]
      ready_timeout: 180s
    ssh: {port: 22, user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

实例是用一个跑 `sshd`、信任 pool key 的镜像创建出来的 Pod。就绪用 Kubernetes 自己的判据——报 `PodReady`
且有 `podIP` 才用——释放时删 Pod。这是四种里测得最少的一种（只有带假客户端的单测），记录的集群实测用的是
`claims`。

### claims——让控制器交一个出来

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
    ssh: {port: 22, user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

`claims` 是一种 API*形状*——创建对象、等 status 里出现地址、结束时删掉——不是内置的某个 CRD，所以 GVK
是配置项。就绪判据是**对 22 端口的 TCP 连接**：按 `endpoint_field` 给的顺序，再退到
`status.sandbox.podIPs`，第一个能连上的胜出；光有 status 字段不够，因为 Service 的 DNS 记录比控制器晚
几秒（ADR-0005）。本项目当初对着的部署用的是 `extensions.agents.x-k8s.io` 的 `SandboxClaim` 加热池，
那只是众多 claim API 中的一个，在这里也只是配置。

### command——你自己供给

```yaml
  - name: cloud
    provision: command
    command:
      acquire: ["/usr/local/bin/provision-ci-vm"]
      release: ["/usr/local/bin/destroy-ci-vm"]
      list: ["/usr/local/bin/list-ci-vms"]
      timeout: 120s
      env: ["CLOUD_PROFILE=ci"]
    ssh: {port: 22, user: "<remote-user>", private_key: /etc/byre/keys/id_key, host_key_policy: accept-new}
```

`acquire` 在 stdout 上只打印一个 JSON 对象——
`{"id":"vm-17","addr":"192.0.2.17:22","user":"ci","private_key":"/path/key"}`——环境里有 `BYRE_IDENTITY`；
`user` 与 `private_key` 可以省略，那时用 `ssh` 块里的值兜底。`release` 拿到 `BYRE_INSTANCE_ID` 与
`BYRE_INSTANCE_ADDR`。`list` 可选，打印同样对象的 JSON 数组；没有它，重启后的网关无法接管这个后端留下的
实例，留在外面的就一直留在外面。就绪判据就是打印出来的 `addr`——等待逻辑属于你的程序。完整契约与一份示例
脚本见 [`docs/handbook/backends.md`](docs/handbook/backends.md)。

一个身份就是一把客户端密钥。`fingerprint` 来自 `ssh-keygen -lf key.pub -E sha256`，`SHA256:` 前缀与
base64 补位都会被归一化，所以同一把钥匙的两种写法不会变成两个身份。`aliases` 只用于日志标签，从不参与路由。
没登记的钥匙永远过不了认证：没有匿名访问，也没有按连接兜底的默认值。

## 部署：Docker 与 Kubernetes

仓库里有容器镜像和 kustomize overlay；想先试试网关，两个都不需要。

```sh
make image                                            # byre:<git describe>，非 root，只读根文件系统
kubectl apply -k deploy/                              # ns、SA、RBAC、ConfigMap、Deployment、Service
kubectl apply -f deploy/instances-rbac.example.yaml   # 单独一条：那条授权在另一个 namespace
kubectl -n byre-system create secret generic byre-pool-key --from-file=id_key=/path/to/key
```

分开 apply 不是偷懒。`kubectl apply -k deploy/` 把网关装进它自己的 namespace，而它租的实例在另一个
namespace，kustomize 的 `namespace:` 又会一次性改写所有对象——所以那条授权只能单独 apply。漏掉它是这个项目
最容易误导人的故障：`/healthz` 一直是绿的，池从不预热，每次 checkout 都返回 403。

网关的 host key 放在自己的 PVC 上（`deploy/hostkey-pvc.yaml`），因为那是客户端 pin 的身份；pool key——byre
*连向*实例的那把——来自上面的 Secret。滚动 Deployment 时新进程先接管活着的租约，所以滚动期间重连的客户端
还能拿回同一实例，但 SSH 连接本身会被切断：没有 draining。安装、轮换、RBAC 与症状见
[`docs/handbook/operations.md`](docs/handbook/operations.md)。

## 和其它方案比

| 类别 | 代表 | 它解决什么 | 与 byre 的差别 |
|---|---|---|---|
| 企业级访问代理 | Teleport、HashiCorp Boundary | 人访问生产机：短期证书、审计、RBAC | 单位是人和已存在的主机；不分配、不回收实例 |
| SSH 反向代理 | [sshpiper](https://github.com/tg123/sshpiper) | 一个入口，按 username 或插件把连接路由到上游 sshd | 路由是静态映射；不创建也不租借，同一客户端下次可能落到别的上游 |
| microVM 租约网关 | [spoond](https://github.com/jrimmer/spoond) | 给 Firecracker microVM 加租约，带 SSH 网关、TTL 与重连 | 精神上最接近：客户端用 `<lease-id>@host` 或友好标签来寻址自己的租约，也就是一个它必须记住并提交的字符串；底座是它自己的 orchestrator |
| agent-sandbox 的 SSH 示例 | [Containarium SSH Sandbox](https://agent-sandbox.sigs.k8s.io/docs/use-cases/examples/containarium-ssh-sandbox/) | agent 只持 SSH 钥匙、不持集群凭据；每条会话被 forced command 钉进 MCP server | 会话被 forced command 钉住；没有身份租约、没有 TTL，沙箱按客户端拨的名字寻址 |

四者留下的缺口是同一个：三分钟后再回来的客户端，要么完全不被认出，要么只有重复一个别人早先发给它的标识符才
被认出。byre 取 SSH 已经验证过的那一个标识符——公钥——把租约挂在上面。代价写在[限制](#限制与已知问题)里。

## 限制与已知问题

- **单副本。** 租约表在一个进程里，第二个副本会让一个身份拿到两个实例。多副本设计写在 `docs/roadmap.md`，
  两个都没实现。
- **`/healthz` 不代表"我能发实例"。** 它回答"进程活着"，并打印编进二进制的版本号。缺跨 namespace 授权的
  网关照样是绿的，池从不预热，每次 checkout 都返回 403。
- **`provision: command` 没有 `list` 就不能接管。** 它创建的实例比进程活得久，而且没人回收。校验器不拒绝
  这种配置，因为它是合法的。
- **没有自动化端到端门禁。** `docs/acceptance/` 里的集群跑是人工执行并记录的；基于 `kind` 的 CI 还是
  roadmap 项，镜像与清单只做了服务端 dry-run 校验。
- **`endpoints` 活不过重启。** 这个 driver 把归属放在内存里，所以重启后的网关重新轮转，重连的客户端落到
  下一个端点上——这是实测，不是推断。`claims` 与 `workloads` 能活下来：身份被写在对象上。
- **`pool_size: 0` 停不掉实例创建。** `ApplyDefaults` 把显式的 0 当作没写，同样地，显式的 `grace: 0s`
  会被替换成 60s。今天要停就把 Deployment 缩到 0。
- **v1 不做：** 多副本一致性、动态后端负载均衡、实例内状态持久化、多租户、UI。任何某个产品特有的东西都属于
  集成示例。

## 文档

| 文档 | 回答什么 |
|---|---|
| [handbook/architecture.md](docs/handbook/architecture.md) | 身份、租约、池、后端、就绪判据怎么拼起来，以及为什么那些显而易见的修法都不行 |
| [handbook/backends.md](docs/handbook/backends.md) | 四种 provision 的细节：字段表、就绪判据、失败行为、`command` 的契约 |
| [handbook/operations.md](docs/handbook/operations.md) | 安装、RBAC、host key 轮换、metrics、升级、症状 |
| [handbook/troubleshooting.md](docs/handbook/troubleshooting.md) | 每种故障在客户端和网关日志里分别长什么样 |
| [roadmap.md](docs/roadmap.md) · [adr/](docs/adr/) | v1 明确不做什么、按什么顺序到来；每个决策以及反转它的条件 |
| [acceptance/](docs/acceptance/) · [docs/README.md](docs/README.md) | 真实集群上验证了什么；以及全部文档的索引 |

## 依赖

`golang.org/x/crypto/ssh` 负责协议；`k8s.io/client-go` 与 `api`、`apimachinery` 用于 `claims` 与
`workloads` 两种后端（另两种运行时完全不需要它）；`gopkg.in/yaml.v3` 解析配置文件。五个直接依赖，没有
gateway、proxy、cache 或 sidecar——一个静态二进制，两个监听。许可证见
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## 许可证

Apache-2.0——见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。
