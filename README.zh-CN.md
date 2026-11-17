# byre

[English](README.md) · **中文**

一个给"用完即弃的实例"做前门的 SSH 网关。客户端用公钥认证，这把公钥决定它拿到哪个实例；此后
只要它还在干活，这个实例就一直是它的，而不是跟着某条 TCP 连接的生死走。

> 状态：**0.3.0，1.0 之前**。身份、租约、热池与重启后接管都在真集群上验证过（`docs/acceptance/`）；
> 四种 provision 形态里只有 `claims` 有集群实跑，`workloads`、`endpoints`、`command` 只有带 fake
> client 的单元测试。仍是单副本，没有自动化 e2e 门禁，镜像与清单那条路径只做到了服务端 dry-run
> 验证。见[边界与已知问题](#边界与已知问题)。

> 项目由 `keypod` 改名而来（见 `docs/rename-to-byre.md`）。与任何同名项目无隶属、无背书、无派生关系；
> 这个名字的本义是牛棚，且许可证不授予任何商标权。

## 它要防的是什么

一个 agent 领到一件活：把工作树同步到机器上，编译，把产物拷回来，跑测试期间还要留一条端口转发。
这是一次有头有尾的会话，而它对 SSH 来说是四条 TCP 连接：

| # | 是什么 | 什么时候 | 它需要看见的状态 |
|---|---|---|---|
| 1 | 走 control master 的 `exec` | t+0s | 什么都没有，工作树由它创建 |
| 2 | `scp`/`tar` 同步，往往是自己一条裸 socket | t+40s | 第 1 条建的工作树，减去已经传过的文件 |
| 3 | 给测试套件用的 `ssh -L` | t+90s | 工作目录里那个刚编出来的二进制 |
| 4 | `ControlPersist` 到期之后，前三条里的任意一条 | t+5min | 以上全部 |

这四条连接每一条都从头认证一次。线上没有任何东西在说"我和三分钟前那个是同一个客户端"，除了那把
公钥。

现在假设网关按连接分配实例。第 1 条落到实例 A，传上去一棵几千个文件的工作树；客户端为每个文件记下
`{path: sha}`，此后不再重传没变过的文件。第 2 条落到实例 B。编译失败，或者更糟：因为脚本里有
`mkdir -p`，它在空目录上"成功"了。第 3 条把端口转发到第三个实例，那里根本没有那个二进制。客户端
没有任何错误可以报：从它的视角看每条命令都成功了，只是对端在两次调用之间忘掉了一切。

这才是这个 bug 难被发现的地方。客户端观察到的现象没有一条是错的，它的账本针对的是"某一个"文件
系统，而网关悄悄给了它四个。

修法必须把分配挂到某种比连接活得久的东西上。认证之前的 SSH 连接几乎不携带可用信息：版本横幅是
客户端自己写的文本，用户名是客户端侧的约定、常常为空或被多人共用，目的端口对所有人一样。认证之后
它携带一件稳定且可验证的东西，就是客户端证明了自己持有它的那把公钥。

## 四种看起来显然的修法，为什么都不行

### 按源地址粘滞

最省事的想法，两头都漏。NAT 后面一个地址被整个网络共用，同一台笔记本上的两个 agent 会变成同一个
客户端；而 NAT 又可能在空闲或重连之后改掉端口映射，所以 t+0s 时标识客户端的那个地址和端口，到
t+5min 时谁也不是。网关若要靠地址猜"是不是同一个客户端"，猜的是一个由别人的网络随手改写的值。

### 按 SSH 会话

没有会话可挂：会话标识是连接创建的。网关终结 SSH，它看到的恰好就是每次都被新建和销毁的那个对象，
也就是我们需要看穿的东西。重连、`scp`、每一条 `-L` 转发，按协议设计都是新连接、新会话。

### 断线即销毁实例

按连接起容器的后端就是这么做的，而且逻辑自洽：实例的寿命就是连接的寿命。代价是两次连接之间状态
无处存放，而那正是全部问题所在。它适合交互式 shell，不适合"由一连串连接组成"的任务。

### 要求客户端保持一条长连接（`ControlMaster`）

这办法可行，直到 `ControlPersist` 到期，而那正好是设计必须扛住的那次重连。它还等于把正确性变成
一份客户端配置：不这么做的客户端，或者像参考客户端那样为 `scp` 单开一条裸 socket 的客户端，会在
无声无息中出错。把硬性要求推给调用方，是网关给自己攒下一种它看不见的支持负担的方式。

## 它实际做的事

```
identity := sha256(客户端公钥)
lease(identity) -> instance            # 同一时刻唯一，独占
该身份的任何一条连接都会续租
只有空闲满 ttl 再加 grace 之后实例才被回收
```

租约表在池之前被查询，所以重连不可能和回收撞车。`ttl` 默认 360s，等于参考客户端 300s 空闲超时加
一点余量；`grace`（60s）是断开之后的重连窗口，`ControlPersist` 到期之所以对客户端不可见，靠的就是
它。同一身份的并发连接共享这个实例，即在一个目标上开多条 channel，这正是 SSH 本来的用法。

```
客户端 ──ssh(公钥)──> byre:2222
                       │ 1. auth:    publickey -> identity
                       │ 2. lease:   identity  -> lease（内存 + 实例注解）
                       │ 3. acquire: 先取池，再让后端创建
                       └─ 4. 用池密钥 ssh 连实例，拼接 channel
                              ├─ session          （pty / shell / exec）
                              ├─ direct-tcpip     （-L）
                              └─ forwarded-tcpip  （-R）
```

## 三条不变量，以及怎么自己验

每条都是对一台运行中的网关执行一条命令。它们是契约：哪一条不成立，就说明这个设计没做到本文说的
事。

### 租约内实例不变

连上去问实例自己叫什么，等 320 秒（比参考客户端的 300s 空闲超时长，比 `ttl` + `grace` 短），再问
一次：

```sh
ssh -p 2222 -i <client-key> <user>@<gateway> hostname    # byre-9ac151bc
sleep 320
ssh -p 2222 -i <client-key> <user>@<gateway> hostname    # byre-9ac151bc，否则就是坏了
```

第二行换了名字，说明分配挂在连接上，上面描述的整场事故就发生在你身上。

### 不同的 key 不会共用一个实例

六个身份、同时六条连接、六个不同的名字：

```sh
for k in 1 2 3 4 5 6; do ssh -p 2222 -i key$k <user>@<gateway> hostname & done; wait
# 实测：6/6 连接成功，6/6 互不相同
```

身份就是 key，所以这条同时也是"我是不是把一把 key 发给了两个 agent"的检查：在系统看来它们是一个
身份，会共享，按设计如此。

### 重启不丢租约

让一个客户端连着，重启进程但不碰客户端，然后去日志里找接管那一行，再确认实例名没变：

```sh
kubectl -n <ns> rollout restart deploy/byre
kubectl -n <ns> logs deploy/byre | grep 'adopted existing leases'   # adopted existing leases count=1
ssh -p 2222 -i <client-key> <user>@<gateway> hostname               # 与重启前同一个实例
```

如果重启时重建了实例，被毁掉的恰好是租约本来在保护的那部分状态，"我们滚动一下网关"就变成了
"所有 agent 都丢了工作树"。

这几个数字的出处是 `docs/acceptance/2026-11-17-m4.md`，连同产生它们的实跑记录。
`test/e2e/cluster-acceptance.sh` 可以对你自己部署的网关跑第一条（加 `--ttl-ok` 会带上那 320 秒的
等待），并检查 `-L` 能把流量送进实例；第三条它断言不了——需要你自己重启网关，再在日志里找
`adopted existing leases`。

## 代价

key 就是身份，所以共享 key 等于共享实例。两个 agent 拿着同一把私钥就是一个客户端：它们会抢同一个
实例、看见彼此的文件、互相等对方。这是设计在正常工作，代价是访问控制被推到了密钥分发上；配置里的
`aliases` 只改日志标签，永远不参与路由。换一把 key 就是一个新身份，也意味着一次冷启动。

空闲是按墙钟算的。一个 agent 想事情想过了 `ttl` + `grace`，回来看到的就是一个新实例和一个空目录，
因为系统里没有任何东西知道它在思考。旋钮是有的（`lease.ttl`），把它调到客户端自身空闲超时之上是
运维职责，不是一个能适配所有人的默认值。

实例不是存储。需要活过一次租约的文件，就得放到比实例活得久的地方，由客户端自己同步过去。

## provision 是一套词汇

一个后端只回答一个问题：实例怎么出现，以及我怎样用 SSH 够到它。答案就是 `provision` 的四个取值
之一，没有一个取值是产品名。

| `provision` | 实例怎么出现 | 驱动 |
|---|---|---|
| `claims` | 有控制器在你开口时交出一个；API 坐标属于配置 | kubernetes |
| `workloads` | 我们自己从一个跑着 sshd 的镜像创建工作负载 | kubernetes |
| `endpoints` | 完全不 provisioning：一批已经存在的主机 | static |
| `command` | 由外部程序 provisioning 并描述实例 | command |

"怎么够到实例"这件事是共享的，放在 provision 块外面：`ssh.port`、`ssh.user`、`ssh.private_key`、
`ssh.host_key_policy`，以及就绪超时。只要是说 SSH 的后端就是后端，不管它是谁造出来的。

```yaml
listen: ":2222"

lease:
  ttl: 360s            # 要大于你自己客户端的空闲超时
  grace: 60s           # 重连窗口，吸收 ControlPersist 到期
  pool_size: 2         # 保持热的实例数

backends:
  - name: pooled
    provision: claims
    namespace: "<namespace>"
    provider:
      template: "<template-name>"
      warm_pool: "<warm-pool-name>"
      api_group: "<api-group>"          # claim API 是配置，从不硬编码
      api_version: "<api-version>"
      resource: "<plural-resource>"
      object_kind: "<Kind>"
      endpoint_field: ["status", "sandbox", "serviceFQDN"]
    ssh:
      port: 22
      user: "<remote-user>"
      private_key: /etc/byre/keys/id_key
      host_key_policy: accept-new

identities:
  - fingerprint: "SHA256:<base64-body>"   # ssh-keygen -lf key.pub -E sha256
    backend: pooled
```

`claims` 指的是一种 API 形状：创建一个对象，等它 status 里出现一个地址，用完删掉。它不是某个具体
的 CRD。`api_group`、`resource`、`object_kind` 这三个字段存在的意义，就是同一个二进制能驱动任何
claim 形状的 API，包括这里没人见过的。写这个项目时部署的那套集群用的是
`extensions.agents.x-k8s.io` 的 `SandboxClaim` 加一个热池；那只是一个例子，而且在这份文件里它是
配置，不是代码里的名字。配置写旧了也不是 schema 报错：`backend.type` 被读到只有一个用途，就是告诉
你它已被 `backend.provision` 取代。

另外三种形态在 `deploy/config.example.yaml` 和 `examples/` 里：一个跑 sshd 的镜像、一批已经存在的
主机、一个替你 provisioning 别的东西的程序。`command` 是我能写出的契约最小的逃生口：打印一个 JSON
对象（`{"id","addr","user?","private_key?"}`），可选地实现 `list`，好让重启后的网关能接管它之前
留下的实例。

完整参考：[`docs/architecture.md`](docs/architecture.md)；决策：
[`docs/adr/`](docs/adr/)；怎么跑：[`docs/operations.md`](docs/operations.md)。

## 边界与已知问题

- 单副本。租约表在一个进程里，所以第二个副本会让同一个身份拿到两个实例。多副本的方案写在
  `docs/roadmap.md` 里，两个都没实现。
- `/healthz` 不等于"我能发出实例"。它回答的是"进程活着"，并打印编进二进制的版本号。缺了那条跨
  命名空间授权的网关是绿的，池永远热不起来，每次取实例都回 403，这是本项目最有误导性的故障，
  `docs/operations.md` 把它放在最前面。
- `provision: command` 不带 `list` 就无法接管。它创建的实例会比进程活得久，而且没人回收。
  `deploy/config.example.yaml` 的注释里写了这件事；校验器不拦它，因为不带 `list` 的 command
  后端是合法配置。
- 没有自动化 e2e 门禁。`docs/acceptance/` 里的集群实跑是人工执行并记录的，基于 `kind` 的 CI 仍是
  roadmap 条目。镜像和 `kubectl apply -k` 这条路径只做了服务端 dry-run（`?dryRun=All`）：构建环境
  里没有容器运行时，没能真的把镜像跑起来。
- `pool_size: 0` 并不能阻止创建实例。`ApplyDefaults` 把显式的 0 当成没写。今天要停下来，只能把
  Deployment 缩到 0 副本。

## 同类项目，以及为什么这里没有用它们

这些项目在它们各自的位置上都很好；它们没有一个是租约管理器，复用一个的结果还是得把缺的那一半写完。

- **sshpiper** 把连接转发到一个由请求（通常是用户名）选出的上游。它从不创建上游，所以没有东西可
  租；而按用户名路由又把一个客户端自选、未经校验的标识符请了回来。
- **ContainerSSH** 为一条 SSH 连接起一个容器，连接结束就删掉。生命周期是连接的，这正是本项目在
  agent 工作负载上反对的模型，同时也是 shell 场景下正确的模型。
- **Teleport** 把人和机器认证到已经存在的服务器上，并记录发生了什么。它回答"谁可以到哪台机器"，
  不回答"这个身份今天该拿到哪台机器"。
- 这一层里被本项目替换掉的那类路由器，在连接到达时就做决定。无论它按什么决定（请求里的名字、端口、
  某个头），决定是一次连接一次，于是下一条连接就是新的一次决定。要让这些决定彼此一致，需要一份比
  连接活得久的登记表，那就是功能更少的租约表。

## 这是程序，不是库

`module byre` 是故意用裸路径的：本项目不打算被别人 import，写一个权威模块路径要么泄露内网域名，
要么暗示一个不会保持稳定的 API。**不支持 `go get`**；要复用请 fork 或 vendor 你需要的包。

## 构建

```sh
make build      # bin/byre
make test       # 单元测试
make lint       # gofmt + go vet（装了 golangci-lint 也一起跑）
./bin/byre -check-config -config deploy/config.example.yaml
make e2e        # 对着 kind 集群跑端到端（需要容器运行时）
```

部署：`kubectl apply -k deploy/`，**外加** `deploy/instances-rbac.example.yaml`。在另一个命名空间
创建实例的授权没法放进 overlay，因为 kustomize 的 `namespace:` 会一次性改写所有对象。漏掉它会怎样，
见上面"边界与已知问题"。

## 接下来读什么

| 文档 | 回答什么 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | 身份、租约、池、后端、就绪判据怎么拼起来 |
| [docs/operations.md](docs/operations.md) | 怎么跑：配置、首次安装、轮换、症状排查 |
| [docs/roadmap.md](docs/roadmap.md) | v1 明确不做什么，以及它们按什么顺序到来 |
| [docs/release.md](docs/release.md) | 什么时候发版、流水线产出什么 |
| [docs/adr/](docs/adr/) | 每个决策，以及"什么条件下会反转它" |
| [docs/acceptance/](docs/acceptance/) | 真实集群上验过什么，带命令与输出 |
| [docs/integrations/](docs/integrations/) | 客户端运行时如何对接网关 |
| [docs/README.md](docs/README.md) | 以上全部的索引 |

## 依赖

五个直接依赖；Kubernetes 客户端自带一套（约 40 个）同一项目的传递依赖。二进制里链接了什么、为什么：

| 模块 | 作用 | 能否避免 |
|---|---|---|
| `golang.org/x/crypto/ssh` | SSH 协议（服务端与客户端） | 不能，自己实现 SSH 是这个项目唯一不该做的事 |
| `k8s.io/client-go`、`k8s.io/api`、`k8s.io/apimachinery` | 用客户端证书与 Kubernetes API 对话 | 对 `claims` 与 `workloads` 不能；`endpoints` 与 `command` 运行时一个都不需要 |
| `gopkg.in/yaml.v3` | 解析配置文件 | 能，代价是配置只能用 JSON |

没有引入任何网关/代理/缓存组件：没有 sshpiper、没有 Envoy、没有 Redis、没有 sidecar。进程就是一个
静态二进制，两个监听端口（SSH 与健康/指标）。各模块的许可证，以及读到这些条款的那个许可证文件的
摘要，见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## v1 的非目标

多副本一致性、动态多后端负载均衡、实例内状态持久化、多租户、UI，以及任何专属于某个产品的功能
（那些属于集成示例）。

## 许可证

Apache-2.0，见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。
