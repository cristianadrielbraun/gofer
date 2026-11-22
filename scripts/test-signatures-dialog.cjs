const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// The Signatures dialog saves by itself: the signature being edited a moment
// after typing stops, and an account's choices as soon as one changes.
const source = fs.readFileSync(path.join(__dirname, '../assets/js/settings.js'), 'utf8')
const start = source.indexOf('function setSignatureModeButtonState(')
const end = source.indexOf('window.syncSignatureEditor = syncSignatureEditor')
assert(start > 0 && end > start, 'Missing the signatures dialog code')

// A small element: attributes, classes, children and the simple selectors
// the dialog code uses ("#id", ".class", "[a]", "tag[a]", '[a][b="v"]', comma
// lists).
class El {
  constructor(tag, attrs = {}, children = []) {
    this.tagName = tag.toUpperCase()
    this.attrs = {}
    this.classes = new Set()
    this.children = []
    this.parent = null
    this.listeners = {}
    this.value = ''
    this.disabled = false
    this.innerHTML = ''
    for (const [name, value] of Object.entries(attrs)) {
      if (name === 'class') value.split(/\s+/).filter(Boolean).forEach(c => this.classes.add(c))
      else if (name === 'value') this.value = value
      else if (name === 'disabled') this.disabled = true
      else this.attrs[name] = String(value)
    }
    children.forEach(child => this.append(child))
    const self = this
    this.classList = {
      add: c => self.classes.add(c),
      remove: c => self.classes.delete(c),
      contains: c => self.classes.has(c),
      toggle: (c, on) => { if (on === undefined ? !self.classes.has(c) : on) self.classes.add(c); else self.classes.delete(c) },
    }
    this.dataset = {}
  }
  append(child) { child.parent = this; this.children.push(child) }
  getAttribute(name) { return name in this.attrs ? this.attrs[name] : null }
  setAttribute(name, value) { this.attrs[name] = String(value) }
  hasAttribute(name) { return name in this.attrs }
  removeAttribute(name) { delete this.attrs[name] }
  matches(selector) {
    return selector.split(',').some(part => {
      if (part.trim().startsWith('#')) return this.getAttribute('id') === part.trim().slice(1)
      if (part.trim().startsWith('.')) return this.classes.has(part.trim().slice(1))
      const m = part.trim().match(/^([a-z]*)((?:\[[a-z-]+(?:="[^"]*")?\])+)$/)
      assert(m, `unsupported selector ${part}`)
      if (m[1] && this.tagName !== m[1].toUpperCase()) return false
      return [...m[2].matchAll(/\[([a-z-]+)(?:="([^"]*)")?\]/g)].every(([, name, value]) =>
        this.hasAttribute(name) && (value === undefined || this.getAttribute(name) === value))
    })
  }
  querySelectorAll(selector) {
    const found = []
    const walk = el => el.children.forEach(child => { if (child.matches(selector)) found.push(child); walk(child) })
    walk(this)
    return found
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null }
  closest(selector) {
    for (let el = this; el; el = el.parent) if (el.matches(selector)) return el
    return null
  }
  addEventListener(name, callback) { (this.listeners[name] = this.listeners[name] || []).push(callback) }
  dispatch(name, target = this) {
    const event = { target, preventDefault() {} }
    for (let el = this; el; el = el.parent) (el.listeners[name] || []).forEach(callback => callback(event))
  }
  focus() { focused = this }
}
let focused = null
const el = (tag, attrs, ...children) => new El(tag, attrs, children)

function signatureRow(id, name, html) {
  return el('button', { 'data-signature-row': id, 'data-signature-row-name': name, 'data-signature-row-html': html, 'aria-current': 'false' })
}
function assignment(kind, chosen, withPlace) {
  const choice = el('input', { 'data-signature-assignment-select': '', name: kind + '_signature_id', value: chosen })
  const parts = [choice]
  if (withPlace) parts.push(el('div', { 'data-signature-placement': '', class: chosen ? '' : 'hidden' }, el('input', { name: kind + '_placement', value: 'before' })))
  return el('div', { 'data-signature-assignment': kind }, ...parts)
}
function dialog({ rows = true } = {}) {
  const signatureForm = el('form', { 'data-signature-form': '' },
    el('input', { 'data-signature-id': '', name: 'id' }),
    el('button', { 'data-signature-new': '', 'aria-current': 'false' }),
    el('div', { 'data-signature-list-region': '' }, ...(rows ? [
      signatureRow('sig-lunch', 'Lunch on Friday?', '<p>Alex</p>'),
      signatureRow('sig-work', 'Work', '<p>Alex Example</p>'),
    ] : [])),
    el('input', { 'data-signature-name': '', name: 'name' }),
    el('button', { 'data-signature-mode-button': 'visual' }),
    el('button', { 'data-signature-mode-button': 'source' }),
    el('textarea', { 'data-signature-html': '', name: 'html_body' }),
    el('div', { 'data-signature-editor': '' }),
    el('textarea', { 'data-signature-source': '', class: 'hidden' }),
    el('button', { 'data-signature-delete-current': '', disabled: true }))
  const accountForm = el('form', { 'data-account-signature-settings': '', 'data-account-id': 'acct-alex' },
    assignment('new', 'sig-work', false),
    assignment('reply', 'sig-work', true))
  const accounts = el('div', { 'data-signature-accounts-region': '' },
    el('input', { 'data-signature-account-select': '', value: 'acct-alex' }),
    accountForm)
  const status = el('span', { 'data-signatures-status': '' })
  status.textContent = 'Changes save automatically'
  const manager = el('div', { 'data-account-signatures-manager': '', 'data-refresh-url': '/api/settings/signatures/manage', 'data-refresh-target': '#compose-signatures-manager' }, signatureForm, accounts, el('div', {}, status))
  return { manager, signatureForm, accountForm, status, name: signatureForm.querySelector('[data-signature-name]'), editor: signatureForm.querySelector('[data-signature-editor]') }
}

// Timers run only when the test says so.
let timers = []
// runTimers(600) runs only the pending saves, runTimers(150) the status
// line's slide, runTimers(2000) the return of its note; no argument runs all.
const runTimers = ms => { const due = timers.filter(t => ms === undefined || t.ms === ms); timers = timers.filter(t => !due.includes(t)); due.forEach(t => t.fn()) }
const requests = []
let signatureReply = body => ({ id: body.get('id') || 'sig-new', name: body.get('name'), html_body: body.get('html_body') })
const context = {
  window: {},
  goferT: key => key,
  sanitizeSignatureHTML: html => html,
  confirmSignatureDelete: () => Promise.resolve(false),
  setTimeout: (fn, ms) => { const t = { fn, ms }; timers.push(t); return t },
  clearTimeout: t => { timers = timers.filter(x => x !== t) },
  htmx: { ajax() {} },
  fetch: (url, options = {}) => {
    requests.push({ url, options })
    if (url === '/api/signatures') {
      const reply = signatureReply(new URLSearchParams(options.body))
      return Promise.resolve({ ok: true, json: () => Promise.resolve(reply) })
    }
    if (url.startsWith('/api/settings/signatures/manage')) return Promise.resolve({ ok: true, text: () => Promise.resolve('<html></html>') })
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ status: 'saved' }) })
  },
  DOMParser: class { parseFromString() { return { querySelector: selector => ({ innerHTML: `fresh ${selector}` }) } } },
  FormData: class {
    constructor(form) { this.entries = form.querySelectorAll('[name]').map(input => [input.getAttribute('name'), input.value]) }
    [Symbol.iterator]() { return this.entries[Symbol.iterator]() }
  },
  URLSearchParams,
}
vm.runInNewContext(source.slice(start, end), context)
const settle = async () => { for (let i = 0; i < 8; i++) await new Promise(resolve => setImmediate(resolve)) }
const current = manager => manager.querySelectorAll('[data-signature-row], [data-signature-new]').filter(item => item.getAttribute('aria-current') === 'true')
const posted = () => requests.filter(r => r.options.method === 'POST')
const sent = r => new URLSearchParams(r.options.body)

;(async () => {
  // Opening the dialog opens the first signature without taking focus.
  let { manager, signatureForm, accountForm, status, name, editor } = dialog()
  context.setupAccountSignaturesManager(manager)
  assert.equal(signatureForm.querySelector('[data-signature-id]').value, 'sig-lunch')
  assert.equal(name.value, 'Lunch on Friday?')
  assert.equal(editor.innerHTML, '<p>Alex</p>')
  assert.deepEqual(current(manager).map(item => item.getAttribute('data-signature-row')), ['sig-lunch'])
  assert.equal(manager.querySelector('[data-signature-delete-current]').disabled, false, 'A saved signature can be deleted')
  assert.equal(focused, null, 'Opening the dialog takes no focus')
  assert.equal(status.textContent, 'Changes save automatically')

  // Typing saves once, a moment after the last keystroke.
  editor.innerHTML = '<p>Alex E.</p>'
  editor.dispatch('input')
  editor.innerHTML = '<p>Alex Ex.</p>'
  editor.dispatch('input')
  assert.equal(posted().length, 0, 'Nothing is sent while typing')
  assert.deepEqual(timers.map(t => t.ms), [600], 'One pending save, restarted by each keystroke')
  runTimers(600); await settle()
  assert.equal(posted().length, 1, 'Two keystrokes, one save')
  assert.equal(sent(posted()[0]).get('id'), 'sig-lunch')
  assert.equal(sent(posted()[0]).get('html_body'), '<p>Alex Ex.</p>')
  assert.equal(status.getAttribute('data-status-swap'), 'out', 'The note slides out first')
  assert.equal(status.textContent, 'Changes save automatically')
  runTimers(150)
  assert.equal(status.textContent, 'js.settings.status.saved')
  assert.equal(status.getAttribute('data-status-tone'), 'success', '"Saved" is in the success color')
  assert.equal(status.hasAttribute('data-status-swap'), false, 'and slides in')
  assert.equal(requests.some(r => r.url.startsWith('/api/settings/signatures/manage')), false, 'Same name: no refresh')
  assert.equal(manager.querySelector('[data-signature-row]').getAttribute('data-signature-row-html'), '<p>Alex Ex.</p>', 'The row keeps the saved text for when it is opened again')
  runTimers(2000)
  assert.equal(status.getAttribute('data-status-swap'), 'out', '"Saved" slides out after a moment')
  runTimers(150)
  assert.equal(status.textContent, 'Changes save automatically', 'The note comes back after "Saved"')
  assert.equal(status.hasAttribute('data-status-tone'), false, 'in its own color')

  // A rename refreshes the list and the accounts' choices, on the same account.
  name.value = 'Lunch on Saturday?'
  name.dispatch('input')
  runTimers(600); await settle()
  const refresh = requests.find(r => r.url.startsWith('/api/settings/signatures/manage'))
  assert.equal(refresh && refresh.url, '/api/settings/signatures/manage?account=acct-alex')
  assert.equal(manager.querySelector('[data-signature-list-region]').innerHTML, 'fresh [data-signature-list-region]')
  assert.equal(manager.querySelector('[data-signature-accounts-region]').innerHTML, 'fresh [data-signature-accounts-region]')

  // Moving to another signature saves the one being left right away.
  ;({ manager, signatureForm, accountForm, status, name, editor } = dialog())
  requests.length = 0
  context.setupAccountSignaturesManager(manager)
  name.value = 'Lunch plans'
  name.dispatch('input')
  manager.querySelectorAll('[data-signature-row]')[1].dispatch('click')
  await settle()
  assert.equal(posted().length, 1)
  assert.equal(sent(posted()[0]).get('id'), 'sig-lunch', 'The left signature is saved, not the opened one')
  assert.equal(sent(posted()[0]).get('name'), 'Lunch plans')
  assert.equal(name.value, 'Work')
  assert.equal(timers.filter(t => t.ms === 600).length, 0, 'No save left pending')
  runTimers()

  // A new signature waits for a name and some text, then is created once;
  // later edits update it.
  requests.length = 0
  manager.querySelector('[data-signature-new]').dispatch('click')
  assert.equal(manager.querySelector('[data-signature-delete-current]').disabled, true, 'Nothing to delete for a new signature')
  assert.equal(focused, name)
  name.value = 'Weekend'
  name.dispatch('input')
  runTimers(600); await settle(); runTimers(150)
  assert.equal(posted().length, 0, 'A name alone is not saved')
  assert.equal(status.textContent, 'js.settings.signature.incomplete')
  assert.equal(status.hasAttribute('data-status-tone'), false, 'Not an error, just what it needs')
  editor.innerHTML = '<p>Off until Monday</p>'
  editor.dispatch('input')
  runTimers(600); await settle()
  assert.equal(posted().length, 1)
  assert.equal(sent(posted()[0]).get('id'), '')
  assert.equal(signatureForm.querySelector('[data-signature-id]').value, 'sig-new', 'The new id is kept for the next save')
  assert.equal(manager.querySelector('[data-signature-delete-current]').disabled, false)
  assert.ok(requests.some(r => r.url.startsWith('/api/settings/signatures/manage')), 'A new signature refreshes the list')
  editor.innerHTML = '<p>Off until Tuesday</p>'
  editor.dispatch('input')
  runTimers(600); await settle()
  assert.equal(sent(posted()[1]).get('id'), 'sig-new', 'Later edits update it instead of adding another')

  // Leaving the editor saves without waiting.
  editor.innerHTML = '<p>Off until Wednesday</p>'
  editor.dispatch('input')
  editor.dispatch('focusout')
  await settle()
  assert.equal(posted().length, 3)
  assert.equal(timers.filter(t => t.ms === 600).length, 0)

  // An account's choice saves at once and the place hides with no signature.
  requests.length = 0
  const replyChoice = accountForm.querySelectorAll('input[data-signature-assignment-select]')[1]
  const replyPlace = accountForm.querySelector('[data-signature-placement]')
  replyChoice.value = ''
  replyChoice.dispatch('change')
  await settle()
  assert.equal(posted().length, 1)
  assert.equal(posted()[0].url, '/api/accounts/acct-alex/signature-settings')
  assert.equal(sent(posted()[0]).get('new_signature_id'), 'sig-work')
  assert.equal(sent(posted()[0]).get('reply_signature_id'), '')
  assert.equal(sent(posted()[0]).has('reply_enabled'), false, 'No separate on/off field is sent')
  assert.equal(replyPlace.classList.contains('hidden'), true, 'The place hides with no signature')
  runTimers(150)
  assert.equal(status.textContent, 'js.settings.status.saved')
  assert.equal(status.getAttribute('data-status-tone'), 'success')
  replyChoice.value = 'sig-lunch'
  replyChoice.dispatch('change')
  await settle()
  assert.equal(replyPlace.classList.contains('hidden'), false, 'Picking a signature shows the place again')

  // With no signatures yet, the dialog opens on a new one.
  const empty = dialog({ rows: false })
  context.setupAccountSignaturesManager(empty.manager)
  assert.deepEqual(current(empty.manager), [empty.manager.querySelector('[data-signature-new]')])

  console.log('Signatures dialog: edits save a moment after typing stops (once per pause), leaving a signature saves it, a new one is created once it has a name and text, renames refresh the list, account choices save on change.')
})().catch(err => { console.error(err); process.exit(1) })
