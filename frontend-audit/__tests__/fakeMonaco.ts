// jsdom 无法运行 Monaco：用记录调用的替身验证组件与编辑器 API 的交互。
export const calls: { options: Record<string, unknown>[]; actions: string[]; editors: FakeEditor[]; diffs: FakeDiffEditor[] } = {
  options: [],
  actions: [],
  editors: [],
  diffs: []
}

class FakeTextModel {
  constructor(public value: string, public language: string) {}
  getValue() {
    return this.value
  }
  dispose() {}
}

class FakeDiffEditor {
  model?: { original: FakeTextModel; modified: FakeTextModel }
  private listeners: (() => void)[] = []
  constructor(public host: HTMLElement, public opts: Record<string, unknown>) {
    calls.diffs.push(this)
  }
  setModel(m: { original: FakeTextModel; modified: FakeTextModel }) {
    this.model = m
    this.host.textContent = `${m.original.value}\n=====\n${m.modified.value}`
    this.listeners.forEach((l) => l())
  }
  onDidUpdateDiff(l: () => void) {
    this.listeners.push(l)
  }
  getLineChanges() {
    if (!this.model) return null
    const a = this.model.original.value.split('\n')
    const b = this.model.modified.value.split('\n')
    let n = 0
    for (let i = 0; i < Math.max(a.length, b.length); i++) if (a[i] !== b[i]) n++
    return new Array(n).fill({})
  }
  updateOptions(o: Record<string, unknown>) {
    calls.options.push(o)
  }
  dispose() {}
}

class FakeModel {
  constructor(public value: string, private host: HTMLElement) {
    host.textContent = value
  }
  getLineCount() {
    return this.value.split('\n').length
  }
  getValue() {
    return this.value
  }
  setValue(v: string) {
    this.value = v
    this.host.textContent = v
  }
  dispose() {}
}

class FakeEditor {
  model: FakeModel
  readOnly = true
  constructor(host: HTMLElement, public opts: Record<string, unknown>) {
    this.model = new FakeModel(String(opts.value), host)
    calls.editors.push(this)
  }
  getModel() {
    return this.model
  }
  updateOptions(o: Record<string, unknown>) {
    calls.options.push(o)
    if ('readOnly' in o) this.readOnly = Boolean(o.readOnly)
  }
  getAction(id: string) {
    return {
      run: async () => {
        calls.actions.push(id)
        // 模拟 Monaco 内置格式化：只读时不生效
        if (id === 'editor.action.formatDocument' && !this.readOnly) this.model.setValue(JSON.stringify(JSON.parse(this.model.value), null, 2))
      }
    }
  }
  onDidContentSizeChange() {}
  getContentHeight() {
    return 100
  }
  dispose() {}
}

export const monaco = {
  editor: {
    create: (host: HTMLElement, opts: Record<string, unknown>) => new FakeEditor(host, opts),
    createDiffEditor: (host: HTMLElement, opts: Record<string, unknown>) => new FakeDiffEditor(host, opts),
    createModel: (value: string, language: string) => new FakeTextModel(value, language),
    setTheme: () => {},
    setModelLanguage: () => {}
  }
}
