#!/usr/bin/env bash
# 生成 juicefs-dms Linux 运行包。
#
# 关键原则：
# 1. 源码仓 go.mod 保持正常公开依赖，不写 replace，不写本地路径。
# 2. 正式发布从公开 Go module tag 构建；本地预发布验证可显式传入 module proxy。
# 3. 只从干净 HEAD 的 git archive 构建，避免未提交文件混入正式制品。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd -P)"

usage() {
  cat >&2 <<'EOF'
用法:
  scripts/package_dms.sh --version VERSION --dms-sdk-version VERSION [--dms-go-proxy PATH] [--expected-tag TAG] [--output DIR]

示例:
  scripts/package_dms.sh \
    --version 0.1.0 \
    --dms-sdk-version v0.1.0 \
    --expected-tag dms-v0.1.0 \
    --output ./artifacts
EOF
}

VERSION=""
DMS_GO_PROXY=""
DMS_SDK_VERSION=""
EXPECTED_TAG=""
OUTPUT_ROOT="${REPO_DIR}/artifacts"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      VERSION="${2:-}"
      shift 2
      ;;
    --dms-go-proxy)
      DMS_GO_PROXY="${2:-}"
      shift 2
      ;;
    --dms-sdk-version)
      DMS_SDK_VERSION="${2:-}"
      shift 2
      ;;
    --expected-tag)
      EXPECTED_TAG="${2:-}"
      shift 2
      ;;
    --output)
      OUTPUT_ROOT="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "未知参数：$1" >&2
      usage
      exit 2
      ;;
  esac
done

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "juicefs-dms 发布包必须在 Linux 构建" >&2
  exit 2
fi
command -v go >/dev/null 2>&1 || { echo "缺少 go" >&2; exit 2; }
command -v git >/dev/null 2>&1 || { echo "缺少 git" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "缺少 python3" >&2; exit 2; }
command -v sha256sum >/dev/null 2>&1 || { echo "缺少 sha256sum" >&2; exit 2; }

if [[ -z "${VERSION}" || -z "${DMS_SDK_VERSION}" ]]; then
  usage
  exit 2
fi
if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]]; then
  echo "非法产品版本：${VERSION}" >&2
  exit 2
fi
if [[ ! "${DMS_SDK_VERSION}" =~ ^v0\.1\.0(-rc\.[0-9a-f]{16})?$ ]]; then
  echo "DMS SDK 版本必须是 v0.1.0 或 v0.1.0-rc.<16位摘要>：${DMS_SDK_VERSION}" >&2
  exit 2
fi

OUTPUT_ROOT="$(mkdir -p "${OUTPUT_ROOT}" && cd "${OUTPUT_ROOT}" && pwd -P)"
DMS_MODULE="github.com/lelezi257/dms/sdk/go"
GOPROXY_VALUE="https://proxy.golang.org,direct"
DMS_SOURCE_KIND="public-module"
if [[ -n "${DMS_GO_PROXY}" ]]; then
  DMS_GO_PROXY="$(cd "${DMS_GO_PROXY}" && pwd -P)"
  DMS_MODULE_PROXY_DIR="${DMS_GO_PROXY}/${DMS_MODULE}/@v"
  for suffix in zip mod info; do
    if [[ ! -f "${DMS_MODULE_PROXY_DIR}/${DMS_SDK_VERSION}.${suffix}" ]]; then
      echo "Go proxy 中缺少 ${DMS_SDK_VERSION}.${suffix}: ${DMS_MODULE_PROXY_DIR}" >&2
      exit 2
    fi
  done
  if ! grep -qx "${DMS_SDK_VERSION}" "${DMS_MODULE_PROXY_DIR}/list"; then
    echo "Go proxy list 未包含 ${DMS_SDK_VERSION}" >&2
    exit 2
  fi
  GOPROXY_VALUE="file://${DMS_GO_PROXY},https://proxy.golang.org,direct"
  DMS_SOURCE_KIND="local-module-proxy"
