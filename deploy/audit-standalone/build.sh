#!/bin/sh
# 在原版容器内编译归档版本：下载与原版程序同一提交的源码，打补丁，
# 构建前端（构建期补丁）并嵌入后端二进制。由 entrypoint.sh 在独立进程组中调用。
#
# 输入（环境变量）：VERSION COMMIT OUT STATE PATCH
set -eu

log() { echo "[audit-build] $(date '+%H:%M:%S') $*"; }

WORK="$STATE/work"
CACHE="$STATE/cache"
JOBS=${AUDIT_BUILD_JOBS:-1}

cleanup() {
    log "清理编译环境"
    cd /
    rm -rf "$WORK"
    [ "${AUDIT_BUILD_KEEP_CACHE:-false}" = "true" ] || rm -rf "$CACHE"
    rm -rf /usr/local/lib/node_modules/pnpm /usr/local/bin/pnpm /usr/local/bin/pnpx /root/.npm /root/.cache /tmp/node-compile-cache
    apk del --quiet .audit-build >/dev/null 2>&1 || true
}
trap cleanup EXIT

rm -rf "$WORK"
mkdir -p "$WORK/src" "$CACHE"

if [ -n "${APK_MIRROR:-}" ]; then
    sed -i "s#https\?://[^/]*/alpine#${APK_MIRROR%/}#" /etc/apk/repositories
fi
log "安装编译依赖（go / nodejs / git，结束后卸载）"
apk add --no-cache --quiet --virtual .audit-build go nodejs npm git

log "下载源码 $COMMIT"
SRC_URL=${AUDIT_SOURCE_URL:-https://codeload.github.com/Wei-Shaw/sub2api/tar.gz/@COMMIT@}
curl -fsSL --retry 3 "$(printf '%s' "$SRC_URL" | sed "s/@COMMIT@/$COMMIT/g")" -o "$WORK/src.tar.gz"
tar -xzf "$WORK/src.tar.gz" -C "$WORK/src" --strip-components=1
rm -f "$WORK/src.tar.gz"

log "应用归档补丁"
cd "$WORK/src"
git apply --whitespace=nowarn "$PATCH"

log "构建前端（含归档查看入口）"
export NODE_OPTIONS="${AUDIT_BUILD_NODE_OPTIONS:---max-old-space-size=2048}"
export npm_config_store_dir="$CACHE/pnpm-store"
[ -z "${NPM_CONFIG_REGISTRY:-}" ] || export npm_config_registry="$NPM_CONFIG_REGISTRY"
npm install -g --silent pnpm@9
cd "$WORK/src/frontend"
pnpm install --frozen-lockfile --reporter=silent
pnpm install --dir ../frontend-audit/deps --frozen-lockfile --ignore-workspace --reporter=silent
ln -sfn "$WORK/src/frontend/node_modules" "$WORK/src/frontend-audit/node_modules"
node ../frontend-audit/tools/verify-patch.mjs
pnpm exec vite build --config ../frontend-audit/vite.config.mts --logLevel warn
grep -q 'name="sub2api-audit-ui"' ../backend/internal/web/dist/index.html
rm -rf node_modules "$WORK/src/frontend-audit/node_modules"

log "编译后端（并行度 $JOBS）"
cd "$WORK/src/backend"
export GOTOOLCHAIN=auto CGO_ENABLED=0 GOFLAGS="-p=$JOBS"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"
export GOPATH="$CACHE/go" GOMODCACHE="$CACHE/go/pkg/mod" GOCACHE="$CACHE/go-build"
export GOGC="${AUDIT_BUILD_GOGC:-100}"
go build -tags embed -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}-audit -X main.Commit=${COMMIT} -X main.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ) -X main.BuildType=release" \
    -o "$OUT.tmp" ./cmd/server
grep -a -q 'sub2api-audit-ui' "$OUT.tmp"
chmod 0755 "$OUT.tmp"
mv -f "$OUT.tmp" "$OUT"
log "完成：$OUT"
