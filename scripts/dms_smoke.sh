#!/usr/bin/env bash
# DMS JuiceFS 真实后端 smoke。
#
# 这个脚本只验证 juicefs-dms 发布件能通过 DMS 对象后端完成基础文件操作：
# format / mount / create / read / overwrite / random-write / delete / list /
# checksum / umount。它不启动 DMS 服务，也不伪造对象后端；调用前必须
# 已经按 DMS 部署手册启动 Meta、Node，并通过 DMS_JUICEFS_ENDPOINTS 指向
# 当前机器应该访问的 Node。三 VM 场景复用同一个脚本，只需要每台机器
# 配置自己的 endpoint 和共享的 JuiceFS 元数据库。

set -euo pipefail

log() {
  printf '[dms-smoke] %s\n' "$*"
}

fail() {
  printf '[dms-smoke] ERROR: %s\n' "$*" >&2
  exit 1
}

if [[ "$(uname -s)" != "Linux" ]]; then
  fail "DMS smoke 需要 Linux/FUSE；macOS 只用于编辑和阅读"
fi

command -v python3 >/dev/null 2>&1 || fail "缺少 python3，无法生成随机写和校验文件"
command -v sha256sum >/dev/null 2>&1 || fail "缺少 sha256sum，无法校验内容"

JFS_BIN="${JFS_BIN:-./juicefs-dms}"
[[ -x "$JFS_BIN" ]] || fail "找不到可执行 juicefs-dms，请设置 JFS_BIN 或构建 ./juicefs-dms；脚本不会回退到普通 ./juicefs"

[[ -n "${DMS_JUICEFS_ENDPOINTS:-}" ]] || fail "必须设置 DMS_JUICEFS_ENDPOINTS，例如 '{\"lab\":\"unix:///run/dms/worker.sock\"}'"

DMS_ALIAS="${DMS_ALIAS:-lab}"
VOLUME_NAME="${VOLUME_NAME:-dms-smoke}"
BLOCK_SIZE_KIB="${BLOCK_SIZE_KIB:-4096}"
WORK_ROOT="${DMS_SMOKE_ROOT:-}"
if [[ -z "$WORK_ROOT" ]]; then
  WORK_ROOT="$(mktemp -d /tmp/dms-juicefs-smoke.XXXXXX)"
else
  mkdir -p "$WORK_ROOT"
fi

META_URL="${META_URL:-sqlite3://$WORK_ROOT/meta.db}"
MOUNT_DIR="${MOUNT_DIR:-$WORK_ROOT/mnt}"
CACHE_DIR="${CACHE_DIR:-$WORK_ROOT/cache}"
mkdir -p "$MOUNT_DIR" "$CACHE_DIR"

MOUNT_PID=""
cleanup() {
  set +e
  if mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
    "$JFS_BIN" umount "$MOUNT_DIR" >/dev/null 2>&1 || fusermount -u "$MOUNT_DIR" >/dev/null 2>&1 || umount "$MOUNT_DIR" >/dev/null 2>&1
  fi
  if [[ -n "$MOUNT_PID" ]]; then
    wait "$MOUNT_PID" >/dev/null 2>&1
  fi
  if [[ -z "${DMS_SMOKE_ROOT:-}" && -d "$WORK_ROOT" ]]; then
    rm -rf "$WORK_ROOT"
  else
    log "保留工作目录: $WORK_ROOT"
  fi
}
trap cleanup EXIT

log "binary=$JFS_BIN"
log "meta=$META_URL bucket=dms://$DMS_ALIAS work=$WORK_ROOT"

log "format"
"$JFS_BIN" format --storage dms --bucket "dms://$DMS_ALIAS" --block-size "$BLOCK_SIZE_KIB" --trash-days 0 "$META_URL" "$VOLUME_NAME"

log "mount"
"$JFS_BIN" --no-agent mount --no-usage-report \
  --backup-meta 0 --cache-size 0 --cache-dir "$CACHE_DIR" \
  --attr-cache 0 --entry-cache 0 "$META_URL" "$MOUNT_DIR" \
  >"$WORK_ROOT/mount.log" 2>&1 &
MOUNT_PID="$!"

for _ in $(seq 1 60); do
  if mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
    break
  fi
  if ! kill -0 "$MOUNT_PID" 2>/dev/null; then
    sed -n '1,160p' "$WORK_ROOT/mount.log" >&2 || true
    fail "mount 进程提前退出"
  fi
  sleep 0.5
done
mountpoint -q "$MOUNT_DIR" 2>/dev/null || {
  sed -n '1,160p' "$WORK_ROOT/mount.log" >&2 || true
  fail "等待 mount ready 超时"
}

log "create/read/checksum"
python3 - "$MOUNT_DIR" <<'PY'
import hashlib
import os
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
path = root / "alpha.txt"
with path.open("wb") as f:
    f.write(b"manifest-v1\n")
    f.flush()
    os.fsync(f.fileno())
data = path.read_bytes()
assert data == b"manifest-v1\n", data
print(hashlib.sha256(data).hexdigest())
PY
sha256sum "$MOUNT_DIR/alpha.txt"

log "overwrite"
python3 - "$MOUNT_DIR" <<'PY'
import os
import pathlib
import sys

path = pathlib.Path(sys.argv[1]) / "alpha.txt"
with path.open("wb") as f:
    f.write(b"manifest-v2\n")
    f.flush()
    os.fsync(f.fileno())
assert path.read_bytes() == b"manifest-v2\n"
PY

log "random-write"
python3 - "$MOUNT_DIR" <<'PY'
import hashlib
import os
import pathlib
import sys

path = pathlib.Path(sys.argv[1]) / "random.bin"
data = bytearray((i % 251 for i in range(128 * 1024)))
with path.open("wb") as f:
    f.write(data)
    f.flush()
    os.fsync(f.fileno())
patch = b"DMS-RANDOM-WRITE"
data[4093:4093 + len(patch)] = patch
with path.open("r+b") as f:
    f.seek(4093)
    f.write(patch)
    f.flush()
    os.fsync(f.fileno())
read_back = path.read_bytes()
assert read_back == bytes(data)
print(hashlib.sha256(read_back).hexdigest())
PY
sha256sum "$MOUNT_DIR/random.bin"

log "delete/list"
mkdir -p "$MOUNT_DIR/dir"
printf 'keep\n' >"$MOUNT_DIR/dir/keep.txt"
printf 'gone\n' >"$MOUNT_DIR/dir/gone.txt"
sync "$MOUNT_DIR/dir/keep.txt" "$MOUNT_DIR/dir/gone.txt" 2>/dev/null || sync
rm "$MOUNT_DIR/dir/gone.txt"
ls "$MOUNT_DIR/dir" | tee "$WORK_ROOT/list.txt"
grep -qx 'keep.txt' "$WORK_ROOT/list.txt" || fail "list 未看到 keep.txt"
if grep -qx 'gone.txt' "$WORK_ROOT/list.txt"; then
  fail "delete 后 list 仍看到 gone.txt"
fi

log "umount"
"$JFS_BIN" umount "$MOUNT_DIR"
MOUNT_PID=""

log "PASS"
