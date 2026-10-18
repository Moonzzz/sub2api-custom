#!/bin/sh
# 重新生成 audit.patch：只含运行所需的 backend/ 与 frontend-audit/ 改动（不含测试）。
#
#   deploy/audit-standalone/make-patch.sh [上游基线提交，默认 git merge-base HEAD upstream/main]
set -eu
here=$(cd "$(dirname "$0")" && pwd)
cd "$here/../.."
base=${1:-$(git merge-base HEAD upstream/main)}
git diff --binary "$base" HEAD -- backend frontend-audit \
    ':(exclude)*_test.go' ':(exclude)frontend-audit/__tests__' \
    ':(exclude)frontend-audit/vitest.config.mts' ':(exclude)backend/cmd/server/VERSION' \
    >"$here/audit.patch"
echo "audit.patch: base $(git rev-parse --short=12 "$base"), $(wc -l <"$here/audit.patch") lines"
