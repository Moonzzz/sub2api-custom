// Monaco Editor（VS Code 编辑器内核）按需加载入口：只引入 JSON 展示用到的功能。
import * as monaco from 'monaco-editor/editor/editor.api.js'
import 'monaco-editor/language/json/monaco.contribution.js'
import 'monaco-editor/editor/contrib/folding/browser/folding.js'
import 'monaco-editor/editor/contrib/format/browser/formatActions.js'
import 'monaco-editor/editor/contrib/find/browser/findController.js'
import 'monaco-editor/editor/contrib/clipboard/browser/clipboard.js'
import 'monaco-editor/editor/contrib/bracketMatching/browser/bracketMatching.js'
// JSON 语言服务（jsonMode，首次出现 JSON 时按需加载）会经由 internal/common/workers.js 注册全部编辑器扩展与服务。
// 若它晚于第一个编辑器加载，之后新建的编辑器会因服务注册晚于服务初始化而抛出
// "depends on UNKNOWN service ..." 错误。这里提前同步引入，保证注册先于任何编辑器创建
//（该模块本就会被 jsonMode 引入，提前只改变加载顺序，不增加体积）。
import 'monaco-editor/internal/common/workers.js'
import EditorWorker from 'monaco-editor/editor/editor.worker.js?worker'
import JsonWorker from 'monaco-editor/language/json/json.worker.js?worker'

;(self as unknown as { MonacoEnvironment: monaco.Environment }).MonacoEnvironment = {
  getWorker: (_id, label) => (label === 'json' ? new JsonWorker() : new EditorWorker())
}

export { monaco }
