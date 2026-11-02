const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/navigation.js'), 'utf8')
const documentListeners = [], windowListeners = []
const window = {
  location: new URL('https://gofer.test/folder/inbox?unread=true'),
  addEventListener(...args) { windowListeners.push(args) },
}
const document = {addEventListener(...args) { documentListeners.push(args) }}
const context = vm.createContext({window, document, URL, history: {state: {folder: 'inbox'}}})
vm.runInContext(source, context)
const route = address => JSON.parse(JSON.stringify(window.GoferNavigation.route(address)))

for (const [address, expected] of [
  ['/', {mode: 'mail', address: '/'}],
  ['/folder/account%3Ainbox/mail%2Fone?unread=true', {mode: 'mail', address: '/folder/account%3Ainbox/mail%2Fone?unread=true'}],
  ['/contacts?contact=one', {mode: 'contacts', address: '/contacts?contact=one'}],
  ['/contacts?new=1', {mode: 'contacts', address: '/contacts?new=1'}],
  ['/calendar?view=week&date=2026-11-02', {mode: 'calendar', address: '/calendar?view=week&date=2026-11-02'}],
  ['/settings', {mode: 'settings', tab: 'accounts', address: '/settings/accounts'}],
  ['/settings/accounts?folder_roles=one', {mode: 'settings', tab: 'accounts', address: '/settings/accounts?folder_roles=one'}],
]) assert.deepEqual(route(address), expected)

for (const tab of ['accounts', 'sync', 'rules', 'operations', 'contacts', 'appearance', 'regional', 'compose-display', 'security', 'advanced', 'export-import', 'about']) {
  assert.equal(route('/settings/' + tab).tab, tab)
}
for (const address of ['https://other.test/settings/accounts', '//other.test/contacts', '/settings/not-a-section', '/settings/security/verify', '/admin/account/security', '/api/calendar/events/one', '/mail/search?q=one', '/contacts#one', '/folder/inbox/one/extra']) {
  assert.equal(route(address), null, 'Leave unsupported routes, verification flows, downloads and external navigation to the browser: ' + address)
  assert.equal(window.GoferNavigation.navigate(address), false)
}
assert.equal(window.GoferNavigation.navigate('/settings/accounts'), false, 'Retain normal links if HTMX/controllers are unavailable')
assert.equal(documentListeners.find(([name]) => name === 'click')[2], true, 'Navigation must mount before HTMX handles links')
assert.equal(windowListeners.find(([name]) => name === 'popstate')[2], true, 'History must restore the whole screen before old pane listeners run')
const count = documentListeners.length + windowListeners.length
vm.runInContext(source, context)
assert.equal(documentListeners.length + windowListeners.length, count, 'History restores must not install duplicate listeners')

// The long-lived compose dialog receives current sender choices, but account
// refreshes must never change the sender of an open or saved draft.
const app = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = app.indexOf('function refreshComposeAccountData(')
const end = app.indexOf('function composeModeForForm(', start)
assert(start >= 0 && end > start)
let open = false, content = false, uploads = 0, updates = 0
const account = {value: 'old'}, identity = {value: 'old-identity'}, display = {textContent: ''}
const menu = {replaceChildren() { updates++ }}
const form = {
  dataset: {},
  querySelector(selector) {
    return {'.compose-from-menu': menu, '[name="account_id"]': account, '[name="identity_id"]': identity}[selector] || null
  },
}
const data = {
  dataset: {composeDefaultAccount: 'current', composeDefaultName: 'Current sender', composeDefaultEmail: 'current@example.test'},
  querySelector() { return {cloneNode() { return {} }, firstElementChild: null} },
}
const senderContext = vm.createContext({
  document: {
    getElementById(id) { return {'compose-form': form, 'compose-account-data': {content: {querySelector() { return data }}}, 'compose-from-display': display}[id] },
    querySelector() { return open },
  },
  _composeHasDraftContent() { return content }, _composePendingUploads() { return uploads }, updateComposeSendState() {},
})
vm.runInContext(app.slice(start, end), senderContext)
for (const state of ['open', 'draft', 'upload', 'sending']) {
  open = state === 'open'; content = state === 'draft'; uploads = state === 'upload' ? 1 : 0
  form.dataset.composeSending = state === 'sending' ? 'true' : 'false'
  senderContext.refreshComposeAccountData()
  assert.equal(account.value, 'old', 'Do not change the sender while ' + state)
  assert.equal(updates, 0)
}
open = content = false; uploads = 0; form.dataset.composeSending = 'false'
senderContext.refreshComposeAccountData()
assert.equal(account.value, 'current', 'A fresh compose receives the current accounts after leaving Settings')
assert.equal(identity.value, '')
assert.equal(display.textContent, 'Current sender <current@example.test>')
assert.equal(updates, 1)
console.log('Client navigation: all screens, query/deep links, unsupported/auth/external routes, fallback links, capture ordering, and idempotent installation passed.')
