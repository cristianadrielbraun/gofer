const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// A change that needs a recently verified session asks for verification when
// it's sent, then goes out once verification succeeds (admin.js).
const source = fs.readFileSync(path.join(__dirname, '../assets/js/admin.js'), 'utf8')
const start = source.indexOf('  // Changes that need a recently verified session carry data-step-up.')
const end = source.indexOf('\n  }, true)', start) + 11
assert(start > 0 && end > start, 'Missing the verification-on-submit block')

const listeners = {}
const calls = []
const elements = {}
const sent = []
class HTMLFormElement {}
HTMLFormElement.prototype.submit = function () { sent.push(this) }
const context = {
  HTMLFormElement,
  document: {
    addEventListener(name, callback) { (listeners[name] = listeners[name] || []).push(callback) },
    getElementById(id) { return elements[id] || null },
    createElement(tag) { return { tag } },
  },
  window: { tui: { dialog: { open(root) { calls.push(['open', root.id]) }, close(root) { calls.push(['close', root.id]) } } } },
}
vm.runInNewContext(source.slice(start, end), context)
const fire = (name, event) => listeners[name][0](event)

function form(id, { stepUp = true, holder = null } = {}) {
  const added = []
  const element = Object.assign(Object.create(HTMLFormElement.prototype), {
    id, added,
    matches(selector) { assert.equal(selector, 'form[data-step-up]'); return stepUp },
    closest(selector) { assert.equal(selector, '[data-tui-dialog]'); return holder },
    appendChild(child) { added.push(child) },
  })
  return element
}
function submit(target, submitter) {
  const event = { target, submitter, prevented: false, preventDefault() { this.prevented = true } }
  fire('submit', event)
  return event
}
function verified() {
  const event = { prevented: false, preventDefault() { this.prevented = true } }
  fire('gofer:verified', event)
  return event.prevented
}

// A fresh session's page has no verification dialog: changes go straight out.
const statusDialog = { id: 'admin-user-status-sample' }
const statusForm = form('status', { holder: statusDialog })
assert.equal(submit(statusForm).prevented, false, 'Without verification on the page, the change is sent')

// A stale session's page has it: the change waits for verification.
const verification = { id: 'admin-security-verification-dialog', contains(node) { return node === verification.panel }, panel: {} }
elements['admin-security-verification-dialog'] = verification
assert.equal(submit(form('plain', { stepUp: false })).prevented, false, 'Forms without data-step-up are left alone')
const button = { name: 'status', value: 'disabled', form: statusForm }
assert.equal(submit(statusForm, button).prevented, true, 'The change waits')
assert.deepEqual(calls, [['close', 'admin-user-status-sample'], ['open', 'admin-security-verification-dialog']],
  'Its modal dialog closes so verification can be used')

assert.equal(verified(), true, 'Verification hands over to the waiting change')
assert.deepEqual(sent, [statusForm], 'The change is sent once verified')
assert.deepEqual(statusForm.added, [{ tag: 'input', type: 'hidden', name: 'status', value: 'disabled' }], 'The pressed button still counts')
assert.equal(verified(), false, 'Nothing is sent twice')

// Closing verification without verifying returns to the change's dialog.
calls.length = 0
const deleteDialog = { id: 'admin-user-delete-sample' }
const deleteForm = form('delete', { holder: deleteDialog })
submit(deleteForm)
fire('close', { target: { id: 'another-dialog' } })
fire('close', { target: verification.panel })
assert.deepEqual(calls, [['close', 'admin-user-delete-sample'], ['open', 'admin-security-verification-dialog'], ['open', 'admin-user-delete-sample']],
  'Cancelling verification reopens the change')
assert.equal(verified(), false, 'A cancelled change stays unsent')
assert.deepEqual(sent, [statusForm])

// A change outside any dialog (the security page's forms) just waits.
calls.length = 0
const pageForm = form('page')
submit(pageForm)
assert.deepEqual(calls, [['open', 'admin-security-verification-dialog']])
assert.equal(verified(), true)
assert.deepEqual(sent, [statusForm, pageForm])

console.log('Admin verification on submit: waits, sends once verified with the pressed button, reopens on cancel, leaves other forms alone.')
