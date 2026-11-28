---
description: "运行 byre 的完整操作手册：首次安装与部署、不在 overlay 里的集群权限、主机密钥轮换、两级镜像仓库与 pull 验证、健康检查与完整指标表、lease 存储的开启与回滚、可调项与命令行参数、安全重启、滚动升级和卸载，以及对运行中 gateway 的冒烟测试与常见症状处置（含日志行与计数器）。"
---

# 运维

这是操作手册：它假定你已读过 README 的快速开始，现在想真正运行 byre。
每个 backend 的字段级细节在 [backends.md](backends.md)，逐项故障指南在
[troubleshooting.md](troubleshooting.md)。

## 部署

```sh
make image-push IMAGE_REGISTRY=<registry>/<team>   # 构建、推送，并记录 dist/images.txt
# 在 deploy/ 里填好镜像引用（见下），然后：
kubectl -n byre-system create secret generic byre-pool-key --from-file=id_key=/path/to/key
kubectl apply -k deploy/
kubectl -n byre-system set image deploy/byre byre="$(sed -n 's/^IMAGE_REF=//p' dist/images.txt)"
make image-verify                                  # digest 与 registry 必须和运行中的 pod 一致
kubectl -n byre-system rollout status deploy/byre
```

`deploy/deployment.yaml` 里的镜像引用是一个**占位符**（`<registry>/byre@sha256:<digest>`），
不是可用的默认值：本项目没有自己的 registry，而裸名字（`byre`）会让 kubelet 去
Docker Hub 找 `library/byre`——一个读起来像 manifest 损坏的 ImagePullBackOff。填它的方式有两种：
用一个 overlay，其 `images:` 块里写的引用与 manifest 中**完全一致**
（kustomize 按字面匹配字符串，因为 `sha256:<digest>` 不是它能解析的 digest）；或者用
`kubectl -n byre-system set image deploy/byre byre=<registry>/byre@sha256:<digest>`。

按 **digest** 固定，不是按 tag。tag 是可变的指针：同一个 tag 可以用不同内容重新推送，
而在 `imagePullPolicy: IfNotPresent` 下节点会保留已缓存的层，于是 Deployment
报告新 tag，进程却还是旧的构建。digest 是内容哈希——内容变了，值就变，
pull 不可能悄悄返回别的东西。`/healthz` 打印编译进二进制的版本，这是判断
实际运行的是哪个构建的办法。

### 发布页携带要固定的 digest

每个带 tag 的发布都在二进制旁边发布 `images.txt`：`IMAGE_REF` 是 Deployment
应当携带的引用，`IMAGE_DIGEST` 是该发布构建时 registry 对这个 tag 的回答。
`scripts/record-release-image.sh` 通过询问 registry（`HEAD
/v2/<image>/manifests/<tag>`）写入它，然后取回 manifest 并对其取哈希，因为 header 是对 body
的声明，两者不一致意味着哪一个都不足以安全固定。

它**不**被 `SHA256SUMS` 覆盖，这是刻意的：`SHA256SUMS` 覆盖的是重新构建能复现的东西，
而 `images.txt` 记录的是 registry 在某一刻的回答——一个任何重建都复现不出来的值。
tag 从未推送过的发布会在那一步失败，而不是发布一个猜出来的 digest，所以**先推
该 tag 对应的镜像，再推 tag**。`PUSHED_AT` 为 `unknown` 也是同样的理由：registry 的 manifest
不携带推送时间戳，编造一个就会成为以后所有人都信任的字段。

从发布资产部署，而不是从执行 rollout 的机器上 `dist/` 里的任何东西部署：

```sh
curl -fsSLO <release-page-url>/images.txt        # 该资产，不是本地构建的记录
kubectl -n byre-system set image deploy/byre byre="$(sed -n 's/^IMAGE_REF=//p' images.txt)"
make image-verify IMAGES=images.txt              # digest 和 registry 前缀，都对运行中的 pod 比
```

`make image-verify` 在无法完成检查时退出码为 2——只有一个裸 digest 而没有 registry
可比——因为「无法检查」不能读作「检查过且没问题」。只比 digest 不够：当同样的 blob
已存在于另一个 registry 名下时，containerd 什么都不下载，`imageID` 继续报告
旧仓库，而 pod 运行的并不是发布页所指的东西。

**pool 密钥 Secret 刻意不包含在 `kubectl apply -k deploy/` 里**——在这里从文件创建它
会把私钥写进 git。在首次 rollout 之前，在同一个 namespace 里创建它：

