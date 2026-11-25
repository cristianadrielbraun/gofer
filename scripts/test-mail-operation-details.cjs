// Details and groups opened in the Mail operations list stay open when the
// list reloads itself, and only those.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/settings.js'), 'utf8')
function extract(name) {
  const start = source.indexOf('function ' + name + '(')
  assert(start >= 0, name)
  return source.slice(start, source.indexOf('\n}\n', start) + 3)
}
const context = vm.createContext({})
vm.runInContext(extract('openMailOperationIDs') + extract('reopenMailOperations'), context)

function node(attributes = {}) {
  const classes = new Set()
  return {
    attributes: { ...attributes },
    classList: { add: name => classes.add(name), has: name => classes.has(name) },
    getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null },
    setAttribute(name, value) { this.attributes[name] = String(value) },
  }
}

// A row's Details, or a group: a collapsible root with a trigger and content.
function details(id, state, attribute = 'data-mail-operation-details') {
  const root = node({ [attribute]: id, 'data-tui-collapsible-state': state })
  root.trigger = node({ 'data-tui-collapsible': 'trigger' })
  root.content = node({ 'data-tui-collapsible': 'content' })
  root.querySelector = selector => selector.includes('trigger') ? root.trigger : root.content
  return root
}

function list(rows) {
  return {
    querySelectorAll(selector) {
      const open = selector.includes('="open"')
      const attribute = selector.match(/^\[([^\]=]+)\]/)[1]
      return rows.filter(row => row.getAttribute(attribute) !== null && (!open || row.getAttribute('data-tui-collapsible-state') === 'open'))
    },
  }
}

const before = list([
  details('message_mutation:a', 'open'),
  details('label_mutation:7', 'closed'),
  details('sent_copy:s1', 'open'),
])
const ids = context.openMailOperationIDs(before, 'data-mail-operation-details')
assert.deepEqual([...ids], ['message_mutation:a', 'sent_copy:s1'])

// After the reload one row is gone and a new one arrived, all closed.
const rows = [details('message_mutation:a', 'closed'), details('label_mutation:7', 'closed'), details('imap_draft:d2', 'closed')]
context.reopenMailOperations(list(rows), 'data-mail-operation-details', ids)
const [kept, closed, added] = rows
assert.equal(kept.getAttribute('data-tui-collapsible-state'), 'open')
assert.equal(kept.trigger.getAttribute('aria-expanded'), 'true')
assert.ok(kept.content.classList.has('tui-collapsible-open'))
for (const row of [closed, added]) {
  assert.equal(row.getAttribute('data-tui-collapsible-state'), 'closed', 'Rows that were closed or new stay closed')
  assert.equal(row.content.classList.has('tui-collapsible-open'), false)
}

// Groups are remembered apart from Details: an open group doesn't open a
// Details that shares nothing with it, and the other way round.
{
  const group = id => details(id, 'open', 'data-mail-operation-group')
  const page = list([group('a1'), details('message_mutation:a', 'open'), details('a2', 'closed', 'data-mail-operation-group')])
  assert.deepEqual([...context.openMailOperationIDs(page, 'data-mail-operation-group')], ['a1'])
  assert.deepEqual([...context.openMailOperationIDs(page, 'data-mail-operation-details')], ['message_mutation:a'])
  const after = [details('a1', 'closed', 'data-mail-operation-group'), details('a2', 'closed', 'data-mail-operation-group')]
  context.reopenMailOperations(list(after), 'data-mail-operation-group', ['a1'])
  assert.equal(after[0].getAttribute('data-tui-collapsible-state'), 'open')
  assert.equal(after[1].getAttribute('data-tui-collapsible-state'), 'closed')
}

console.log('mail operation details: ok')
