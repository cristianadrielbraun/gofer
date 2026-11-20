// Undo and Retry all failed on the Mail operations list: Undo opens the
// confirm dialog for its operation, and confirming posts the undo and reloads
// the list; Retry all posts at once and says how many went.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('function setupMailOperationActions(')
const end = source.indexOf('\n}\n', start) + 3
assert(start >= 0 && end > start)

function element(attributes = {}) {
  return {
    attributes: { ...attributes },
    disabled: false,
    textContent: '',
    children: {},
    getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null },
    setAttribute(name, value) { this.attributes[name] = String(value) },
    querySelector(selector) { return this.children[selector] || null },
    closest(selector) {
      const name = selector.slice(1, -1)
      return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this : null
    },
  }
}

function setup(response) {
  const listeners = []
  const text = element({ 'data-mail-operation-undo-text': '' })
  const confirm = element({ 'data-mail-operation-undo-confirm': '' })
  const dialog = element({ id: 'mail-operation-undo-dialog' })
  dialog.children['[data-mail-operation-undo-text]'] = text
  dialog.children['[data-mail-operation-undo-confirm]'] = confirm
  const calls = { opened: [], closed: [], fetched: [], reloads: [], toasts: [] }
  const context = vm.createContext({
    _mailOperationActionsSetupDone: false,
    document: {
      addEventListener(type, listener) { if (type === 'click') listeners.push(listener) },
      getElementById(id) { return id === 'mail-operation-undo-dialog' ? dialog : null },
    },
    window: {
      tui: { dialog: { open(id) { calls.opened.push(id) }, close(id) { calls.closed.push(id) } } },
      htmx: { ajax(method, url, options) { calls.reloads.push([method, url, options.target]) } },
      location: { reload() { throw new Error('should reload with htmx') } },
    },
    fetch(url, options) {
      calls.fetched.push([url, options.method])
      return Promise.resolve({ ok: response.ok, json: () => Promise.resolve(response.body) })
    },
    goferT: key => key,
    goferTn: (key, count) => key + ':' + count,
    showGoferToast: toast => calls.toasts.push(toast),
  })
  vm.runInContext(source.slice(start, end), context)
  context.setupMailOperationActions()
  const click = target => {
    const event = { target, preventDefault() {}, stopImmediatePropagation() {} }
    listeners.forEach(listener => listener(event))
  }
  return { click, calls, dialog, text, confirm }
}

const flush = () => new Promise(resolve => setImmediate(resolve))

;(async () => {
  // The button fills the dialog for its own operation.
  {
    const page = setup({ ok: true, body: {} })
    const undo = element({
      'data-mail-operation-undo': '',
      'data-mail-operation-id': 'message_mutation:abc',
      'data-mail-operation-undo-text': 'Gofer will stop trying to move this message.',
    })
    page.click(undo)
    assert.deepEqual(page.calls.opened, ['mail-operation-undo-dialog'])
    assert.equal(page.dialog.getAttribute('data-mail-operation-id'), 'message_mutation:abc')
    assert.equal(page.text.textContent, 'Gofer will stop trying to move this message.')
    assert.equal(page.calls.fetched.length, 0, 'Opening the dialog must not undo anything yet')

    page.click(page.confirm)
    assert.equal(page.confirm.disabled, true)
    await flush()
    assert.deepEqual(page.calls.fetched, [['/api/mail-operations/message_mutation%3Aabc/undo', 'POST']])
    assert.deepEqual(page.calls.closed, ['mail-operation-undo-dialog'])
    assert.deepEqual(page.calls.reloads, [['GET', '/settings/operations/content', '#mail-operations-content']])
    assert.equal(page.calls.toasts[0].title, 'js.operations.undone')
  }

  // A refusal keeps the dialog open, says why, and lets the person try again.
  {
    const page = setup({ ok: false, body: { error: "This change can't be undone in Gofer." } })
    page.click(element({ 'data-mail-operation-undo': '', 'data-mail-operation-id': 'label_mutation:7', 'data-mail-operation-undo-text': 'x' }))
    page.click(page.confirm)
    await flush()
    await flush()
    assert.deepEqual(page.calls.closed, [])
    assert.deepEqual(page.calls.reloads, [])
    assert.equal(page.calls.toasts[0].title, 'js.operations.undo_failed_title')
    assert.equal(page.calls.toasts[0].description, "This change can't be undone in Gofer.")
    assert.equal(page.confirm.disabled, false)
  }

  // Retry all reports how many went and how many couldn't.
  {
    const page = setup({ ok: true, body: { retried: 10, skipped: 2 } })
    const retryAll = element({ 'data-mail-operations-retry-all': '' })
    page.click(retryAll)
    assert.equal(retryAll.disabled, true)
    await flush()
    assert.deepEqual(page.calls.fetched, [['/api/mail-operations/retry-failed', 'POST']])
    assert.deepEqual(page.calls.reloads, [['GET', '/settings/operations/content', '#mail-operations-content']])
    assert.equal(page.calls.toasts[0].title, 'js.operations.retry_all.retrying:10')
    assert.equal(page.calls.toasts[0].description, 'js.operations.retry_all.help js.operations.retry_all.skipped:2')
    assert.equal(page.calls.toasts[0].variant, 'warning')
  }
  {
    const page = setup({ ok: true, body: { retried: 0, skipped: 0 } })
    page.click(element({ 'data-mail-operations-retry-all': '' }))
    await flush()
    assert.equal(page.calls.toasts[0].title, 'js.operations.retry_all.none')
    assert.equal(page.calls.toasts[0].variant, 'success')
  }
  {
    const page = setup({ ok: false, body: { error: 'Failed to retry mail operation' } })
    const retryAll = element({ 'data-mail-operations-retry-all': '' })
    page.click(retryAll)
    await flush()
    await flush()
    assert.equal(page.calls.toasts[0].title, 'js.operations.retry_failed_title')
    assert.equal(retryAll.disabled, false, 'A refused Retry all can be pressed again')
  }

  console.log('mail operation actions: ok')
})().catch(error => { console.error(error); process.exit(1) })
