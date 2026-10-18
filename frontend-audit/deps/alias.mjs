// 补丁自带依赖（deps/node_modules）的解析别名；vue 等仍从 frontend/ 解析。
import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const modules = resolve(dirname(fileURLToPath(import.meta.url)), 'node_modules')

/** 补丁自带依赖所在目录（用于单独分包） */
export const auditDepsDir = modules

/** @returns {{ find: RegExp, replacement: string }[]} */
export function auditDepsAlias() {
  // monaco-editor/<path> → 包内 esm/vs/<path>（与其 package.json exports 映射一致）
  return [{ find: /^monaco-editor\/(.*)$/, replacement: `${modules}/monaco-editor/esm/vs/$1` }]
}
