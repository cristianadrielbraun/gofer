const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const slice = (start, end) => source.slice(source.indexOf(start), source.indexOf(end))

const classList = () => {
  const names = new Set()
  return {add: name => names.add(name), remove: name => names.delete(name), contains: name => names.has(name), toggle(name, on) { on ? names.add(name) : names.delete(name) }}
}
const field = (value = '') => ({value, attributes: {}, setAttribute(name, v) { this.attributes[name] = v }, removeAttribute(name) { delete this.attributes[name] }, dispatchEvent() {}})
const route = (value, accountId, identityId, extra = {}) => ({dataset: {tuiSelectboxValue: value, tuiSelectboxSelected: 'false', tuiSelectboxDisabled: 'false', accountId, identityId, fromKind: 'unknown', fromDomains: '', fromNote: 'may refuse', fromWarns: 'true', ...extra}})

const routes = [
  route('account:gmail', 'gmail', '', {tuiSelectboxDisabled: 'true', fromKind: 'listed', fromNote: ''}),
  route('identity:shop', 'gmail', 'shop'),
  route('account:jmap', 'jmap', '', {fromKind: 'domains', fromDomains: 'example.net shop.example.net', fromNote: 'any address', fromWarns: 'false'}),
  route('account:imap', 'imap', ''),
]
const parts = {
  '[data-compose-one-time-active]': field('1'),
  '[data-compose-one-time-send-via]': field(),
  '[data-compose-one-time-name]': field('Robin'),
  '[data-compose-one-time-email]': field(),
  '[data-compose-one-time-note]': {textContent: '', classList: classList(), getBoundingClientRect: () => ({height: 16})},
  '[data-compose-send-via] [data-tui-selectbox-hidden-input]': field(),
}
const row = {dataset: {}, classList: classList(), getBoundingClientRect: () => ({height: 80}), querySelector: s => parts[s], querySelectorAll: s => s === '[data-compose-send-via] .select-item' ? routes : []}
const account = field('imap')
const form = {id: 'compose-form', querySelector: s => s === '[data-compose-one-time-from]' ? row : s === 'input[name="account_id"]' ? account : null}
const display = {textContent: ''}
let signatures = 0

const context = vm.createContext({
  document: {getElementById: id => id === 'compose-from-display' ? display : null},
  Event: class { constructor(type) { this.type = type } },
  getComputedStyle: () => ({marginTop: '0px'}),
  window: {matchMedia: () => ({matches: false})},
  applyDefaultComposeSignature() { signatures++ },
})
vm.runInContext(slice('function _composeRecipientEmail(', 'function _splitComposeRecipients(') + slice('// _composeEaseHeight runs change', 'function activateComposeOneTimeFrom(') + slice('// setComposeOneTimeRoute picks', '// selectComposeOneTimeFrom is') + slice('// _composeSkipsField leaves', '// syncComposeOneTimeFrom shows'), context)
const selected = () => routes.find(r => r.dataset.tuiSelectboxSelected === 'true')

// The account's own way when it takes other addresses.
context.setComposeOneTimeRoute(form, 'imap', '')
assert.equal(selected().dataset.tuiSelectboxValue, 'account:imap')
assert.equal(parts['[data-compose-one-time-send-via]'].value, '')

// Gmail's own way is closed, so its identity's server is chosen, and the
// account stays.
context.setComposeOneTimeRoute(form, 'gmail', '')
assert.equal(selected().dataset.tuiSelectboxValue, 'identity:shop')
assert.equal(parts['[data-compose-one-time-send-via]'].value, 'shop')
assert.equal(account.value, 'gmail')
assert.equal(signatures, 1)

assert.equal(context.composeOneTimeFromError(form), 'Enter the address to send from.')
parts['[data-compose-one-time-email]'].value = 'robin@'
assert.equal(context.composeOneTimeFromError(form), 'Enter a valid address to send from.')
parts['[data-compose-one-time-email]'].value = '*@example.org'
assert.equal(context.composeOneTimeFromError(form), 'Enter a valid address to send from.')
parts['[data-compose-one-time-email]'].value = 'robin+tag@example.org'
assert.equal(context.composeOneTimeFromError(form), '')
context._renderComposeOneTimeDisplay(form)
assert.equal(display.textContent, 'Robin <robin+tag@example.org>')

// A JMAP server takes only its catch-all domains.
context.setComposeOneTimeRoute(form, 'jmap', '')
assert.equal(context.composeOneTimeFromError(form), 'This server sends only from addresses at example.net, shop.example.net.')
parts['[data-compose-one-time-email]'].value = 'Robin@Shop.Example.net'
assert.equal(context.composeOneTimeFromError(form), '')
context._renderComposeOneTimeNote(form, true)
assert.equal(parts['[data-compose-one-time-note]'].textContent, 'any address')

// A server that may refuse it warns; an error replaces the warning.
context.setComposeOneTimeRoute(form, 'imap', '')
context._renderComposeOneTimeNote(form, true)
assert.ok(parts['[data-compose-one-time-note]'].classList.contains('compose-one-time-warning'))
parts['[data-compose-one-time-email]'].value = 'nope'
context._renderComposeOneTimeNote(form, true)
assert.equal(parts['[data-compose-one-time-note]'].textContent, 'Enter a valid address to send from.')
assert.ok(parts['[data-compose-one-time-note]'].classList.contains('text-destructive'))
assert.ok(!parts['[data-compose-one-time-note]'].classList.contains('compose-one-time-warning'))
assert.equal(parts['[data-compose-one-time-email]'].attributes['aria-invalid'], 'true')

assert.equal(context._composeSkipsField(form, {name: 'from_email'}), false)

// Off, nothing stands in the way, and the row's fields, emptied only once
// it has closed, are left out of what the form sends.
parts['[data-compose-one-time-active]'].value = ''
assert.equal(context.composeOneTimeFromError(form), '')
for (const name of ['one_time_from', 'from_name', 'from_email', 'send_via']) assert.equal(context._composeSkipsField(form, {name}), true)
assert.equal(context._composeSkipsField(form, {name: 'to'}), false)

console.log('compose one-time From checks passed')
