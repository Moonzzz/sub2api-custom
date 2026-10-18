// Monaco Editor（VS Code 编辑器内核）按需加载入口：只引入 JSON 展示用到的功能。
import * as monaco from 'monaco-editor/editor/editor.api.js'
import 'monaco-editor/language/json/monaco.contribution.js'
import 'monaco-editor/editor/contrib/folding/browser/folding.js'
import 'monaco-editor/editor/contrib/format/browser/formatActions.js'
import 'monaco-editor/editor/contrib/find/browser/findController.js'
import 'monaco-editor/editor/contrib/clipboard/browser/clipboard.js'
import 'monaco-editor/editor/contrib/bracketMatching/browser/bracketMatching.js'
import EditorWorker from 'monaco-editor/editor/editor.worker.js?worker'
import JsonWorker from 'monaco-editor/language/json/json.worker.js?worker'

;(self as unknown as { MonacoEnvironment: monaco.Environment }).MonacoEnvironment = {
  getWorker: (_id, label) => (label === 'json' ? new JsonWorker() : new EditorWorker())
}

export { monaco }
