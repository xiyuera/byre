# byre

[English](README.md) · **中文**

**基于租约的 SSH 网关。** 客户端出示公钥，就拿到一个实例，整个会话都用它；租约到期前这个实例由它独占。

> 状态：**M4** —— 租约网关已端到端可用：公钥身份、session/exec、`-L`、`-R`、热池、声明式 claim 与 pod
> 两种后端、重启后按注解接管，并已用**非默认身份**在真实集群上验收（见 `docs/acceptance/`）。
> 仍是单副本、没有自动化 e2e 门禁；M5 做加固。

> 项目由 `keypod` 改名而来（见 `docs/rename-to-byre.md`）。与任何同名项目**无隶属、无背书、无派生关系**；
> 这个名字的本义是牛棚，且许可证不授予任何商标权。

## 这是程序，不是库

`module byre` 是故意用裸路径的：本项目不打算被别人 import，写一个权威模块路径要么泄露内网域名，要么暗示
一个不会保持稳定的 API。**不支持 `go get`**；要复用请 fork 或 vendor 你需要的包。

## 为什么"按连接分配"是错的

任何一个 SSH 客户端在一次会话里都会开多条 TCP 连接：`exec` 一条、`scp`/`sftp` 一条、每条 `-L` 转发一条，
再加 `ControlPersist` 到期后的重连。如果网关按连接分配实例，那么对任何在会话内保留远端状态的客户端
（增量文件同步、工作目录、环境变量快照）：

| 发生的事 | 后果 |
|---|---|
| 连接落到实例 B，而状态同步在 A | 客户端"已同步"的账本是错的，它不会重传 |
| 两条连接之间实例被回收 | 下一条命令从一个空文件系统开始 |
| 实例被另一个身份复用 | 一个人的状态被另一个人看见 |

所以租约绑定的是**身份**，不是连接：

```
identity := sha256(客户端公钥)
lease(identity) -> instance      # 同一时刻唯一，独占
空闲 ttl 后到期，任何连接活动都会续期
```

## 为什么不"每次连接起一个容器"

ContainerSSH 那类做法是断线即销毁。对交互式 shell 没问题，但对 agent 的工作流是致命的：一个任务里
`git clone`、编译、跑测试是三次独立连接，中间没有任何"会话"可以挂靠。

## 快速开始

```sh
make build            # 静态二进制，版本号由 ldflags 注入
make test             # 单元测试（-race 在 CI 里跑）
./bin/byre -check-config -config deploy/config.example.yaml
```

部署：`kubectl apply -k deploy/`，**外加** `deploy/instances-rbac.example.yaml`（跨命名空间的实例授权不在
overlay 里，kustomize 的 `namespace:` 表达不了；缺它的症状是"网关健康但一个实例都创建不了"）。

## 文档

| 文档 | 回答什么 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | 身份、租约、池、后端、就绪判据怎么拼起来 |
| [docs/operations.md](docs/operations.md) | 怎么跑：配置、首次安装、轮换、常见症状 |
| [docs/release.md](docs/release.md) | 什么时候发版、流水线产出什么、GitHub 怎么镜像 |
| [docs/README.md](docs/README.md) | 全部文档的索引 |
| [docs/adr/](docs/adr/) | 每个决策，以及"什么条件下会反转它" |
| [docs/acceptance/](docs/acceptance/) | 在真实集群上验过什么，带命令与输出 |

## 依赖

Go 标准库 + `golang.org/x/crypto/ssh` + 官方 `k8s.io/client-go` + `gopkg.in/yaml.v3`（配置解析）。
**不引入**现成的 SSH 网关/代理/缓存组件。每个模块的许可证与许可文件摘要见
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## 文档语言

英文是权威版本，中文是同步翻译，用 `*.zh-CN.md` 后缀区分。翻译与原文的对应关系记录在
`docs/.i18n.json`，`scripts/check-i18n.sh` 会在原文变更而译文未更新时报红——见 [docs/i18n.md](docs/i18n.md)。

## 许可证

Apache-2.0，见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。
