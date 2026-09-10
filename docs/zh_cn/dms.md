# DMS 数据后端（第一阶段候选）

本检出增加 `dms` ObjectStorage 后端。文件名、目录、权限、文件到数据对象的布局仍由 JuiceFS 元数据引擎管理；对象 bytes 只写入 DMS。Redis 在下面的示例中只是文件系统元数据引擎，不保存文件内容。

当前为源码开发预览，不是高可靠存储产品。Node 内存丢失可能导致文件无法读取；Meta 的 WAL 不能恢复丢失的 bytes。仅在可信的开发环境使用，不要存放唯一重要数据。

## 取得配套源码

接入分支位于 [lelezi257/juicefs-dms](https://github.com/lelezi257/juicefs-dms/tree/integration/juicefs-baseline)，保留上游 v1.4.1 的提交 `0b90c7db5a929ae6adc5faad948d108efd2c99f9`。仅对象后端增加DMS适配，不改变文件元数据、Chunk/VFS/FUSE。

在已准备好 Go 1.25或兼容工具链、C编译器和FUSE的Linux环境执行：

```bash
git clone --branch integration/juicefs-baseline https://github.com/lelezi257/juicefs-dms.git
cd juicefs-dms
go build -mod=readonly -o juicefs .
```

Go会下载 `go.mod` 固定的公开SDK源码版本 `v0.0.0-20260910013452-f4555eac2319`，不需要本地proxy、replace、Rust或protoc。这是对应Git提交的Go伪版本，不是正式v0.1.0 Release。配套DMS代码基线为 `f4555eac23190ceef555b284366e4623c48fb72b`，服务端构建/启动见 [DMS安装指南](https://github.com/lelezi257/dms/blob/f4555eac23190ceef555b284366e4623c48fb72b/docs/installation.md)。准确复现应固定接入仓提交，不把未来分支更新当作同一版。

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
./juicefs format --storage dms --bucket dms://lab --block-size 4096 "$META_URL" dms-demo
export MOUNT=/tmp/dms-demo
export CACHE=/tmp/dms-demo-cache
mkdir -p "$MOUNT" "$CACHE"
./juicefs --no-agent mount --no-usage-report \
  --backup-meta 0 --cache-size 0 --cache-dir "$CACHE" \
  --attr-cache 0 --entry-cache 0 "$META_URL" "$MOUNT"
```

本阶段显式关闭文件系统元数据的自动备份（`--backup-meta 0`），暂不把可能较大的备份对象写入 DMS。正常后台任务仍然开启，**不要加 `--no-bgjob`**。演示关闭 JuiceFS 磁盘数据缓存及目录项、属性缓存，使读写验证经过实际后端；这不会清空 DMS Node 已有的数据或缓存。另一挂载使用独立的 MOUNT、CACHE 目录，不复用上述目录。

另一个 Linux 终端从挂载路径正常读写文件即可。共享同一个 `META_URL` 的另一挂载进程可以通过自己的 DMS Node 读到文件内容。

`fsync` 在这一阶段验证在线提交与可见性，不代表 Node 重启后数据仍然存在。卸载使用 `juicefs umount /tmp/dms-demo`；不要直接删除正在挂载的目录。

## 本阶段参数与限制

| 配置或能力 | 候选行为 |
| --- | --- |
| 数据对象大小 | 单次 Put 上限 8MiB；默认使用 4MiB JuiceFS block，文件可以远大于一个对象 |
| 并发 Put 的临时内存 | `DMS_JUICEFS_MAX_INFLIGHT_PUTS` 默认 4，允许 1～64；限制 Reader 物化并发，不是总 Node 容量 |
| SDK 小对象直传 | 默认 64KiB；较大对象走原有 staging/payload 流程 |
| Get/Head/List/Delete | 由原生 Go SDK 提供；空值与不存在区分；分页失败返回错误，不当作扫描结束 |
| Range Get | 把 ObjectStorage 的末端裁剪转换为 SDK 严格范围；先 Stat 固定版本再读，不混合两个版本 |
| multipart、Copy、归档恢复、存储层切换 | 当前没有专门实现；保留上游 unsupported/能力标记，不承诺所有管理工具可用 |
| TLS/多租户隔离/Node 重启恢复 | 当前候选不承诺，不应公开暴露服务端口 |

正常删除与物理回收有独立安全验收；不能用“读写成功”代替“空间循环可复用”的结论。完整通过范围以本轮验收报告为准。

## 新增代码在哪里

- `pkg/object/dms.go`：ObjectStorage 适配器、别名配置、范围和错误转换。
- `pkg/object/dms_test.go`：适配器 mock 回归，不代替真实双挂载测试。
- `go.mod`：原生 Go SDK 的版本依赖。构建用户不需要生成 protobuf，也不需要 Rust 编译器。

本接入仓只发布源码，未创建正式Release或预编译下载包。历史本地候选proxy仍可用于开发实验，但不是本分支的构建前提；当前依赖以go.mod的远端固定版本为准。
