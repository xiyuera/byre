# Issue tracker: Gitea

这个仓库的 issue、spec、ticket 都放在自建 Gitea（`git.xiyuera.site:8443`）的 Issues 里，用 REST API 操作。

## 凭据

- Token：本机 `~/.hermes/credentials/gitea`（`: "${GITEA_ACCESS_TOKEN:=...}"` 形式，`source` 后使用），
  网关侧 `/opt/data/work/gitea-token`（600）也是同一枚；**不要**把 token 写进仓库、URL 或 `.git/config`。
- 主机：`GITEA_HOST=https://git.xiyuera.site:8443`（自签证书，curl 加 `-k`）。
- 仓库：`byre/byre`，可用 `git remote -v` 从当前克隆反查。

## 约定（curl + REST，没有 gh CLI）

- **建 issue**：`POST /repos/byre/byre/issues`，body `{"title": "...", "body": "...", "labels": [<label id>]}`
- **读**：`GET /repos/byre/byre/issues/<n>`
- **列表**：`GET /repos/byre/byre/issues?state=open&labels=<name1,name2>&limit=50`
- **评论**：`POST /repos/byre/byre/issues/<n>/comments`，body `{"body": "..."}`
- **加/removed label**：`POST /repos/byre/byre/issues/<n>/labels` `{"labels":[<id>,...]}` /
  `DELETE /repos/byre/byre/issues/<n>/labels/<id>`
- **指派**：`PATCH /repos/byre/byre/issues/<n>` `{"assignees":["<username>"]}`
- **关闭**：先评论，再 `PATCH /repos/byre/byre/issues/<n>` `{"state":"closed"}`
- label 名 → id：`GET /repos/byre/byre/labels` 查表，不要按名字猜 id。

## 子 issue 与阻塞（Gitea 没有原生 sub-issue / dependencies）

- **父/子**：子 issue body 首行写 `Part of #<parent>`；父 issue body 维护一个任务列表 `- [ ] #<child>`。
- **阻塞**：子 issue body 首行写 `Blocked by: #<n>, #<n>`；blocker 全部 closed 才算可开工。
- **判断可开工**（wayfinder 的 frontier 查询）：列出父 map 下 open 的子 issue，丢弃 body 里 `Blocked by:`
  指向的 issue 仍 open 的，以及已有 assignee 的；按 map 中的顺序取第一个。

## PR 作为 triage 表面

**PRs as a request surface: no。**（若改成 `yes`，`/triage` 会把外部 PR 一并纳入队列，用
`GET /repos/byre/byre/pulls?state=open` + label 同样的流程。）

## 技能说"publish to the issue tracker"时

在 `byre/byre` 建一个 Gitea issue（或在本地写 `docs/` 草稿后再建）。

## 技能说"fetch the relevant ticket"时

按上面的 **读** 取 issue 正文与评论。
