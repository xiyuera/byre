---
description: "用 Helm 安装 byre：chart 会带上 ByreLease CRD，可选地还会带上一个 SandboxTemplate 与热 pool。必须自己设置的两个 values 是 pool 密钥与谁可以连接，另外还要选 lease 存储（crd 或 memory）、TTL、grace 和 pool 大小。"
---

# byre Helm chart

gateway，外加（可选的）它从中取 lease 的 sandbox 模板与热 pool。

```sh
helm install byre ./charts/byre -n byre --create-namespace
```

这样就能装好并启动。但它还不会*做*任何事，因为有两个 values 没有安全的默认值：哪些密钥可以申请
lease，以及 gateway 用哪把密钥登录 instance。两者都是刻意的——一个默认成"任何人"或者自带一对密钥的
chart 就是个陷阱。

## 你必须设置的两个 values

```sh
# 1. pool 密钥：gateway 面对 instance 的身份
examples/docker-sandbox/scripts/keys.sh            # 写出 keys/id_key{,.pub}
helm upgrade byre ./charts/byre -n byre \
  --set-file poolKey.privateKey=examples/docker-sandbox/keys/id_key
# 或者更好：自己创建 secret，然后指向它：
#   --set poolKey.existingSecret=byre-pool-key

# 2. 谁可以连接
ssh-keygen -lf ~/.ssh/id_ed25519.pub               # SHA256:...
helm upgrade byre ./charts/byre -n byre \
  --set identities[0].fingerprint=SHA256:... \
  --set identities[0].backend=sandbox
```

然后，从集群内部：

```sh
kubectl -n byre port-forward svc/byre 2222:2222
ssh -p 2222 -i ~/.ssh/id_ed25519 sandbox@127.0.0.1 hostname
```

## lease 表存在哪里

`lease.store` 选择底层存储：

| value | 含义 |
|---|---|
| `crd`（默认） | 每个 identity 一个 `ByreLease` 对象，放在 `lease.namespace` 里。能跨 gateway 重启存活；可以用 `kubectl get byl -n <ns>` 读；两个 gateway 读同一个 namespace 时，对谁持有什么的判断一致。 |
| `memory` | 存在 gateway 进程里。重启会丢掉所有 lease（instance 会保持已分配状态直到 TTL 到期），并且多副本时，同一个 identity 会各自拿到独立的 instance。 |

CRD（`byreleases.byre.io`，Namespaced，短名 `byl`）随本 chart 的
[`crds/`](../assets/files/byreleases.yaml) 一起发布，所以裸的 `helm install` 就会创建它。升级之前有两点
值得知道：

- **`helm upgrade` 从不更新 CRD。** Helm 只在第一次安装时安装 `crds/`，之后就不管了，这是设计如此，
  因为 CRD 变更可能毁掉数据。如果新版本的 chart 带了更新的 CRD，自己 apply：

  ```sh
  helm pull oci://ghcr.io/xiyuera/byre --untar   # 或者从仓库里取 crds/byreleases.yaml
  kubectl apply -f byre/crds/byreleases.yaml
  ```

- **`helm uninstall` 不会删除 CRD**——所以 `ByreLease` 对象能活过一次重装。如果你确实想删，就明确地
  删掉：`kubectl delete crd byreleases.byre.io`。

如果你不能创建集群级对象，用 `--skip-crds --set lease.store=memory` 安装。这个组合是自洽的
（没有任何东西引用 CRD），CI 正是用它来在没有集群的情况下渲染 chart。

当 `lease.store=crd` 时，chart 还会在 `lease.namespace` 里渲染第二个 Role 和 RoleBinding
（`<fullname>-leases`），授予 `byreleases` 与 `byreleases/status`。没有它，store 的就绪检查无法
list `byreleases`，gateway 就会退回从 instance 的 annotation 里读 identity——看起来健康，其实一个
lease 都没持有。

`lease.namespace` 之所以存在，是因为 lease 表和它描述的那些 sandbox 不必待在一起：一个共享的 lease
namespace 加按团队划分的 sandbox namespace 是合理的布局。留空则回落到 `backend.namespace`，那个再留空
则回落到 release 的 namespace。

## Sandbox 模板与 pool

`sandbox.enabled` 默认是 **false**。打开它会渲染一个 `SandboxTemplate` 和一个 `SandboxWarmPool`，
模板指向 `sandbox.image`——如果那个 image 还不存在，预热好的 pod 会停在 `Pending`，gateway 没有东西可发。
先构建一个：

```sh
docker build -f examples/docker-sandbox/Dockerfile.sandbox -t <reg>/byre-sandbox:latest examples/docker-sandbox
docker push <reg>/byre-sandbox:latest

helm upgrade byre ./charts/byre -n byre \
  --set sandbox.enabled=true \
  --set sandbox.image.repository=<reg>/byre-sandbox \
  --set sandbox.image.tag=latest \
  --set sandbox.warmPool.replicas=2
```

支撑这些 CRD（`extensions.agents.x-k8s.io/v1beta1`）的 controller **不**由本 chart 安装。先装它：

```sh
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/sandbox-with-extensions.yaml
kubectl -n agent-sandbox-system rollout status deploy/agent-sandbox-controller
```

在没有它的情况下打开 `sandbox.enabled=true`，安装时就会以 `no matches for kind
SandboxTemplate` 失败——这是诚实的失败，但它发生在 gateway Deployment 已经创建之后，所以安装
只成功了一半。先 apply controller 的 manifest。[Kubernetes 起步套件](kubernetes-sandbox.md) 有完整走查。

## 改变形态的 values

| value | 默认值 | 作用 |
|---|---|---|
| `backend.provision` | `claims` | `claims` \| `command` \| `workloads` \| `endpoints` |
| `backend.command.{acquire,release,list}` | `[]` | `command` provisioner 用的程序 |
| `backend.endpoints` | `[]` | `endpoints` provisioner 用的静态主机 |
| `backend.ssh.hostKeyPolicy` | `accept-new` | 长期存在的主机用 `strict`，用完就丢的容器用 `insecure` |
| `lease.ttl` / `lease.grace` | `10m` / `30s` | 空闲回收 |
| `lease.poolSize` | `null` | `0` 表示"没有热容量，按需创建" |
| `lease.store` | `crd` | `crd` \| `memory`——见下 |
| `lease.namespace` | `""` | `ByreLease` 对象的 namespace；空 = sandbox namespace = release namespace |
| `replicaCount` | `1` | 大于 1 时同一个 identity 会拿到彼此独立的 sandbox；见 `values.yaml` 里的说明 |
| `rbac.create` | `true` | 预先绑定了 ServiceAccount 时设为 false |
| `sandbox.enabled` | `false` | 渲染模板与热 pool |
| `extraVolumes` / `extraVolumeMounts` | `[]` | 例如给 host key 一个 PVC，让它跨重启存活 |

`values.yaml` 里的每个 value 都有内联文档；模板里没有任何硬编码。

## 为什么 host key 用的是 emptyDir

重启会重新生成它，这会让任何钉住指纹的客户端失效。对 chart 来说这是正确的默认（不引入意外的
storage class），但不是生产环境的正确答案：通过 `extraVolumes` 提供一个 PVC，并在 ConfigMap 里让
`host_key` 指向它。