```sh
kubectl -n byre-system create secret generic byre-pool-key --from-file=id_key=/path/to/pool_key
```

跳过它不会大声失败：Deployment 挂载该 Secret 的 volume 不是可选的，所以 Pod
停在 `CreateContainerConfigError`，永不启动。这就是预期的失败模式——另一种做法
（把 volume 设为可选）会让 gateway 在没有密钥的情况下启动，然后每次 acquire 都以一个
误导性的 SSH 错误失败。

`deploy/` 创建：ServiceAccount、Role/RoleBinding（在目标 namespace 里）、Deployment（SSH
前端在 2222，健康检查在 8099）、Service（ClusterIP 2222）。配置来自 ConfigMap；pool
密钥来自上面的 Secret。两者都是示例——复制，然后编辑。

## 镜像仓库：两级

两个仓库服务这个集群，它们不可互换。下文里，`<your-gitea-host>:<port>`
是你 Gitea 服务的容器仓库端点——发布推送到的那个——而
`<internal-registry-host>:<port>` 是一个位于集群网络上的临时仓库。

| 仓库 | 角色 | 认证 | 适合放什么 |
|---|---|---|---|
| `<your-gitea-host>:<port>`（Gitea 容器仓库） | 发布的事实来源 | token——推送需要 `write:package`，拉取需要 `read:package` | 任何工作负载要运行的东西：带版本号的 tag、digest，可追溯到某个仓库 |
| `<internal-registry-host>:<port>`（`registry:2`，自签 CA） | 临时空间 | 无——允许匿名推送 | 一次性实验、手工构建、下个月没人再要的镜像 |

规则只有一行：**任何进入生产的东西都来自 Gitea 仓库**。临时仓库里的镜像是
默认临时的，可能被无通知删除。保留而不是退役这个临时仓库，是因为它已经会说
v2 API，比让所有人为了偶尔手工构建的镜像去学 `docker save` / `ctr import` 更便宜。

### 仓库名不同

Gitea 要求 `owner/image`——两段路径，其中 `owner` 是用户或组织，而
镜像名与任何仓库名无关。因此迁移重命名了单段仓库：

| 临时仓库 | Gitea |
|---|---|
| `byre` | `byre/byre` |
| `gitea/runner:6.0.0` | `byre/runner:6.0.0` |
| `hermes-sandbox-desktop:ssh-uid10000-v19` | `byre/sandbox-desktop:ssh-uid10000-v19` |
| `library/ci-go:1.23` | `byre/ci-go:1.23` |
| `library/golang:1.23-alpine` | `byre/golang:1.23-alpine` |

容器包**不**链接到仓库（`repo_id = 0`）：它们出现在
组织的 packages 页面下（`/byre/-/packages/container/<image>`），而仓库的 Packages 标签页
是空的。删除仓库不会删除它的镜像，而删除镜像是
组织级操作。

### 在集群内从 Gitea 拉取

节点上什么都不用改。该仓库出示的是 `<your-domain>` 的 Let's Encrypt 证书，
每个节点都已信任它（`openssl s_client -verify_return_error` 在每个节点上返回 0），
所以 `registries.yaml` 不需要为它添加 CA 条目——因此**不需要 `systemctl restart k3s`**。
临时仓库的现有条目保留，因为那个仓库是自签的。

这是刻意的：`registries.yaml` 里的凭据就是每个节点磁盘上的凭据。拉取
凭据应当放在 `kubernetes.io/dockerconfigjson` Secret 里，那正是
`imagePullSecrets` 的用途：

```sh
kubectl -n <namespace> create secret docker-registry gitea-registry \
  --docker-server=<your-gitea-host>:<port> \
  --docker-username=<gitea user> \
  --docker-password=<token with read:package only>
```

然后从工作负载里引用它（`spec.imagePullSecrets`），或者为了 namespace 里每个 pod，从
默认 ServiceAccount 引用。三个 namespace 需要它：`byre-system`、`hermes`（CI runner、sandbox
模板）和 `agent-sandbox-system`。

拉取 token 是专用的：只有 `read:package`，别的都不要。能推送的 token 让
持有者可以替换生产运行的东西，节点没有理由持有这样一个 token。

### 证明 pull 确实走了 Gitea

`Successfully pulled image` 事件不是证据：当同样的 digest 在本地已存在——
临时仓库和 Gitea 持有逐字节相同的 blob——containerd 什么都不下载，节点甚至可能
在 `imageID` 里报告旧仓库名。诚实的检查用一个其层**只**存在于 Gitea 的镜像：

