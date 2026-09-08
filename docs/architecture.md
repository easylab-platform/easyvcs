# EasyVCS 架构设计文档

一个 **Change-Native** 版本控制系统。基于 jj(Jujutsu) 的理念，但去掉 Git 兼容层、操作日志、工作副本跟踪，聚焦于**基于 API 的修改系统 + 数据库存储**。

> 术语注：早期文档使用 change/bookmark；代码现统一为 **revision/branch**。本文已同步。

## 1. 核心命题

**Revision 是唯一的第一等实体。** 一次"逻辑修改"有永久稳定的 ID（`revision_id`）；内容（Snapshot）是可变的、内容寻址的。任何 rebase/squash/amend 都改变内容但**永不改变 revision_id**。

## 2. 命名与概念

| 概念 | 命名 | 说明 | 稳定性 |
|---|---|---|---|
| 一次修改 | `Revision` / `revision_id` | 用户引用的主概念 | **永不变** |
| 内容版本 | `Snapshot` / `sha` | 内容寻址的快照，图节点 | 会变（rebase 后新 sha） |
| 存储容器 | `repository` | 中央 DB 里的一条 `(namespace, name)` 记录 | — |
| 可变引用 | `branch` | 指向 revision | repoint |
| 不可变引用 | `tag` | 指向 revision | 锚点 |
| 工作副本 | 无 | API 手动提交 | — |
| 操作日志 | 无 | 不支持 undo/reflog | — |

## 3. 对象模型

```
Snapshot (= 图节点 = commit)
├─ sha          ← 内容哈希（revision_id + parents + tree + desc 派生）rebase 会变
├─ revision_id  ← 随机稳定标签，挂在此快照上
├─ parents[]    ← DAG 边（指向父 snapshot sha）
├─ tree_id      ← 根树（内容）
├─ description / author / time

Revision (= 逻辑修改 = 稳定对象)
├─ id           ← 随机生成，永不修改
├─ hash         ← 指向当前 snapshot sha
└─ created

Object (内容寻址，不可变)
├─ blob（文件内容）
├─ tree（目录：name → (kind, object sha)）
└─ conflict（合并冲突，一等对象）
```

### 关键规则
1. **revision↔snapshot 1:1**：一个 revision 指向唯一的当前 snapshot。
2. **rebase = 换 snapshot sha，revision_id 不变**：只改 parents，生成新 sha，repoint hash。
3. **squash/合并 = 生成新 sha**：内容经 3-way 合并，吸收进目标 revision（保留目标 revision_id）。
4. **`sha` 含 revision_id**（与 jj 不同，jj 的 commit id 不含 change id）：`sha = H(revision_id + tree_id + parents + description + author)`。fork 时为保持 fork 间 revision 身份唯一、可追溯，重映射 revision_id 并重算 sha。

## 4. 存储抽象（关键架构）

通过 `store.RepoStore` 接口将**语义层**与**存储后端**解耦。同一套 revision/merge 逻辑可跑在不同后端上。当前唯一后端是 **GORM**，支持 SQLite（默认，纯 Go）、Postgres、MySQL/MariaDB——均由 `store.OpenDriver(DriverConfig{Kind,DSN})` 选择（共享 `EASYVCS_DB_DRIVER`/`EASYVCS_DB_DSN`）。

```
┌─ 语义层（revision 包）  与存储无关 ─────────────────────┐
│  revision.Workspace: Commit / Rebase / Merge / Log     │
└────────────────────▲────────────────────────────────┘
                     │ store.RepoStore 接口 (GORM)
         ┌────────────┴───────────────────┐
         │                                  │
    sqlite (default, glebarez/modernc)   postgres / mysql
    纯 Go，无 cgo；同一 schema            dialect 由 GORM 生成
```