fi

OS="$(go env GOOS)"
ARCH="$(go env GOARCH)"
COMMIT="$(git -C "${REPO_DIR}" rev-parse --short=12 HEAD)"
if ! git -C "${REPO_DIR}" diff --quiet \
  || ! git -C "${REPO_DIR}" diff --cached --quiet \
  || [[ -n "$(git -C "${REPO_DIR}" ls-files --others --exclude-standard)" ]]; then
  echo "源码目录不干净，正式构包拒绝继续" >&2
  exit 2
fi
if [[ -n "${EXPECTED_TAG}" ]]; then
  ACTUAL_TAG="$(git -C "${REPO_DIR}" describe --exact-match --tags HEAD 2>/dev/null || true)"
  if [[ "${ACTUAL_TAG}" != "${EXPECTED_TAG}" ]]; then
    echo "HEAD tag 不匹配：期望 ${EXPECTED_TAG}，实际 ${ACTUAL_TAG:-无}" >&2
    exit 2
  fi
fi
BUILD_ID="${JFS_DMS_BUILD_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
if [[ ! "${BUILD_ID}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ || "${BUILD_ID}" == *..* ]]; then
  echo "非法 JFS_DMS_BUILD_ID=${BUILD_ID}" >&2
  exit 2
fi

PACKAGE_NAME="juicefs-dms-${VERSION}-${OS}-${ARCH}"
WORK_DIR="$(mktemp -d /tmp/juicefs-dms-package.XXXXXX)"
cleanup() {
  rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

STAGE="${WORK_DIR}/src"
PACKAGE_DIR="${OUTPUT_ROOT}/${PACKAGE_NAME}-${BUILD_ID}"
ROOT_IN_PACKAGE="${PACKAGE_DIR}/${PACKAGE_NAME}"
ARCHIVE="${PACKAGE_DIR}/${PACKAGE_NAME}.tar.gz"

if [[ -e "${PACKAGE_DIR}" ]]; then
  echo "输出目录已存在，拒绝覆盖：${PACKAGE_DIR}" >&2
  exit 2
fi
mkdir -p "${STAGE}" "${ROOT_IN_PACKAGE}/bin" "${ROOT_IN_PACKAGE}/docs" "${ROOT_IN_PACKAGE}/scripts"

# 只复制 HEAD 中的已提交内容；构包输入和 tag/commit 可以一一对应。
git -C "${REPO_DIR}" archive --format=tar HEAD | tar -C "${STAGE}" -xf -

(
  cd "${STAGE}"
  # 只改 staging 目录中的 go.mod。源码树不出现 replace，也不永久绑定未发布版本。
  go mod edit -require="${DMS_MODULE}@${DMS_SDK_VERSION}"
  if [[ "${DMS_SOURCE_KIND}" == "local-module-proxy" ]]; then
    GOPROXY="${GOPROXY_VALUE}" GONOSUMDB="${DMS_MODULE}" go mod download "${DMS_MODULE}@${DMS_SDK_VERSION}"
  else
    GOPROXY="${GOPROXY_VALUE}" go mod download "${DMS_MODULE}@${DMS_SDK_VERSION}"
  fi
  GOPROXY="${GOPROXY_VALUE}" go build -trimpath \
      -ldflags "-s -w -X github.com/juicedata/juicefs/pkg/version.version=${VERSION} -X github.com/juicedata/juicefs/pkg/version.revision=${COMMIT}" \
      -o "${ROOT_IN_PACKAGE}/bin/juicefs-dms" .
  # go list 的原始 JSON 含 staging 绝对目录。发布清单只保留模块身份和
  # checksum，既能审计依赖，又不会把构建机器路径写入正式制品。
  GOPROXY="${GOPROXY_VALUE}" go list -m -json all | python3 -c '
import json
import sys

decoder = json.JSONDecoder()
text = sys.stdin.read()
offset = 0
modules = []
while offset < len(text):
    while offset < len(text) and text[offset].isspace():
        offset += 1
    if offset >= len(text):
        break
    item, offset = decoder.raw_decode(text, offset)
    record = {
        key: item[key]
        for key in ("Path", "Version", "Sum", "GoModSum", "Main", "Indirect")
        if key in item
    }
    if "Replace" in item:
        replacement = item["Replace"]
        record["Replace"] = {
            key: replacement[key]
            for key in ("Path", "Version", "Sum", "GoModSum")
            if key in replacement
        }
    modules.append(record)
json.dump({"schema_version": 1, "modules": modules}, sys.stdout, indent=2, ensure_ascii=False)
sys.stdout.write("\n")
' >"${ROOT_IN_PACKAGE}/GO-MODULES.json"
)

install -m 0644 "${REPO_DIR}/docs/zh_cn/dms.md" "${ROOT_IN_PACKAGE}/docs/dms.md"
install -m 0755 "${REPO_DIR}/scripts/dms_smoke.sh" "${ROOT_IN_PACKAGE}/scripts/dms_smoke.sh"
install -m 0644 "${REPO_DIR}/LICENSE" "${ROOT_IN_PACKAGE}/LICENSE"

cat >"${ROOT_IN_PACKAGE}/PACKAGE-METADATA" <<EOF
name=${PACKAGE_NAME}
version=${VERSION}
os=${OS}
arch=${ARCH}
source_commit=${COMMIT}
source_dirty=false
dms_go_sdk_module=${DMS_MODULE}
dms_go_sdk_version=${DMS_SDK_VERSION}
dms_go_source=${DMS_SOURCE_KIND}
remote_publish=false
EOF

python3 - "${ROOT_IN_PACKAGE}" "${PACKAGE_NAME}" "${VERSION}" "${OS}" "${ARCH}" "${COMMIT}" "${DMS_MODULE}" "${DMS_SDK_VERSION}" "${DMS_SOURCE_KIND}" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
package_name, version, os_name, arch, commit, module, sdk_version, source_kind = sys.argv[2:10]

def record(path: Path) -> dict:
    data = path.read_bytes()
    return {
        "path": path.relative_to(root).as_posix(),
        "bytes": len(data),
        "sha256": hashlib.sha256(data).hexdigest(),
    }

manifest = {
    "schema_version": 1,
    "package": "juicefs-dms",
    "name": package_name,
    "version": version,
    "os": os_name,
    "arch": arch,
    "source_commit": commit,
    "source_dirty": False,
    "remote_publish": False,
    "dms_go_sdk": {
        "module": module,
        "version": sdk_version,
        "source": source_kind,
    },
    "binaries": [record(root / "bin/juicefs-dms")],
    "docs": ["docs/dms.md", "LICENSE", "GO-MODULES.json"],
    "smoke": ["scripts/dms_smoke.sh"],
}
Path(root / "JUICEFS-DMS-PACKAGE-MANIFEST.json").write_text(
    json.dumps(manifest, indent=2, ensure_ascii=False) + "\n",
    encoding="utf-8",
)
PY

(
  cd "${ROOT_IN_PACKAGE}"
  find . -type f ! -name SHA256SUMS ! -name MANIFEST.txt -printf '%P\n' | LC_ALL=C sort >MANIFEST.txt
  xargs -r sha256sum <MANIFEST.txt >SHA256SUMS
)

tar --sort=name \
  --mtime="@${SOURCE_DATE_EPOCH:-0}" \
  --owner=0 \
  --group=0 \
  --numeric-owner \
  -C "${PACKAGE_DIR}" \
  -cf - "${PACKAGE_NAME}" \
  | gzip -n >"${ARCHIVE}"
(
  cd "${PACKAGE_DIR}"
  sha256sum "$(basename "${ARCHIVE}")" >"$(basename "${ARCHIVE}").sha256"
)

echo "juicefs-dms 发布包已生成：${ARCHIVE}"
echo "归档校验：${ARCHIVE}.sha256"
