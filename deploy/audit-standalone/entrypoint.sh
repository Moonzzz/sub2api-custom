#!/bin/sh
# 请求报文归档：原版镜像的包装启动脚本。
#
# 1. 读取镜像内原版程序的版本与源码提交；
# 2. 若缓存中没有对应「提交 + 补丁」的产物，在容器内编译一次
#    （独立进程组，结束后清理残留进程、卸载编译依赖）；
# 3. 用编译产物替换 /app/sub2api，失败时保持原版继续启动；
# 4. 交给原版 /app/docker-entrypoint.sh 完成启动。
set -eu

log() { echo "[audit] $*" >&2; }

BIN=/app/sub2api
ORIG=/app/sub2api.orig
DIR=/opt/audit
STATE=${AUDIT_BUILD_DIR:-/app/audit-build}
ROOT=${AUDIT_ROOT_DIR:-/app/audit}
ACTIVE=${AUDIT_ACTIVE_DIR:-$ROOT/active}
ARCHIVE=${AUDIT_ARCHIVE_DIR:-$ROOT/archive}

start() {
    if [ "$(id -u)" = "0" ]; then
        for d in "$ROOT" "$ACTIVE" "$ARCHIVE"; do
            mkdir -p "$d"
            # 只修正顶层属主，不递归（归档文件再多也不拖慢启动）
            chown sub2api:sub2api "$d" 2>/dev/null || true
        done
    fi
    exec /app/docker-entrypoint.sh "$@"
}

use_orig() {
    if [ -f "$ORIG" ] && ! cmp -s "$ORIG" "$BIN"; then
        cp -p "$ORIG" "$BIN"
    fi
    log "$1；以原版程序启动（无归档功能）"
    shift
    start "$@"
}

if [ "$(id -u)" != "0" ]; then
    log "容器未以 root 启动，无法编译替换；以原版程序启动"
    exec /app/docker-entrypoint.sh "$@"
fi
if [ "${AUDIT_PATCH:-true}" != "true" ]; then
    use_orig "AUDIT_PATCH=${AUDIT_PATCH}" "$@"
fi

# 容器首次启动时保留原版程序（容器重建后镜像层会恢复原版，这里重新保留）
[ -f "$ORIG" ] || cp -p "$BIN" "$ORIG"

info=$("$ORIG" --version 2>&1 || true)
VERSION=$(printf '%s\n' "$info" | sed -n 's/.*Sub2API \([^ ]*\) (commit: \([0-9a-f]*\).*/\1/p' | head -1)
COMMIT=$(printf '%s\n' "$info" | sed -n 's/.*Sub2API \([^ ]*\) (commit: \([0-9a-f]*\).*/\2/p' | head -1)
if [ -z "$VERSION" ] || [ -z "$COMMIT" ]; then
    use_orig "无法从原版程序读取版本/提交" "$@"
fi

if [ ! -f "$DIR/audit.patch" ] || [ ! -f "$DIR/build.sh" ]; then
    use_orig "缺少 $DIR/audit.patch 或 $DIR/build.sh（检查 compose 中的挂载）" "$@"
fi

PATCH_SHA=$(sha256sum "$DIR/audit.patch" "$DIR/build.sh" | sha256sum | cut -c1-16)
KEY="${VERSION}-$(printf '%s' "$COMMIT" | cut -c1-12)-${PATCH_SHA}-$(uname -m)"
OUT="$STATE/bin/sub2api-$KEY"
FAILED="$STATE/failed-$KEY"
mkdir -p "$STATE/bin"

if [ ! -x "$OUT" ]; then
    if [ -f "$FAILED" ] && [ "${AUDIT_REBUILD:-false}" != "true" ]; then
        use_orig "上次编译失败（$FAILED，日志 $STATE/build.log）；设 AUDIT_REBUILD=true 或删除该文件可重试" "$@"
    fi
    log "首次使用 Sub2API $VERSION ($COMMIT)，开始编译归档版本，日志 $STATE/build.log"
    rm -f "$FAILED"
    # 独立会话 / 进程组运行，结束后整组清理，不留任何编译进程
    set +e
    setsid env VERSION="$VERSION" COMMIT="$COMMIT" OUT="$OUT" STATE="$STATE" PATCH="$DIR/audit.patch" \
        sh "$DIR/build.sh" >"$STATE/build.log" 2>&1 &
    pid=$!
    wait "$pid"
    rc=$?
    kill -9 -- "-$pid" 2>/dev/null
    set -e
    if [ "$rc" != "0" ] || [ ! -x "$OUT" ]; then
        tail -n 30 "$STATE/build.log" >&2 || true
        : >"$FAILED"
        if [ "${AUDIT_REQUIRE_PATCH:-false}" = "true" ]; then
            log "编译失败（AUDIT_REQUIRE_PATCH=true，拒绝以原版启动）"
            exit 1
        fi
        use_orig "编译失败" "$@"
    fi
    log "编译完成"
fi

# 只保留当前版本的产物
for f in "$STATE"/bin/sub2api-*; do
    [ "$f" = "$OUT" ] || rm -f "$f"
done
rm -f "$STATE"/failed-*

if ! cmp -s "$OUT" "$BIN"; then
    cp "$OUT" "$BIN.new"
    chown sub2api:sub2api "$BIN.new"
    chmod 0755 "$BIN.new"
    mv -f "$BIN.new" "$BIN"
fi
log "已替换为归档版本：Sub2API ${VERSION}-audit"
start "$@"