```sh
kubectl -n hermes run pullprobe --image=<your-gitea-host>:<port>/byre/pullprobe:v1 \
  --overrides='{"spec":{"imagePullSecrets":[{"name":"gitea-registry"}],"nodeName":"<node>","restartPolicy":"Never"}}'
kubectl -n hermes get pod pullprobe -o jsonpath='{.status.containerStatuses[0].imageID}'
```

`byre/pullprobe:v1` 是一个 162 字节的层，专为此保留。容器没有 shell，所以以
`Failed` 结束的 pod（exec `/bin/sh`）是预期的；要紧的是在一次非零下载之后
`imageID` 指向 `<your-gitea-host>:<port>`。用完删除该 pod。

## 构建并发布镜像

```sh
make image                                        # byre:<git describe>；只落在本地 daemon
make image-push IMAGE_REGISTRY=<registry>/<team>  # 推 <registry>/<team>/byre:<tag>；见下面的 digest
# 然后在两处都填上占位符——它们按字符串匹配，见 deploy/kustomization.yaml：
#   deploy/deployment.yaml:39          image: <registry>/byre@sha256:<digest>
#   deploy/kustomization.yaml images:  name: <registry>/byre@sha256:<digest>, newName: …, digest: …
```

`make image-push` 通过 `scripts/publish-image.sh` 推送，后者再把 registry 分配的
digest 写入 `dist/images.txt`——`IMAGE_REF`（要固定的引用）、`IMAGE_TAG`、`IMAGE_DIGEST` 和
`PUSHED_AT`——并在该发布资产已存在时把 `IMAGE_DIGEST=` 追加到 `dist/build-info.txt`。
一次没有返回 digest 的推送会非零退出，而不是留下一个只是看起来完整的文件。
`make image-verify` 把运行中 pod 的 `imageID` 与该 digest 比较，不一致时非零退出：
这是抓出「重新推送了 tag 而节点早已有那些层」的检查。

上面两步都不是可选的，而两者失败的方式都不会提到它们。`make image` 只喂
本地容器运行时，而 manifest 发布的是 `<registry>/byre@sha256:<digest>`——一个 kubelet
无法解析的占位符，所以没填的 `deploy/` 会停在 `ImagePullBackOff`。缺少 `byre-pool-key`
Secret 更糟：该 volume 不是 `optional`（`deploy/deployment.yaml:77-80`），所以 pod 永不启动，
停在 `CreateContainerConfigError`。

镜像是 Alpine 上的静态二进制；没有安装其他东西。镜像自己的用户是 `10001:10001`，
而随附的 Deployment 以 uid `65532` 运行容器、`fsGroup: 10001`
（`deploy/deployment.yaml:23,27`）。让 `0400` 的 Secret 挂载可读的是 `fsGroup`——决定权在
属主，不在 uid——这个配对已在真实集群上于
`docs/acceptance/2026-11-17-deploy.md` 验证。两个数字应当一致；在 manifest 统一到
`10001` 之前，描述容器时以镜像声明的和使它能工作的 `fsGroup` 为准。

### 主机密钥

gateway **自己的** SSH 主机密钥存放在 `byre-hostkey` PVC 上（`host_key:
/var/lib/byre/hostkey/id_ed25519`）。首次启动时文件不存在，gateway 会创建它，
并记录指纹（`generated a new host key and persisted it`）。把这个指纹分发给
客户端；此后重启保持同一 identity。

要改用你自己的密钥，把一个 Secret 挂载到该路径——已存在的文件永远不会被重写。

轮换（刻意如此）：

```sh
kubectl -n byre-system scale deploy/byre --replicas=0
kubectl -n byre-system exec <debug-pod> -- rm /var/lib/byre/hostkey/id_ed25519   # 或者替换该文件
kubectl -n byre-system scale deploy/byre --replicas=1
kubectl -n byre-system logs deploy/byre | grep fingerprint                       # 发布新的指纹
# 在每台客户端上：
ssh-keygen -R '[<gateway-host>]:<port>'
```

如果构建主机无法访问基础镜像 registry，先构建二进制并使用预构建
变体，它不拉取任何东西（`FROM scratch`）：

```sh
CGO_ENABLED=0 GOOS=linux go build -trimpath -o bin/byre ./cmd/byre
podman build -f Dockerfile.prebuilt -t <registry>/byre:<tag> .   # docker 用法相同
```

