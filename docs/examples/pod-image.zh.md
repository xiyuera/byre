---
description: "byre 的逃生通道：用 provision: workloads 指定任意会说 SSH 的 image，byre 创建一个裸 pod、等它接受 SSH，lease 结束时删掉。没有 CRD、没有模板、没有 orchestrator 专属接线；image 里只要有 sshd 和 pool 公钥即可。"
---

# 示例：任何会说 SSH 的 image（`provision: workloads`）

这是那条逃生通道：byre 用你指定的 image 创建一个裸 pod，等它接受 SSH，
到 lease 结束时删掉它。没有 CRD，没有模板，没有 orchestrator 专属接线。

## 一个最小 image

```dockerfile
# examples/pod-image/Dockerfile
FROM alpine:3.20
RUN apk add --no-cache openssh-server \
 && ssh-keygen -A \
 && mkdir -p /root/.ssh && chmod 700 /root/.ssh \
 && sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
COPY authorized_keys /root/.ssh/authorized_keys   # gateway 的 pool 公钥
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
```

把它构建、推到你的集群能拉的任意 registry。这个文件里没有一处是 byre 专有的：
任何跑 `sshd`、信任 pool 密钥的 image 都能用。

## Backend

这是一个**片段**：它放在完整配置的 `backends:` 下面，那份配置还需要 `listen`、
`host_key` 和一个 `identities:` 列表（见 [`../local-quickstart/config.yaml`](../assets/files/config.yaml)）。

```yaml
backends:
  - name: adhoc
    provision: workloads
    namespace: "<namespace>"
    provider:
      image: "<registry>/<sshd-image>:<tag>"
      start_command: ["/usr/sbin/sshd", "-D", "-e"]   # 可选；省略时用 image 的 CMD
      ready_timeout: 180s                            # 冷 image 拉取很慢
    ssh:
      user: root
      private_key: /etc/byre/keys/id_key
      host_key_policy: accept-new
```

## 备注

- pod 被标注 `byre/managed=true`，所以重启后的 gateway 能 adopt 或回收它。
- `ready_timeout` 覆盖 image 拉取，不只是启动；一个冷的多几百 MB 的 image 经常要 >120s。
- 不挂任何卷：instance 按设计就是一次性的。需要状态的客户端得自己把状态搬进搬出
  （参考客户端的文件同步干的正是这件事）。
- 有带池的 CRD 时优先用 `claims`：热容量就是第一条连接 300 ms 和 30 s 的区别。