### DB Schema（GORM AutoMigrate，单一 schema 三库通用）
```
objects(sha PK, kind, content BLOB)          -- 内容寻址，可去重，存 DB
snapshots(repo_id,sha,revision_id,tree_id,commit_time,meta BLOB)
revisions(repo_id,id,hash,created,fork_from,changed_paths)
refs(repo_id,name,kind,target)               -- branch/tag
remotes / remote_refs / workspaces / push_mirrors
users / tokens / namespace_members / merge_requests / mr_reviews / mr_comments
git_revision_links  -- 与 git commit 的弱追溯映射
```
- SQLite/Postgres/MySQL 同一 schema（GORM AutoMigrate 生成方言 DDL），无 cgo，无 `.git` 对象存储。
- 本地 sqlite 用单 `.db` 文件；server 可换 Postgres/MySQL。

## 5. 后端实现选择（多态 RepoStore）

`store.RepoStore` 接口（store/store.go）：
- `WriteObject/ReadObject/ObjectExists`
- `PutSnapshot/GetSnapshot`
- `PutRevision/GetRevision/UpdateRevisionHash[+CAS]/ListRevisions`
- `PutRef/DeleteRef/GetRef/ListRefs`（branch/tag）
- `BeginTx`/`WriteObjectsBatchTx`/`PutSnapshotTx`/`PutRevisionTx`
- `Close`

实现（单一 GORM，按 Kind 选方言）：
| 实现 | 用途 |
|---|---|
| `GORM(sqlite)` | 本地默认（`glebarez/sqlite`，纯 Go） |
| `GORM(postgres)` | server / 集群（pgx） |
| `GORM(mysql)` | MariaDB（go-sql-driver），`mariadb` 别名 |

## 6. 语义操作

### Commit（产生新 snapshot [，可选新 revision]）
1. 从工作区构建 tree（递归写 blob/tree 对象）。
2. 计算 `sha = H(revision_id + tree_id + parents + desc + author)`。
3. 写 snapshot；若 revision_id 为空则随机生成，否则 repoint 现有 revision 的 hash。

### Rebase（revision_id 稳定，sha 变）
1. 读 revision 的当前 snapshot。
2. **保留 tree_id、desc、author**，仅替换 parents → 新 sha。
3. 写新 snapshot，repoint revision.hash。**revision_id 永不改。**

### Merge（3-way tree merge + 一等冲突）
- 对 base/ours/theirs 三棵树的每个路径递归合并。
- 无冲突 → 合并树；有冲突 → 返回冲突列表（路径 + 参与的三方 sha），不失败。
- 冲突是**一等对象**（jj 理念），可在 tree 里记录，供后续 resolve。

## 7. CLI 命令

```
easyvcs init [DIR]                    创建仓库
easyvcs commit [DIR]                  从工作区快照（新 revision）
easyvcs amend <revision> [DIR]        更新现有 revision
easyvcs log [DIR]                     列 revision + 当前 snapshot
easyvcs show <sha> [DIR]              看 snapshot 元数据
easyvcs rebase <revision> --onto <sha> revision 改父（id 不变）
easyvcs merge --base/--ours/--theirs  3-way 合并
easyvcs rev <expr>                    解析修订（@ / <revision_id>）
```

## 8. 协议 Server（easyvcs-server / server 库）

change-native 智能协议（HTTP/JSON + 二进制 bundle，h1+h2c 双栈）：

- `POST /repo/{ns}/{name}/advertise`  读 ACL（列出 revision/ref）
- `POST /repo/{ns}/{name}/fetch`      读 ACL（增量 bundle）
- `POST /repo/{ns}/{name}/push`       写 ACL + 分支白名单 + 非快进拒绝 + 原子事务

鉴权/审计/加密见 §15。

## 9. 与 jj 的对比（我们简化了什么 / 保留了哪些核心）

**保留（与 jj 同等能力）**：revision_id 稳定、内容寻址、3-way 合并 + 一等冲突、tree/blob 对象模型。

**简化（我们的设计取舍）**：
| 丢弃 | jj 对应 | 收益 |
|---|---|---|
| Git 兼容层 | git_backend/git 命令 (34k行) | 砍掉 |
| 操作日志/undo | op_store/operation (~2k行) | 砍掉（无 oplog） |
| 工作副本 mtime | local_working_copy (~3.2k行) | 砍掉（无实时跟踪） |
| revset 大语言 | revset.rs (6.5k行) | 精简为 @ / revision_id |
| 模板/富diff/签名 | templates/gpg | 砍掉 |

