# DMS 数据后端 0.1.0

本检出增加 `dms` ObjectStorage 后端。文件名、目录、权限、文件到数据对象的布局仍由 JuiceFS 元数据引擎管理；对象 bytes 只写入 DMS。Redis 在下面的示例中只是文件系统元数据引擎，不保存文件内容。

当前为源码开发预览，不是高可靠存储产品。Node 内存丢失可能导致文件无法读取；Meta 的 WAL 不能恢复丢失的 bytes。仅在可信的开发环境使用，不要存放唯一重要数据。

## 取得配套源码

接入代码从本 fork 的 [main 分支](https://github.com/lelezi257/juicefs-dms/tree/main) 获取。该分支保留上游 JuiceFS 历史并加入 DMS 对象后端适配；不要把它理解为纯上游 v1.4.1 分支。接入仍只改变对象后端边界，不改变文件元数据、Chunk/VFS/FUSE 的数据布局。

在已准备好 Go 1.25或兼容工具链、C编译器和FUSE的Linux环境执行：

```bash
git clone --branch main https://github.com/lelezi257/juicefs-dms.git
cd juicefs-dms
git rev-parse HEAD
go build -mod=readonly -o juicefs-dms .
```

Go 会下载 `go.mod` 固定的 DMS Go SDK `v0.1.0`，不需要本地 proxy、replace、Rust 或 protoc。配套 DMS 服务端版本同为 `v0.1.0`；构建、启动和限制见 [DMS 0.1.0 发布说明](https://github.com/lelezi257/dms/releases/tag/v0.1.0)。

发布包使用 `juicefs-dms-<version>-<os>-<arch>` 命名，避免和普通 `juicefs` 二进制混淆。DMS 验收与发布文档统一使用 `juicefs-dms` 作为二进制名。

## 生成 0.1.0 运行包

正式包只从干净且带 `dms-v0.1.0` tag 的提交构建，并从公开 Go module 下载 DMS SDK：

```bash
scripts/package_dms.sh \
  --version 0.1.0 \
  --dms-sdk-version v0.1.0 \
  --expected-tag dms-v0.1.0 \
  --output ./artifacts
```

脚本会生成：

- `juicefs-dms-0.1.0-linux-<arch>.tar.gz`
- 对应 `.sha256`
- 包内 `JUICEFS-DMS-PACKAGE-MANIFEST.json`，记录源码提交、DMS Go SDK module、精确 SDK 版本、二进制校验和
- 包内 `GO-MODULES.json` 与 `LICENSE`

这条链路用来证明发布包消费的是公共 SDK tag，不是本地源码路径。需要验证未发布改动时，脚本也允许显式传入 `--dms-go-proxy`，但这类包不能冒充正式 0.1.0。

## 连接配置

卷配置保存 `dms://lab` 这样的稳定集群别名，不保存某一台机器的本地 socket 路径。每个挂载进程用环境变量把同一个别名映射到它能访问的 DMS Node：

```bash
# 在运行 JuiceFS 的 Linux 终端设置；路径必须与该 Node 配置一致。
export DMS_JUICEFS_ENDPOINTS='{"lab":"unix:///run/dms/worker.sock"}'
export DMS_SHARED_MEMORY=true
```

远端或不使用共享内存时：

```bash
export DMS_JUICEFS_ENDPOINTS='{"lab":"http://10.0.0.11:19200"}'
export DMS_SHARED_MEMORY=false
```

不同机器可以使用不同的 Node 地址，但必须连接到同一个 DMS 集群。不要让同一个别名在不同机器上指向互不相干的集群。共享内存需要 Linux、本机 Node 和 FD Broker；仅把远端地址改成 `unix://` 并不能共享内存。

## 创建与挂载

先按 DMS 手册启动 Meta、Node，再准备一个独立的 JuiceFS 元数据库。以下地址仅为示例；不要复用正在使用的元数据库。

```bash
export META_URL=redis://127.0.0.1:6379/1
./juicefs-dms format --storage dms --bucket dms://lab --block-size 4096 "$META_URL" dms-demo
export MOUNT=/tmp/dms-demo
export CACHE=/tmp/dms-demo-cache
mkdir -p "$MOUNT" "$CACHE"
./juicefs-dms --no-agent mount --no-usage-report \
  --backup-meta 0 --cache-size 0 --cache-dir "$CACHE" \
  --attr-cache 0 --entry-cache 0 "$META_URL" "$MOUNT"
```

本阶段显式关闭文件系统元数据的自动备份（`--backup-meta 0`），暂不把可能较大的备份对象写入 DMS。正常后台任务仍然开启，**不要加 `--no-bgjob`**。演示关闭 JuiceFS 磁盘数据缓存及目录项、属性缓存，使读写验证经过实际后端；这不会清空 DMS Node 已有的数据或缓存。另一挂载使用独立的 MOUNT、CACHE 目录，不复用上述目录。

另一个 Linux 终端从挂载路径正常读写文件即可。共享同一个 `META_URL` 的另一挂载进程可以通过自己的 DMS Node 读到文件内容。

`fsync` 在这一阶段验证在线提交与可见性，不代表 Node 重启后数据仍然存在。卸载使用 `juicefs-dms umount /tmp/dms-demo`；不要直接删除正在挂载的目录。

## 单 VM smoke

`scripts/dms_smoke.sh` 是真实 DMS 后端 smoke，不会启动 DMS 服务，也不会 mock 对象存储。它只消费已经构建好的发布件和环境变量，覆盖 format、mount、create、read、overwrite、random-write、delete、list、checksum、umount。

```bash
go build -mod=readonly -o juicefs-dms .

# 单 VM：alias 指向本机 Node。路径需要和 DMS Node 的 worker UDS 配置一致。
export DMS_JUICEFS_ENDPOINTS='{"lab":"unix:///run/dms/worker.sock"}'
export DMS_SHARED_MEMORY=true

JFS_BIN=./juicefs-dms scripts/dms_smoke.sh
```

三 VM 复用同一个脚本：每台机器使用自己的 `DMS_JUICEFS_ENDPOINTS` 指向当前挂载进程应访问的 Node，`META_URL` 指向同一个 JuiceFS 元数据库。需要验证跨节点读时，在 A 机器写入文件，在 B 机器用同一 `META_URL` 挂载后读取并校验 checksum。

## 本阶段参数与限制

| 配置或能力 | 0.1.0 行为 |
| --- | --- |
| 数据对象大小 | 单次 Put 上限 8MiB；默认使用 4MiB JuiceFS block，文件可以远大于一个对象 |
| 并发 Put 的临时内存 | `DMS_JUICEFS_MAX_INFLIGHT_PUTS` 默认 4，允许 1～64；普通已知长度 Reader 直接调用 SetFrom，未知长度输入仍受限暂存，不是总 Node 容量 |
| SDK 小对象直传 | 默认 64KiB；较大对象走原有 staging/payload 流程 |
| Get/Head/List/Delete | 由原生 Go SDK 提供；空值与不存在区分；分页失败返回错误，不当作扫描结束 |
| Range Get | 一次 GetReader，Node 在同一版本上裁剪末端，不再前置 Stat；后续 Read 不重新 Get |
| Reader / 用户 buffer | Get 返回 SDK Body；SHM 直接复制到 Read(buffer) 的目标，TCP 仍有协议分段缓冲 |
| delimiter List | SDK Scan 做目录分组；游标绑定 prefix/delimiter，分页不承诺全局快照 |
| multipart | 当前没有专门实现。普通文件仍按 JuiceFS block 写入 DMS；需要对象存储原生 multipart 管理语义的工具会看到 unsupported |
| Copy | 当前没有 DMS 服务端对象复制能力。文件复制仍可通过 VFS 读后再写完成，但不会像支持 CopyObject 的后端那样在对象服务端内部完成，性能可能较差 |
| 归档恢复、存储层切换 | 当前没有冷热分层和 restore 语义。普通读写不受影响；依赖对象归档/恢复的管理流程会看到 unsupported |
| TLS/多租户隔离/Node 重启恢复 | 0.1.0 不承诺，不应公开暴露服务端口 |

正常删除与物理回收有独立安全验收；不能用“读写成功”代替“空间循环可复用”的结论。完整通过范围以本轮验收报告为准。

## 新增代码在哪里

- `pkg/object/dms.go`：ObjectStorage 适配器、别名配置、范围和错误转换。
- `pkg/object/dms_test.go`：适配器 mock 回归，不代替真实双挂载测试。
- `go.mod`：原生 Go SDK 的版本依赖。构建用户不需要生成 protobuf，也不需要 Rust 编译器。
- `scripts/package_dms.sh`：从干净 tag 构建可复现 Linux 发布包。
- `scripts/dms_smoke.sh`：真实 DMS 后端单 VM smoke；三 VM 通过 endpoint 配置复用。

0.1.0 发布页提供已验证 Linux 架构的运行包。功能验证包含 Rust/Go SDK TCP/SHM、三 Node、真实 FUSE 和冻结的 14 项性能门禁；它仍是易失内存数据后端，不承诺 Node 重启后的 value 恢复。
