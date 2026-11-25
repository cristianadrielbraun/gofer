const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const slice = (start, end) => {
  const from = source.indexOf(start)
  assert.ok(from >= 0, `${start} is missing from app.js`)
  return source.slice(from, source.indexOf(end, from))
}

const listeners = new Set()
const timers = []
const context = vm.createContext({
  window: {innerHeight: 800, innerWidth: 1000},
  document: {
    body: null,
    addEventListener(type, fn) { if (type === 'mousedown') listeners.add(fn) },
    removeEventListener(type, fn) { if (type === 'mousedown') listeners.delete(fn) },
  },
  setTimeout: fn => timers.push(fn),
})
vm.runInContext(slice('function _openComposeMenu(', 'function closeComposeSignatureMenu('), context)

const host = name => ({name, children: [], appendChild(child) { this.children.push(child); child.parentNode = this; child.isConnected = true }})
const body = host('body')
context.document.body = body
const dialog = host('dialog')

const menu = () => {
  const item = {}
  const el = {
    style: {}, offsetHeight: 60, offsetWidth: 160, isConnected: false, shown: false, item,
    showPopover() { this.shown = true },
    contains(node) { return node === this || node === item },
    remove() { this.isConnected = false },
  }
  return el
}
const anchorIn = parent => ({closest: s => (s === 'dialog' ? parent : null), getBoundingClientRect: () => ({left: 200, bottom: 300})})
const press = target => [...listeners].forEach(fn => fn({target}))
const flush = () => timers.splice(0).forEach(fn => fn())

// In the compose dialog the menu opens inside it, as a popover, so it sits
// over the modal dialog and takes clicks.
let closes = 0
const picker = menu()
context._openComposeMenu(picker, anchorIn(dialog), () => { closes++; picker.remove() })
flush()
assert.equal(picker.parentNode, dialog)
assert.equal(picker.popover, 'manual')
assert.equal(picker.shown, true)
assert.equal(picker.style.top, '306px')
assert.equal(picker.style.left, '200px')

// Pressing one of its items leaves it open, so the item's click happens.
press(picker.item)
assert.equal(closes, 0)
assert.equal(picker.isConnected, true)

// A press outside closes it once.
press({})
assert.equal(closes, 1)
press({})
assert.equal(closes, 1)
assert.equal(listeners.size, 0)

// Inline compose has no dialog, so the menu goes in the page.
const inline = menu()
context._openComposeMenu(inline, anchorIn(null), () => inline.remove())
flush()
assert.equal(inline.parentNode, body)

// A menu that an item's click already closed doesn't close a newer one on
// the next press.
inline.remove()
let newerCloses = 0
const newer = menu()
context._openComposeMenu(newer, anchorIn(dialog), () => { newerCloses++; newer.remove() })
flush()
press(newer.item)
assert.equal(newerCloses, 0)
assert.equal(newer.isConnected, true)

// Near the window's edge the menu stays inside it.
const edge = menu()
context._openComposeMenu(edge, {closest: () => null, getBoundingClientRect: () => ({left: 950, bottom: 780})}, () => {})
assert.equal(edge.style.top, '732px')
assert.equal(edge.style.left, '832px')

console.log('compose menus ok')
