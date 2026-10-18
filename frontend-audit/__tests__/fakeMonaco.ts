// jsdom 无法运行 Monaco：用记录调用的替身验证组件与编辑器 API 的交互。
export const calls: { options: Record<string, unknown>[]; actions: string[]; editors: FakeEditor[] } = { options: [], actions: [], editors: [] }

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
    setTheme: () => {},
    setModelLanguage: () => {}
  }
}