按 **digest** 固定镜像，不是按 tag：在 `imagePullPolicy: IfNotPresent` 下，重新推送的 tag
会保留节点缓存的层，于是 Deployment 报告新 tag，运行中的进程仍是旧
构建（`/healthz` 显示真相——它打印烙进二进制的版本）。

```sh
podman push --digestfile /tmp/digest <registry>/byre:<tag>
kubectl -n byre-system set image deploy/byre byre=<registry>/byre@<digest>
```

### 那个不在 overlay 里的授权 {#the-grant-that-is-not-in-the-overlay}

`kubectl apply -k deploy/` 把 gateway 安装在**它自己的 namespace** 里。它 lease 的
instance 的创建和删除发生在*另一个* namespace，而 kustomize 的 `namespace:` 会一次性重写
每个对象，所以那个授权是单独的 apply（`deploy/instances-rbac.example.yaml`）。

跳过它会产出本项目最具误导性的故障：`/healthz` 是绿的，pool 从不
预热，每次 checkout 返回 403——读起来像是「backend 挂了」。当 gateway 活着但发不出
instance 时，第一个就查它：

```sh
kubectl -n <instances-ns> auth can-i patch <plural-resource> --as system:serviceaccount:<gateway-ns>:byre
```

`<plural-resource>` 是 backend `provider` 块里的 `resource`，API group 就是该块
写的东西——gateway 两者都不硬编码。本项目写作时基于的部署用的是
`extensions.agents.x-k8s.io` 的 `sandboxclaims`；那只是众多 claim API 之一，不是要求。

**如果 `pool_size > 0`，** gateway 会立即开始创建 instance。首次冒烟测试若还不想
要热容量，在 ConfigMap 里设 `pool_size: 0`；该值现在是显式的（字面量 0 不再被改写成
默认值 2），所以那种模式下 `/readyz` 按构造就是绿的。显式的 `grace: 0s` 也按原文
保留：零重连窗口是被接受的，它只意味着断开即结束 lease 共享。而且该设置只对
实现了 `Create` 的 driver 生效——`endpoints` 和 `command` 从不预热任何东西。

kustomize 生成的 ConfigMap 名字里带内容哈希（`byre-config-<hash>`）。不要
把 `kubectl get cm byre-config` 写进 runbook——它会 404。从 Deployment 读名字：

```sh
kubectl -n byre-system get deploy byre -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}'
```

## 发布前验证配置

```sh
byre -config /etc/byre/config.yaml -check-config
```

文件有问题时非零退出并列出*每一项*违规（不是第一条）：未知字段、
未知或缺失的 `provision` 值、该 provision 需要而文件省略的字段、重复的
指纹、畸形的 endpoint、缺失的必需值、在零无法工作处的显式零（`ttl`、`ssh.port`、
`ready_timeout`），以及解码后不是 32 字节的指纹。

它检查配置，不检查集群：在 `-check-config` 下，Kubernetes driver 被
校验并构建，但不带 API 客户端，所以它能在没有凭据的工作站上跑。集群
是否会接受这些对象是另一个问题——那个问题由上面的 `auth can-i` 检查和
第一次 acquire 回答。

它也看不见意图。`deploy/config.example.yaml` 以 `rc=0` 通过，而每个值仍是
`<placeholder>`、指纹是 32 个零字节：校验器检查形状（base64、`host:port`、
已知的 `provision`），不检查你是否打算编辑这个文件。

它还会探测 `host_key`：已存在的路径必须是文件，不存在的路径必须能在
最近的存在祖先目录里创建，因为首次启动时无法写入生成的密钥会死在一半。
`host_key` 本身是**必需的**：没有它，每次重启都会改变客户端固定的指纹，每个
开启严格主机密钥检查的客户端都会停止连接。`-allow-ephemeral-hostkey` 只用于测试和实验
运行；它是命令行参数，永远不是配置键，所以部署无法通过编辑自己的
ConfigMap 来退出这条要求。即使带这个参数，省略也会作为警告打印。

## 健康检查

`:8099` 上有三个路径：

| 端点 | 问题 | 使用者 |
|---|---|---|
| `GET :8099/healthz` | 进程活着吗？打印二进制里的 `ok <version>` | livenessProbe |
| `GET :8099/readyz` | 这个进程能 provision instance 吗？ | readinessProbe |
| `GET :8099/metrics` | Prometheus 文本 | 抓取 |

