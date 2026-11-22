// The Mail operations list follows the person's background mail work: it
// reloads when the server says the work changed, once more when a change
// came in during a reload, when a hidden tab is shown again, and after the
// event stream reconnects. It no longer counts down to reloading.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const settings = fs.readFileSync(path.join(__dirname, '../assets/js/settings.js'), 'utf8')
const app = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
function extract(source, name) {
  const start = source.indexOf('function ' + name + '(')
  assert(start >= 0, name)
  return source.slice(start, source.indexOf('\n}\n', start) + 3)
}

function setup() {
  const label = { textContent: '' }
  const content = { id: 'mail-operations-content', isConnected: true, querySelector: () => label }
  const calls = { triggered: 0, attention: 0, dispatched: [] }
  const document = {
    hidden: false,
    getElementById: id => (id === 'mail-operations-content' ? content : null),
    dispatchEvent: event => calls.dispatched.push(event.type),
  }
  const context = vm.createContext({
    document,
    window: { htmx: {} },
    htmx: { trigger: (el, name) => { assert.equal(name, 'mail-operations-refresh'); calls.triggered++ } },
    goferT: key => key,
    CustomEvent: function (type) { this.type = type },
    refreshMailOperationsAttention: () => { calls.attention++ },
  })
  vm.runInContext(['mailOperationsLiveState', 'reloadMailOperationsList', 'mailOperationsListLoaded'].map(n => extract(settings, n)).join('\n') + extract(app, 'mailOperationsChanged'), context)
  // What the htmx request hooks in settings.js do around a reload.
  const begin = () => { context.mailOperationsLiveState().loading = true }
  return { context, calls, document, content, label, begin }
}

// A change reloads the list.
{
  const page = setup()
  page.context.reloadMailOperationsList()
  assert.equal(page.calls.triggered, 1)
}

// Changes during a reload lead to one more reload after it, not one each.
{
  const page = setup()
  page.context.reloadMailOperationsList()
  page.begin()
  page.context.reloadMailOperationsList()
  page.context.reloadMailOperationsList()
  assert.equal(page.calls.triggered, 1)
  page.context.mailOperationsListLoaded(page.content, true)
  assert.equal(page.calls.triggered, 2)
  page.begin()
  page.context.mailOperationsListLoaded(page.content, true)
  assert.equal(page.calls.triggered, 2, 'nothing changed during the second reload')
}

// A hidden tab waits, and reloads once when shown.
{
  const page = setup()
  page.document.hidden = true
  page.context.reloadMailOperationsList()
  page.context.reloadMailOperationsList()
  assert.equal(page.calls.triggered, 0)
  page.document.hidden = false
  assert.equal(page.context.mailOperationsLiveState().again, true)
  page.context.reloadMailOperationsList()
  assert.equal(page.calls.triggered, 1)
}

// A failed reload says so and leaves the list.
{
  const page = setup()
  page.context.reloadMailOperationsList()
  page.begin()
  page.context.mailOperationsListLoaded(page.content, false)
  assert.equal(page.label.textContent, 'js.settings.operations.refresh_failed')
  assert.equal(page.calls.triggered, 1)
}

// Outside the page there is no list to reload.
{
  const page = setup()
  page.document.getElementById = () => null
  page.context.reloadMailOperationsList()
  assert.equal(page.calls.triggered, 0)
}

// The server's event refreshes the attention mark and tells the list.
{
  const page = setup()
  page.context.mailOperationsChanged()
  assert.equal(page.calls.attention, 1)
  assert.deepEqual(page.calls.dispatched, ['gofer:mail-operations-changed'])
}

// The stream wiring: the event, a reconnect counting as a change, and no
// countdown left.
assert.match(app, /source\.addEventListener\("mail-operations-changed", mailOperationsChanged\)/)
assert.match(app, /if \(setupSSE\.reconnecting\) mailOperationsChanged\(\)/)
assert.match(app, /source\.onerror = function \(\) \{\n\s*\/\/ Updates sent while reconnecting are lost\.\n\s*setupSSE\.reconnecting = true/)
assert.match(settings, /addEventListener\("gofer:mail-operations-changed", reloadMailOperationsList\)/)
assert.doesNotMatch(settings, /data-mail-operations-countdown|refresh_in"/)

console.log('mail operations live list: ok')