**代码规模**：核心库约 1.6k 行 Go（相较 jj ~267k Rust）。保留引擎核心（对象模型、树合并、可插拔存储），砍掉网络/oplog/工作副本/Git 兼容/UX 层。

## 10. 命令集（本次补充）

```
init / commit / amend / log / show
diff <shaA> <shaB>            文件级差异（A/M/D）
checkout <sha> [DEST]         把 snapshot 的树落到文件系统
rebase <revision> --onto <sha> revision 改父（revision_id 稳定，sha 变）
squash <revision>             revision 吸收进父 revision（父 revision_id 稳定）
merge --base/--ours/--theirs  3-way 树合并（一等冲突）
branch <name> <revision>      可变引用 -> revision（派生独立 revision）
tag <name> <revision>         不可变引用 -> revision
refs                          列出所有 branch/tag
rev <expr>                    解析修订（@ / <revision_id>）
remote add/remove             远端配置（token 加密存储）
fetch/pull/push <remote>      智能协议（增量、token 鉴权、非快进 409）
git-pull/git-push <url>       git 互操作（见 §13.1）
gc [--dry-run] / verify       维护（见 §13.2）
```

## 11. 协议端点（server 库）

见 §8。easylab 通过 `server.New(cs, sink)` 复用同一协议 handler（`/api/v1` 托管 API 是 easylab 自己的 mux，不在本模块）。

## 12. 与 jj「文件能否存 DB」的相回回应

- jj 目前只实现 `SimpleBackend`（文件）+ `GitBackend`，**没有 SQL 版**，但 `Backend` trait 预留了插槽。
- EasyVCS 只用 `store.RepoStore`（GORM），同时提供 sqlite/postgres/mysql，**天然支持它们**；内容本体（tree/blob）也进 DB。
- 不兼容 Git 的优势：EasyVCS 可直接把 tree/blob 存 DB；与 Git 的互操作通过独立的 `gitbridge`（go-git，smart protocol）完成，`.git` 对象从不作为 easyvcs 存储。

## 13. 构建

```
CGO_ENABLED=0 go build -o easyvcs ./cmd/easyvcs
CGO_ENABLED=0 go build -o easyvcs-server ./cmd/server
```

## 13.1 Git 互操作（gitbridge）

EasyVCS 通过 **go-git** 与真实 git 仓库走 **smart protocol** 互操作（无 cgo、不存 `.git` 对象）：

- `git-push <url> [branch] [--token][--ssh-key][--passphrase][--squash]`：
  把分支 tip 的 revision 序列导成 git commit，`revision: <id>` 写入 commit message
  作弱追溯；`--squash` 折叠为单 commit；仓库的不可变 tag 导出为 `refs/tags/`。
- `git-pull <url> [branch] [...]`：把远端 commit 逐条导入为 revision（读回
  `revision:` 头；未知名则生成新 revision id）。

## 13.2 维护命令

- `gc [--dry-run]`：回收未被任何 snapshot 引用的孤儿对象（内容寻址只增）。
- `verify`：一致性校验（snapshot 树可读、引用对象存在）。

## 15. 安全模型（P1/P2 加固后）

### 鉴权
- Bearer token 每请求 `store.LookupToken` 解析；**token 只存 SHA-256**（创建时返回一次明文）。
- Token level：`read`（只读）/ `write`（默认）；read-level token push 一律 403。

### ACL（两级）
- **仓库级**：`namespace_members.role` ∈ `readonly|member|admin|owner`。写（push）要求非 readonly；私有库读要求成员或开放实例。
- **分支级**：`branch_acl(repo,user,branch)` 白名单，只作用于 push；用户无行 = 沿用仓库角色，有行 = 仅列出的分支可推。
- 开放实例（无任何用户）匿名可读写；一旦创建首个用户即关闭。
- `kind=mirror` 的仓库拒绝一切 push（只读镜像）。

