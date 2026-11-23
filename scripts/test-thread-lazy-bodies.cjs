// Opening a long conversation: a collapsed message's body is only filled in,
// and loaded, when the message opens.
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

// Opening a collapsed message fills in its body from the template.
{
  const processed = []
  const dispatched = []
  const delays = []
  const context = vm.createContext({
    CustomEvent: class { constructor(type) { this.type = type } },
    window: {htmx: true},
    htmx: {process: el => processed.push(el)},
    setTimeout: (fn, ms) => { delays.push(ms); return delays.length },
    clearTimeout() {},
  })
  vm.runInContext(slice('function contentOf(el)', 'function clipSlot('), context)

  const body = {cloned: true}
  let placed = null
  const template = {content: {cloneNode: deep => (assert.equal(deep, true), body)}, replaceWith: node => { placed = node }}
  const ct = {
    querySelector: selector => (selector === ':scope > template[data-thread-body]' && !placed ? template : null),
    dispatchEvent: event => dispatched.push(event.type),
  }
  const details = {querySelector: selector => (selector === '.thread-details-content' ? ct : null)}
  context.fillThreadBody(details)
  assert.equal(placed, body, 'the template is replaced by its body')
  assert.deepEqual(processed, [ct], 'htmx sees the new controls')
  assert.deepEqual(dispatched, ['gofer:thread-body-added'], 'the translate control is told about the new body')
  context.fillThreadBody(details)
  assert.equal(processed.length, 1, 'a message already filled in is left as it is')

  // A body that never loaded gets longer to report its height.
  const frame = (reported) => ({_bodyReported: reported, _bodyMeasuredWidth: 0, clientWidth: 600, addEventListener() {}, removeEventListener() {}})
  const content = (iframe) => ({querySelector: () => iframe})
  context.whenBodiesMeasured([content(frame(false))], () => {})
  context.whenBodiesMeasured([content(frame(true))], () => {})
  assert.deepEqual(delays, [500, 150], 'a new body waits longer than one re-measuring')
}

console.log('Thread lazy bodies tests passed')
