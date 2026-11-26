const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// A rule's form is drawn again once the "For" list closes after a change, so
// its folder conditions offer the accounts chosen (setupMailRuleForm).
const settings = fs.readFileSync(path.join(__dirname, '../assets/js/settings.js'), 'utf8')
const start = settings.indexOf('function setupMailRuleForm() {')
const end = settings.indexOf('function sanitizeSignatureStyle(', start)
assert(start >= 0 && end > start)

const listeners = {}
const body = {}
const accounts = {
  value: 'all', attributes: {value: 'all'},
  getAttribute(name) { return this.attributes[name] },
  matches: selector => selector === '[data-mail-rule-accounts]',
  closest: selector => selector === '[data-mail-rule-form-body]' ? body : null,
}
const list = {matches: selector => selector === '[data-tui-selectbox-content]'}
const redraws = []
const document = {
  addEventListener(name, listener) { (listeners[name] = listeners[name] || []).push(listener) },
  querySelector: selector => selector === '[data-mail-rule-form-body] [data-mail-rule-accounts]' ? accounts : null,
}
const window = {htmx: {trigger(element, name) { redraws.push([element, name]) }}}
const context = vm.createContext({document, window})
vm.runInContext(settings.slice(start, end) + '\nsetupMailRuleForm()', context)
const fire = (name, event) => { for (const listener of listeners[name] || []) listener(event) }
const pick = value => { accounts.value = value; fire('change', {target: accounts}) }

pick('all,work')
assert.equal(accounts.value, 'work', 'choosing an account clears All accounts')
pick('work,home')
assert.equal(redraws.length, 0, 'the form is not drawn again while the list is open')
fire('toggle', {newState: 'open', target: list})
assert.equal(redraws.length, 0)
fire('toggle', {newState: 'closed', target: list})
assert.deepEqual(redraws, [[body, 'mail-rule-rerender']], 'closing the list after a change draws the form again')
fire('toggle', {newState: 'closed', target: list})
assert.equal(redraws.length, 1, 'closing it again without a change does nothing')
fire('toggle', {newState: 'closed', target: {matches: () => false}})
assert.equal(redraws.length, 1, 'other popovers closing do nothing')
console.log('Rule accounts: the form is drawn again once the For list closes after a change.')