### 审计
- `AuditSink` 接口 + `audit_log` 表（append-only，无 undo）。
- 记录：动作（advertise/fetch/push）、仓库、用户、IP、结果（ok/denied）、详情。
- XFF 仅在设置 `EASYVCS_TRUSTED_PROXY` 时采信（默认只记 socket 地址）。

### Secret 加密
- `EASYVCS_SECRET_KEY`（AES-GCM，`enc:` 前缀标记）；缺省明文兼容。
- 覆盖列：`remotes.token`、`repositories.mirror_token`、`push_mirrors.token`。

### HTTP 加固
- `http.Server` 超时（ReadHeader 10s / Read+Write 10min / Idle 2min）、`MaxHeaderBytes 1MiB`。
- 请求体上限 `http.MaxBytesReader`（默认 512MiB，`EASYVCS_MAX_BODY` 可调，超限 413）；gzip 解压上限 16GiB。
- 5xx 响应收敛为通用文案，原始错误只进服务端日志。

### 原子性
- push 全程单事务（`transfer.ApplyTx`：objects+snapshots+revisions+refs 全进全出）。
- 每 repo 互斥锁串行化"非快进检查 → 应用"，消除 TOCTOU。
- SQLite：WAL + busy_timeout + 单连接池；事务内不做池读（预读后开事务）。

## 14. EasyLab 开发/部署平台（ops）

EasyLab 把「代码托管 + 制品仓库 + 容器构建/启动」收进一个自包含容器（`easylab`），
复用同一中央 SQLite 与鉴权。对外暴露 `/api/v1/ops/*`：

| 端点 | 说明 |
|---|---|
| `GET /api/v1/ops/namespaces` | 已批准 ops 命名空间 + 默认（`EASYVCS_OPS_NAMESPACES`） |
| `POST /api/v1/ops/runs` | 一次性命令（容器内子进程，日志流到 task） |
| `GET /api/v1/ops/tasks[/{id}][/stream]` | SSE 任务进度（有界日志 + 广播） |
| `POST /api/v1/ops/builds` | 用内置 **buildah** 构建镜像（`bud`），推送回内置 `/v2` |
| `/api/v1/ops/services` (+`/scale`,`/{name}`) | 用内置 **podman** 启动/管理服务 |

### 14.1 镜像构建（buildah，内置）
- 构建后端**只有 buildah**（daemonless、rootless、`--layers` 本地缓存 + registry 缓存复用）。
- `buildDir()` 用 `EASYVCS_BUILDAH_TMP`（默认 `<EASYVCS_HOME>/buildtmp`）作可写 ext4 目录；
  `--tls-verify=false`、`STORAGE_DRIVER=vfs`、`BUILDAH_ISOLATION=chroot`。
- 内部 `/v2`（`EASYVCS_REGISTRY`，默认 `127.0.0.1:8080`）供 buildah 拉基础镜像、缓存、推送产物。

### 14.2 服务启动（podman，内置，唯一后端）
- 服务后端**只有内部 podman**（无 docker / k8s 依赖），每服务一个 podman 容器：
  - 每服务独立 bridge 网络；`Group`/`Network` 相同时**共享网络**，服务间用 `<name>` 域名互访（aardvark DNS）。
  - 真实 cgroup 资源限制：`--cpus` / `--memory`（`ServiceRequest.CPUs`/`MemoryBytes`）。
  - 端口映射到 EasyLab loopback（`WorkerURL`），供反代/外部访问。
- 需 pod 满足：`privileged` + `capAdd[SYS_ADMIN,NET_ADMIN,NET_RAW]` + `hostUsers` + `/sys/fs/cgroup` 可写
  （entrypoint 启动时 `mount -o remount,rw /sys/fs/cgroup`，或 hostPath 挂 cgroup）。
- `ensureNetwork` 会清 aardvark-dns 残留配置，避免旧网络网关导致 DNS 服务崩溃。

