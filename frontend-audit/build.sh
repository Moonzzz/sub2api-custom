#!/usr/bin/env bash
# 构建包含归档查看增强的前端产物（不修改 frontend/ 下任何源文件）。
#
#   frontend-audit/build.sh [额外 vite 参数...]
#
# 前置：已在 frontend/ 执行 pnpm install --frozen-lockfile。
# 产物：与原始构建相同，输出到 backend/internal/web/dist（供 -tags embed 嵌入）。
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/.." && pwd)"

# 补丁目录自身不安装依赖：链接到 frontend/node_modules，使配置与组件解析到同一份依赖。
ln -sfn "$repo/frontend/node_modules" "$here/node_modules"
# 补丁自带依赖（CodeMirror）单独安装在 deps/，不修改 frontend/package.json。
(cd "$repo/frontend" && pnpm install --dir "$here/deps" --frozen-lockfile --ignore-workspace)

node "$here/tools/verify-patch.mjs"

cd "$repo/frontend"
pnpm run check:i18n
pnpm exec vue-tsc -b
pnpm exec vue-tsc --noEmit -p "$here/tsconfig.json"
pnpm exec vitest run --config "$here/vitest.config.mts"
pnpm exec vite build --config "$here/vite.config.mts" "$@"

dist="${AUDIT_UI_DIST:-$repo/backend/internal/web/dist}"
for arg in "$@"; do
  case "$arg" in --outDir=*) dist="${arg#--outDir=}" ;; esac
done
grep -q 'name="sub2api-audit-ui"' "$dist/index.html" || { echo "audit-ui marker missing in $dist/index.html" >&2; exit 1; }
echo "audit-ui build OK -> $dist"
