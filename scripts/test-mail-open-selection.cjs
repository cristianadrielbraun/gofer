// Opening a message: the list marks it as soon as it starts loading, not
// once the reader arrived.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const slice = (start, end) => {
  const from = source.indexOf(start)
  const to = source.indexOf(end, from)
  assert.ok(from >= 0 && to > from, 'found ' + start)
  return source.slice(from, to)
}

// The list marks the message when its request starts, not when it ends.
{
  const handlers = {}
  const selected = []
  const markedRead = []
  const context = vm.createContext({
    document: {body: {addEventListener: (name, fn) => { (handlers[name] = handlers[name] || []).push(fn) }}},
    virtualMailList: {
      itemsContainer: {},
      onEmailSelected: (id, options) => selected.push([id, options.inThread]),
      syncSelectionClasses() {},
    },
    preserveMailListSelectionFor: null,
    suppressEmailUrlPushFor: null,
    showMailViewLoading() {},
    scheduleAutoMarkRead: id => markedRead.push(id),
  })
  vm.runInContext(slice('function setupEmailSelectionTracking()', 'function setupMailListViewToggle()') + '\nsetupEmailSelectionTracking()', context)
  const event = {detail: {pathInfo: {requestPath: '/email/42?folder_id=inbox'}, elt: {closest: () => null}}}
  handlers['htmx:beforeRequest'].forEach(fn => fn(event))
  assert.deepEqual(selected, [['42', false]], 'the row is marked while the message loads')
  assert.deepEqual(markedRead, [], 'nothing is marked read before the message arrived')
  handlers['htmx:afterRequest'].forEach(fn => fn(event))
  assert.deepEqual(selected, [['42', false]], 'the row is not marked a second time')
  assert.deepEqual(markedRead, ['42'], 'the message is marked read once it arrived')

  const dialog = {detail: {pathInfo: {requestPath: '/email/42/move'}, elt: null}}
  handlers['htmx:beforeRequest'].forEach(fn => fn(dialog))
  assert.equal(selected.length, 1, 'other requests about the message leave the list alone')
}

console.log('Mail open selection tests passed')
