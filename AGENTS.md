# AGENTS.md

## DMS 接入分支约束

- 本检出是DMS数据后端接入分支；保持上游模块名、文件元数据、Chunk/VFS/FUSE实现。新增后端集中在pkg/object/dms.go与对应测试，禁止复制SDK协议实现。
- DMS Go SDK是独立依赖；用户接口不得泄漏protobuf/FD。没有私有SDK value缓存，普通读返回owned bytes。
- 当前阶段仅易失内存数据后端，不能宣称fsync具备重启可靠性。支持矩阵见docs/zh_cn/dms.md；未验证能力不标已支持。
- 所有编译、测试和可执行工具在Linux VM/容器；macOS仅编辑阅读。运行使用隔离目录/端口，不替换用户挂载和数据。
- 新接入代码中文注释解释转换、资源与错误边界；遵守Apache许可证头。不手改生成代码。origin为公开接入fork，upstream保持原项目；后续推送/发布仍需对应任务授权，不自动向upstream提交PR。
- `main` 是本fork唯一长期集成分支，DMS接入工作通过短分支/MR收口，合并后删除短分支；不要从upstream同步所有分支或用强推覆盖origin/main历史。

JuiceFS is a POSIX-compatible distributed file system written in Go
(`module github.com/juicedata/juicefs`). A client coordinates a **metadata engine**
and **object storage**, exposing POSIX (FUSE) and an S3 gateway, plus Java/Hadoop
(`sdk/java/`) and Python (`sdk/python/`) SDKs.

Metadata engine families under `pkg/meta/`:

- **Redis** — `redisMeta` (`redis.go`); also KeyDB.
- **SQL/DB** — `dbMeta` (`sql.go`): MySQL, PostgreSQL, SQLite.
- **KV (TKV)** — `kvMeta` (`tkv.go`): TiKV, etcd, BadgerDB, FoundationDB.

## Repository map

Entry points: `main.go` (root) and `cmd/main.go` (CLI commands live in `cmd/`).

| Path           | Responsibility                                                  |
| -------------- | --------------------------------------------------------------- |
| `cmd/`         | CLI subcommands (`mount`, `gateway`, `sync`, `format`, `gc`, …) |
| `pkg/meta/`    | Metadata engine abstraction + per-engine implementations        |
| `pkg/vfs/`     | Virtual filesystem layer (POSIX semantics)                      |
| `pkg/fuse/`    | FUSE bindings (Linux/macOS); `pkg/winfsp/` for Windows          |
| `pkg/fs/`      | High-level filesystem logic                                     |
| `pkg/chunk/`   | Chunk / slice / block data management and caching               |
| `pkg/object/`  | Object storage backend abstraction                              |
| `pkg/gateway/` | S3-compatible gateway                                           |
| `pkg/sync/`    | Data synchronization (`juicefs sync`)                           |
| `pkg/acl/`     | POSIX ACL support                                               |
| `docs/`        | Documentation: `docs/en/` (English), `docs/zh_cn/` (Chinese)    |

## Build

```sh
make juicefs                 # standard build -> ./juicefs
STATIC=1 make juicefs        # static binary (needs musl-gcc)
make BUILD=debug all         # debug build (-N -l)
make juicefs.lite            # minimal build, most backends disabled
make juicefs.ceph            # -tags ceph
make juicefs.fdb             # -tags fdb (FoundationDB)
```

Local volume for manual testing (SQLite metadata):

```sh
./juicefs format sqlite3://test.db myjfs    # create a volume
./juicefs mount  sqlite3://test.db /tmp/jfs # mount it
```

## Test

Use the smallest target covering your change. Targets are in the `Makefile`
and mirror CI (`.github/workflows/unittests.yml`).

```sh
make test.meta.core          # ./pkg/meta/... core (no external services)
make test.meta.non-core      # Redis/PostgreSQL/etcd/KeyDB engine tests
make test.pkg                # all ./pkg/... except meta (-tags gluster)
make test.cmd                # ./cmd/... (needs MinIO env, runs under sudo)
make test.fdb                # FoundationDB tests (-tags fdb)
```

| Change scope       | Run                                                               |
| ------------------ | ----------------------------------------------------------------- |
| `pkg/meta/**`      | `make test.meta.core` (+ `test.meta.non-core` if engine-specific) |
| `cmd/**`           | `make test.cmd`                                                   |
| any other `pkg/**` | `make test.pkg`                                                   |

- When fixing a bug, add a regression test that fails before the fix and passes after.
- Group new test cases for the same module/category together; extend an existing test
  rather than scattering new cases.

## Lint & format

- Run `go fmt` before committing.
- Linting uses `golangci-lint` per `.golangci.yml`; pre-commit pins v1.52.2 and CI runs v2.6 (see `.github/workflows/verify.yml`).
- Install hooks once with `pre-commit install` (config in `.pre-commit-config.yaml`).

## Code style & license header

- Follow [Effective Go](https://go.dev/doc/effective_go) and
  [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments).
- Prefer self-explanatory code over redundant comments; keep necessary comments concise.
- Every new `.go` file MUST start with the Apache 2.0 header (see `main.go` for the canonical template).

## Version compatibility

- Persistent metadata or serialization changes in `pkg/meta/{interface,redis,sql,tkv}.go`
  must remain readable by new clients, and old clients must not silently drop new
  fields when rewriting records.
- When metadata fields change, review `pkg/meta/{dump,backup}.go`, `pkg/meta/*_bak.go`,
  and `pb/backup.proto`. Released dump/load formats must remain readable; tolerate
  unknown fields where feasible, reject unsupported formats explicitly, and never
  silently lose correctness-critical data.
- Evaluate mixed-version behavior for metadata features or semantic changes. If
  unsafe, raise (never lower) `MinClientVersion` and enable the feature only after
  old clients have exited.
- FUSE option changes must preserve existing names and defaults. Review graceful
  restart, `FuseOptions`, `StripOptions`, and old-config normalization in
  `cmd/mount_unix.go`, `pkg/vfs/vfs.go`, and `pkg/fuse/fuse.go`.
- Add compatibility tests, or explicitly report missing coverage during review.

## Agent boundaries

- Correctness first: this is a distributed file system; small changes can affect data
  integrity. Do not invent APIs, defaults, or behavior — verify against the code, and
  don't bypass safety checks.
- Metadata-engine parity: a semantic change in `pkg/meta/` must behave identically
  across all three families (Redis, SQL/DB, KV) and be covered by their shared tests.
- Behavior changes need matching unit tests; user-facing changes update the docs.
- Keep issues and pr comments concise: state the problem clearly and avoid lengthy exposition.
- Keep PRs minimal and focused on the current task; avoid unrelated features,
  refactors, or formatting-only churn.
- Do not hand-edit generated code or vendored dependencies.
- Match existing conventions in the file you are editing.
- Confirm before destructive or hard-to-reverse actions (deleting files, force pushes,
  schema/data changes).
