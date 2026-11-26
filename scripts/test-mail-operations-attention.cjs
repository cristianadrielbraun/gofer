// The notice that background mail work needs attention: the dot and count
// follow what the server says, the toast shows once per new problem and never
// inside Settings, and asking again is held back to once every few seconds.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
function extract(name) {
  const start = source.indexOf('function ' + name + '(')
  assert(start >= 0, name)
  return source.slice(start, source.indexOf('\n}\n', start) + 3)
}
const names = ['mailOperationsAttentionState', 'refreshMailOperationsAttention', 'applyMailOperationsAttention', 'mailOperationsAttentionToastWanted', 'showMailOperationsAttentionToast']

function element(attribute) {
  return { attribute, hidden: true, textContent: '' }
}

function setup({ pathname = '/', stored = '', responses = [] } = {}) {
  const dot = element('data-mail-operations-attention-dot')
  const badge = element('data-mail-operations-attention-count')
  const label = element('data-mail-operations-attention-label')
  const all = [dot, badge, label]
  const calls = { fetched: 0, toasts: [], timers: [] }
  const storage = { value: stored }
  let now = 100000
  const context = vm.createContext({
    Date: { now: () => now },
    setTimeout: (fn, ms) => { calls.timers.push(ms); return 1 },
    document: {
      querySelectorAll: selector => all.filter(el => selector.includes(el.attribute)),
      getElementById: () => null,
    },
    window: {
      location: { pathname },
      localStorage: { getItem: () => storage.value, setItem: (_, v) => { storage.value = v } },
    },
    fetch: () => {
      calls.fetched++
      const body = responses.shift() || { count: 0, fingerprint: '' }
      return Promise.resolve({ ok: true, json: () => Promise.resolve(body) })
    },
    goferT: key => key,
    goferTn: (key, count) => key + ':' + count,
    goferNumber: n => String(n),
    showGoferToast: toast => calls.toasts.push(toast),
  })
  vm.runInContext(names.map(extract).join('\n'), context)
  return { context, calls, dot, badge, label, storage, advance: ms => { now += ms } }
}

const flush = () => new Promise(resolve => setImmediate(resolve))

;(async () => {
  // Something needs attention: dot, count and label show, and the toast
  // shows once.
  {
    const page = setup({ responses: [{ count: 2, fingerprint: '2|a' }, { count: 2, fingerprint: '2|a' }] })
    page.context.refreshMailOperationsAttention(true)
    await flush()
    assert.equal(page.dot.hidden, false)
    assert.equal(page.badge.hidden, false)
    assert.equal(page.badge.textContent, '2')
    assert.equal(page.label.textContent, 'js.operations.attention.title:2')
    assert.equal(page.calls.toasts.length, 1)
    assert.equal(page.calls.toasts[0].actions[0].label, 'js.operations.attention.open')
    assert.equal(page.storage.value, '2|a')

    // Asked again too soon: held back until the wait is over.
    page.context.refreshMailOperationsAttention(false)
    assert.equal(page.calls.fetched, 1)
    assert.equal(page.calls.timers.length, 1)

    // The same problems again: no second toast.
    page.advance(6000)
    page.context.refreshMailOperationsAttention(false)
    await flush()
    assert.equal(page.calls.fetched, 2)
    assert.equal(page.calls.toasts.length, 1, 'The toast shows once for the same problems')
  }

  // Nothing needs attention: everything hidden, no toast.
  {
    const page = setup({ responses: [{ count: 0, fingerprint: '' }] })
    page.dot.hidden = false
    page.context.refreshMailOperationsAttention(true)
    await flush()
    assert.equal(page.dot.hidden, true)
    assert.equal(page.badge.hidden, true)
    assert.equal(page.label.textContent, '')
    assert.equal(page.calls.toasts.length, 0)
  }

  // Already shown before, as this browser remembers.
  {
    const page = setup({ stored: '1|x', responses: [{ count: 1, fingerprint: '1|x' }] })
    page.context.refreshMailOperationsAttention(true)
    await flush()
    assert.equal(page.dot.hidden, false)
    assert.equal(page.calls.toasts.length, 0)
  }

  // Never inside Settings or the other sections; a new problem shows again
  // in Mail.
  const wanted = setup().context.mailOperationsAttentionToastWanted
  const data = { count: 1, fingerprint: '1|b' }
  for (const pathname of ['/settings/operations', '/settings', '/contacts', '/calendar/week', '/admin/operations']) {
    assert.equal(wanted(data, pathname, ''), false, pathname)
  }
  for (const pathname of ['/', '/folder/inbox', '/search']) {
    assert.equal(wanted(data, pathname, '1|a'), true, pathname)
  }
  assert.equal(wanted(data, '/', '1|b'), false)

  console.log('mail operations attention: ok')
})().catch(error => { console.error(error); process.exit(1) })