没有 pool、或至少一个 instance 是热的、或最后一次填充创建了一个时，`/readyz` 为 200；
否则为 503，其 body 写出 pool 目标、热数量和最后的错误（ADR-0011）。
随附的 manifest 用 `/healthz` 做 liveness、`/readyz` 做 readiness——无法
provision 的 rollout 保持红色，而不是在一个坏掉的 pool 背后变绿。`/readyz` 仍然
*不*说的是：它发出的 instance 可能以只有真实会话才会暴露的方式坏掉（见
[那个不在 overlay 里的授权](#the-grant-that-is-not-in-the-overlay)和 ADR-0005）。

端口是编译进去的：`&http.Server{Addr: ":8099"}` 在 `cmd/byre/main.go:147`，没有配置键也
没有参数。两个后果：同一台主机上的第二个 gateway 无法启动——它绑定 SSH 监听器，
记录 `health endpoint failed: listen tcp :8099: bind: address already in use` 然后退出——而
Deployment 绝不能让每个节点上调度两个副本（它本来就是设计成单副本的）。

完整指标列表：

| 指标 | 类型 | 含义 |
|---|---|---|
| `byre_up` | gauge | 进程在服务时恒为 1 |
| `byre_build_info{version,commit,build_date}` | gauge | 实际在跑什么（`/healthz` 打印同一个戳） |
| `byre_leases_active` | gauge | 至少有一条打开连接的 lease |
| `byre_leases_idle` | gauge | 没有连接、等待 reaper 的 lease |
| `byre_leases_adopted` | counter | 启动时从已有 instance 恢复的 lease |
| `byre_leases_adopted_pending` | gauge | 尚未被任何连接接手的 adopted lease |
| `byre_oldest_idle_lease_age_seconds` | gauge | 最老的空闲 lease；超过 `ttl+grace` 还在增长说明 reaper 卡住了 |
| `byre_pool_target{backend}` | gauge | 这个 backend 试图维持的热容量（0 = 无 pool） |
| `byre_pool_warm{backend}` | gauge | 每个 backend 的热 instance；没有 pool 的 driver（`endpoints`、`command`）为 0 |
| `byre_pool_inflight{backend}` | gauge | 进行中的创建 |
| `byre_pool_fill_seconds` | histogram | 一次填充的时长，包括其中的失败 |
| `byre_pool_fill_errors_total` | counter | 一次填充中失败的 instance 创建 |
| `byre_handshake_fail_total` | counter | 端口应答了但 SSH 没有的 readiness 探测 |
| `byre_release_errors_total` | counter | 失败的 release 尝试（一次 release 最多可计三次） |
| `byre_acquire_total{backend,result}` | counter | 按结果统计的 acquire：`reuse`、`pool`、`created`、`error` |
| `byre_process_resident_memory_bytes` | gauge | 来自 `/proc` 的 RSS；内存上限按这个数判断 |

告警：用 `byre_pool_warm < byre_pool_target`，**并且只在 `byre_pool_target > 0` 时**——静态
backend 没有 pool，天真的比较会永远触发。

## 开启 lease 存储

默认情况下 lease 表活在内存里：重启即清空，每个 identity 冷启动，而
它此前用的 instance 还在运行。设置 `lease.store: crd` 把表搬进 `ByreLease` 对象，
于是重启——或第二个副本——读到同一份事实（ADR-0013）。

**前置条件，两者都能在改动任何配置之前检查：**

```sh
kubectl get crd byreleases.byre.io
kubectl auth can-i list byreleases.byre.io -n <namespace> \
  --as=system:serviceaccount:<namespace>:byre
```

CRD 缺失就 apply `deploy/`（它包含 `crd/byreleases.yaml`）。第二条命令回答的是
「*pod 的* identity 能不能做这件事」，那才是要紧的——管理员 kubeconfig 在这里证明不了任何东西。

**改动本身**是 backend 配置里的两行：

```yaml
lease:
  store: crd
  namespace: <the namespace the instances live in>
```

`lease.kubeconfig` 只在没有任何 backend 已有集群客户端时才需要。有的话，它的客户端会被
刻意复用：两个客户端可能指向两个不同的集群。

**按这个顺序验证。** 每一步失败的方式都不同，直接跳到最后一个会掩盖是哪一步：

1. **启动。** 日志行是 `lease store kind=crd namespace=…`。如果它是
   `could not read the lease table at startup`，说明 pod 的 identity 缺少上面的 RBAC——而按设计
   gateway 仍然启动，降级到 annotation 恢复，而不是拒绝每个会话。
2. **一条连接。** `kubectl get byreleases -n <ns>` 显示每个存活 lease 一个对象，携带
   `spec.identity`、`spec.instance`，以及在配置了 `identities[].name` 时的 `spec.name`。配置了名字而
   `spec.name` 为空，说明指纹拼写不匹配——见该 store 的 `identityNames` 里的说明。
3. **重启。** 启动日志对 N 个存活 lease 报告 `adopted: N`，来自同一
   identity 的重连返回**同一个** `spec.instance`。这就是全部要点；在 ADR-0013 之前，这一步
   会让大家全部冷启动。只靠注解才知道的 lease 会在此刻补写它的 `ByreLease`，所以
   `kubectl get byreleases` 立刻就能看到每个被接管的 lease 一行；如果这次写入失败，
   `byre_leases_unpersisted` 会说明，sweep 会重试（ADR-0015）。
4. **删除一个 instance 对象。** 几秒内 `kubectl get byreleases` 丢掉那一行，下一条
   连接拿到一个全新的 instance，无需重启。这检验了 watch（第 4 部分）。

**回滚**是移除 `lease.store` 并重启：表回到内存，此后 CR 被
忽略。把它们留在原处——它们是回滚那一刻每个 identity 在用哪个 instance 的唯一记录，
重新启用 store 会再次接上它们。

**之后要观察什么：**

- `byre_leases_unpersisted` 大于零说明表和 store 不一致。它会在下一次
  成功写入时自愈（sweep 会重试），所以一次尖峰是 apiserver 抖了一下；持续非零是权限或
  CRD 问题，错误日志会点出来。
- `byre_persist_errors_total` 持续上升而没有恢复，说明写入直接失败。
- 处于 `lease fail` 的 instance 与仅仅不可达的 instance 分开计数。

## 可调项

| 键 | 默认值 | 说明 |
|---|---|---|
| `lease.ttl` | `360s` | 必须超过客户端的空闲超时（参考客户端：300s） |
| `lease.grace` | `60s` | 断开后的重连窗口；吸收 `ControlPersist` 过期 |
| `lease.pool_size` | `2` | 热 instance；空闲时每个占一个 pod 的内存。`0` 是真实取值：没有热容量，每次 acquire 都按需 provision |
| `provider.ready_timeout` | `120s` | 慢镜像（冷层拉取）时调高 |
| `provider.run_as_root` | `false` | 工作负载 instance：默认以非 root 运行，丢弃所有 capability，`RuntimeDefault` seccomp；仅当镜像的 entrypoint 确实需要 uid 0 时设为 true |
| `ssh.host_key_policy` | `accept-new` | `strict` 需要 known_hosts；`insecure` 只用于一次性 instance |
| `provider.api_group/api_version/resource/object_kind` | — | `provision: claims` 需要：claim API 是配置，永不硬编码 |
| `provider.endpoint_field` | `status.sandbox.serviceFQDN` | claim 形状的 API 暴露其 SSH 地址的位置；裸主机补 `:22`。只是默认值——设成你 API 的路径 |
| `provider.image` | — | `provision: workloads`：任何运行 sshd 并信任 pool 密钥的镜像 |
| `command.acquire/release/list` | — | `provision: command`：创建、销毁并（可选）列出 instance 的程序 |

热容量以 `byre_pool_warm{backend=...}` 对 `byre_pool_target{backend=...}` 报告。永不
达到目标的 pool 意味着 `Create` 在失败（镜像拉取、配额、缺模板）——查
gateway 日志里的 `pool warm failed` 和 `byre_pool_fill_errors_total`。

## 命令行参数

| 参数 | 默认值 | 含义 |
|---|---|---|
| `-config` | `config.yaml` | 配置文件 |
| `-listen` | 文件里写的值 | 为一次运行覆盖 `listen`：`-listen 127.0.0.1:2223` |
| `-check-config` | 关闭 | 校验，打印每一项违规，有任何违规时退出 2 |
| `-shutdown-delay` | `2s` | SIGTERM 后继续接受这么久再丢弃会话，好让 Service 先停止往这里路由。它住在二进制里，不是 `preStop` hook：预构建镜像是 `FROM scratch`，没有 shell 可以 `sleep` |
| `-allow-ephemeral-hostkey` | 关闭 | 没有 `host_key` 也能运行（仅测试：指纹每次重启都变） |
| `-version` | 关闭 | 打印烙进二进制的版本 |

```sh
byre -config /etc/byre/config.yaml -listen 127.0.0.1:2223   # 第二个端口，同一份配置，无需改动
```

## 日志

每个事件一行，JSON。下面的消息是二进制实际发出的：

```json
{"level":"INFO","msg":"session open","identity":"a1b2c3d4e5f60718","backend":"k8s-claims","instance":"byre-4k2p9"}
{"level":"INFO","msg":"session closed","identity":"a1b2c3d4e5f60718","instance":"byre-4k2p9"}
{"level":"INFO","msg":"lease released","identity":"a1b2c3d4e5f60718","backend":"k8s-claims","instance":"byre-4k2p9","reason":"expired"}
{"level":"WARN","msg":"stale-binding","identity":"a1b2c3d4e5f60718","backend":"k8s-claims","instance":"byre-4k2p9","reason":"instance no longer exists","cause":"...","action":"lease dropped; the next connection provisions a replacement"}
{"level":"WARN","msg":"cannot reach instance","identity":"a1b2c3d4e5f60718","instance":"byre-4k2p9","error":"dial ..."}
{"level":"INFO","msg":"lease replaced after the instance disappeared","identity":"a1b2c3d4e5f60718","backend":"k8s-claims","old_instance":"byre-4k2p9","new_instance":"byre-7xw2m"}
{"level":"INFO","msg":"lease address refreshed","identity":"a1b2c3d4e5f60718","backend":"k8s-claims","instance":"byre-4k2p9","old":"sandbox-2b47c.ns.svc:22","new":"sandbox-vk77n.ns.svc:22"}
{"level":"WARN","msg":"instance discarded at adoption","backend":"k8s-claims","instance":"byre-warm1","reason":"no identity annotation (stale warm capacity)"}
```

没有按会话时长的字段：一次会话的长度是它的 `session open` 与
`session closed` 时间戳之间的距离。`lease released` 携带 `reason`（`expired` 或 `invalidated`），
因为「客户端空闲了」和「instance 坏了」是不同的运维事件，而 `stale-binding` 是
那行说明 lease 在客户端回来之前就被丢弃了——下一条连接会 provision 一个
替代品，而不是拿到一个错误。

identity 以指纹的前 16 个字符出现——够用来关联，不够用来重建密钥。

## 常见症状

| 症状 | 可能原因 | 处理 |
|---|---|---|
| 客户端的文件在命令之间消失 | `lease.ttl` 短于客户端的空闲超时，或客户端打开的连接被 gateway 当成新 identity | 调高 `ttl`；确认客户端到处用同一把密钥 |
| instance 很多，客户端只有一个 | 客户端轮换密钥，或用多把密钥认证 | 每个 identity 优先用一把密钥：identity *就是*指纹，`aliases` 只是日志标签——它们从不把两把密钥路由到一个 lease |
| instance 永不回收 | `grace` 很大，或 channel 泄漏（会话一直开着） | 查 `/metrics`（M3）；通过 backend 手动杀掉该 lease |
| 换密钥后 `unable to authenticate` | 新指纹不在 `identities` 里 | 加上它（追加），重新加载 |
| 启动以很长的错误列表退出 | 配置违规 | 修掉列出的每一项；不要「改一个再重试」 |
| instance 在集群里存在但没人引用它 | 一次 `Acquire` 返回了不可用的 instance，或 adoption 发现重复的 identity annotation | 查 `byre_instances_discarded`；driver 本应已释放它——如果没有，提一个 bug |
| claim（或 pod）是 READY 的，但每次重连都以 `Broken pipe` 死掉，且没有任何 instance 被替换 | claim 背后的对象被删掉、以新名字重建了，于是 lease 缓存的地址不再能解析；`Alive` 只看得到 claim | 升级到带地址刷新的版本（ADR-0015）。用 `kubectl get sandboxclaim <name> -o jsonpath='{.status.sandbox.serviceFQDN}'` 拿到的地址，与网关 `session open` 那行里的地址比一比；正在发生这件事时两者不同，而 `lease address refreshed` 标记了恢复 |

## 安全重启或升级

1. 新进程先 adopt 存活的 lease，其次销毁无 annotation（陈旧 pool）的对象，然后
   补满 pool。客户端连接着时预期 `byre_leases_adopted` 非零，
   `byre_instances_discarded` 应涵盖它无法拥有的每个对象（没有 identity annotation、
   重复的 identity，或无法拨号的描述）。
2. 把 `byre_leases_active + byre_leases_adopted_pending` 与重启前的
   `byre_leases_active` 值比较。被 adopt 的 lease 还没有连接，所以按定义是空闲的；只比
   `active` 永远读到零，分不清健康重启和丢失的表。
3. **没有客户端认领的被 adopt lease 在 `ttl + grace` 之后被销毁**（默认值下 420s），
   与其他空闲 lease 完全一样。这是文档化的契约：在窗口内重连。
4. **只有 `claims` 和 `workloads` 能跨重启存活。** 它们的 instance 以 annotation 携带
   identity，所以新进程能读回来。`endpoints` backend 只在内存里保留归属：
   重启后每个 endpoint 都可被抢，下一条连接按轮询分配，就像
   什么都没发生。没有东西坏掉——但一个曾在某台主机上持有目录树的客户端，现在在另一台上。
5. 在这个窗口内重连的客户端保留它的 instance。lease 已过期的客户端拿到新的一个
   ——这是文档化的契约，不是回归。
6. 观察 `byre_pool_warm{backend=...}` 回到 `byre_pool_target{backend=...}`；停在 0 的 pool
   意味着 `Create` 在失败——gateway 每 15s 记录 `pool warm failed`，计入
   `byre_pool_fill_errors_total`，`/readyz` 保持红色。
7. instance 既绑定 object UID 也绑定名字。如果 lease 背后的对象被删除并
   重建，gateway 记录 `stale-binding`，**不会**把替代品交给旧客户端；
   遗留物作为未绑定容量被回收。
8. **进程退出时每条 SSH 连接都被切断，但不是瞬时的。** 收到 SIGTERM 时进程
   继续接受 `-shutdown-delay`（默认 2s）——即 Service 停止往这里路由的窗口
   ——然后关闭监听器和每个存活会话。没有连接交接：一次 rollout 结束每个存活
   会话，包括 `ssh -L` 隧道。在 `ttl` + `grace` 内重连的客户端落在同一个 instance 上；
   连接客户端一侧的长时间运行命令则不会存活。因此 pod 在数秒内退出，而不是等完
   `terminationGracePeriodSeconds`——那是上限，不是预期时长。在可接受的时间窗口里滚动，
   或者在二进制允许的地方保持进程存活、只换配置。

更多症状及识别每一条的日志行：[troubleshooting.md](troubleshooting.md)。

## 对运行中的 gateway 做冒烟测试

`make` 和脚本是同一件事；如果 CI 有一个 gateway，它调用的就是这个脚本。

```sh
BYRE_CFG=config.yaml BYRE_KEY=path/to/client_key test/e2e/run.sh
make e2e CONFIG=config.yaml KEY=path/to/client_key          # 等价
# 加 --ttl-ok（脚本）以包含 320s 空闲重连检查
# 加 --allow-na 以容忍无法评估的检查（见下）
```

没有 `BYRE_CFG` 和 `BYRE_KEY`（或 `CONFIG`/`KEY`）时，脚本打印 `No target given` 并以 2
退出——这就是单独 `make e2e` 的全部内容。基于 `kind` 的模式在 `docs/roadmap.md` 里有
计划；它需要容器运行时来构建并载入镜像，所以目前还没有东西调用它。

它针对一个已在运行的 gateway 断言：跨重连是同一个 instance、`-L` 能到
instance 自己 sshd 的 22 端口、以及 adoption 和 pool 计数器。因为这个 `:22`，endpoint
监听其他端口的实验环境会挂在隧道检查上——那是 harness，不是 byre。它与
`docs/acceptance/` 里记录的是同一串步骤。

无法评估的检查报告为 `N/A` 并让运行非零退出：AC5（adoption）只能
在一次 adopt 了存活 instance 的重启之后观察到，所以对一个没重启过的 gateway
跑脚本不能作为 AC5 的证据。`--allow-na` 是为那些明确不声称这些检查的运行准备的
——一次绿的 `--allow-na` 运行不是它们的证据。

## 升级

1. 用新二进制对当前文件跑 `byre -check-config`。
2. 滚动 Deployment；管理器在启动时 adopt 存活的 instance（ADR-0002 的后果）。
3. 观察 `byre_leases_active + byre_leases_adopted_pending` 回到此前
   `byre_leases_active` 的值；永久下降意味着 adoption 失败，instance 正在仍持有状态的
   客户端下面被重建。

## 卸载

删除 Deployment 和 Service，然后删除带 `byre/managed=true` 的 instance。静态主机
按设计不受影响：gateway 从不销毁不是它创建的东西。