### 14.3 后端选择
`easylab` 里 `newServiceRunner()` 固定返回 podman 后端；`Builder` 固定 buildah。
数据库仍为单一 `~/.easyvcs/easyvcs.db`（`EASYVCS_HOME` 覆盖）。

### 14.4 构建/运行镜像（easy-lab）
- `easy-lab/Dockerfile`：runtime 基于 `alpine`，apk 源切到阿里云镜像（`mirrors.aliyun.com`，加进 `NO_PROXY`）。
- `easy-lab/build-image.sh`：用**集群 buildkitd**（`buildctl`）构建 EasyLab 镜像 → 导出 docker archive →
  `skopeo` 推送到 forgejo OCI（`forgejo.../root/easylab:v0.1.0`）。
- EasyLab 容器内**只内嵌 buildah + podman**（不做 Docker-in-Docker，无 docker-cli）。

### 14.5 可切换后端（为横扩/集群化铺路）

EasyLab 的存储边界已抽象为可切换，语义层（revision/merge/transfer/artifactkit）零改动：

| 层 | 接口 | 后端开关 |
|---|---|---|
| 元数据 | `CentralStore` | `EASYVCS_DB_DRIVER=sqlite\|postgres`，`EASYVCS_DB_DSN` |
| 对象/blob | artifactkit `BlobStore` | `EASYVCS_BLOB_BACKEND=sqlite\|s3`（s3 为占位） |

- `internal/store/driver.go`：`DriverConfig{Kind,DSN}` + `OpenDriver`；`rebindPostgres` 把 `?`→`$n`；
  `SetWAL`/`migrateRepoColumns` 按 kind 分支。
- `cmd/easylab/main.go` 用 `OpenDriver` 组装；`cmd/easylab/registry.go` 按 `BLOB_BACKEND` 选 blob。
- 从单容器升级到集群只需：`EASYVCS_DB_DRIVER=postgres`（元数据共享）+ `EASYVCS_BLOB_BACKEND=s3`（对象共享）。

### 14.6 Agent 原生：extension 桥接（easyvcs 完全独立于 agent）

easylab **不依赖任何 agent 协议**，只暴露普通 HTTP `/api/v1`。agent 能力由两个独立
extension 进程桥接（各自 `Config.ID`，通过 NATS `discover`/`tool.call` 被 agent 动态发现）：

```
zergx-agent / 任意 agent
   │  NATS: abc.discover / tool.call.{extId}.{tool}   (nats://127.0.0.1:14222, JetStream)
   ├─ easyvcs-code   (11 tools, 读/写代码 → revision 落库)
   └─ easyvcs-ops    (9 tools, sandbox / build / 服务)
   (两个 extension 只调 easylab 的普通 HTTP /api/v1)
easylab (核心, 纯 VCS/ops, 与 agent 无关 · :18160)
nats-server (JetStream · :14222)   ← 内嵌消息/事件总线
```

- **`easy-lab/ext/code/`** → `easyvcs-code`：`list read write edit delete search revision_diff log blame refs history`。
- **`easy-lab/ext/ops/`** → `easyvcs-ops`：`sandbox_run sandbox_read sandbox_write sandbox_services container_build service_deploy service_scale service_status service_delete`。
- 走 `abc-protocol/sdk-go`（`easy-lab/deps/abc-sdk-go`，本地 replace）。协议常量全部 `abc.*`，
  无产品名绑定；agent 不写死应用名。
- easylab 新增 `/api/v1/ops/sandbox/{name}/{exec,file}`（持久 podman 容器内执行/读写，**不落库**）。
- 动态插拔：起停 `easyvcs-code`/`easyvcs-ops` 程序组即可，agent 下次 `discover` 自动感知。
- supervisord 组：`nats`、`easyvcs-code`(:18091)、`easyvcs-ops`(:18092)、`easylab`(:18160)，全部常驻。

未开放（按决策后置）：历史重写（rebase/squash/resolve/merge）、MR、发布(artifactkit package)、browser/helm。


