const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const slice = (start, end) => {
  const from = source.indexOf(start)
  const to = source.indexOf(end, from)
  assert.ok(from >= 0 && to > from, `app.js has no "${start}" … "${end}"`)
  return source.slice(from, to)
}

// A toolbar is a list of buttons in page order. A button in a menu sits
// inside popover content; a hidden one has no boxes; a folded one keeps its
// boxes but sits in a part marked data-mail-collapsed.
let buttons = []
let focused = null
const toolbar = {querySelectorAll: () => buttons}
const button = (name, extra = {}) => ({
  name, shown: true, folded: false, inMenu: false, disabled: false, tabIndex: 0, ...extra,
  closest(selector) {
    if (selector === '[data-tui-popover-content]') return this.inMenu ? {} : null
    if (selector === '[data-mail-collapsed]') return this.folded ? {} : null
    if (selector === '[data-toolbar-arrows]') return toolbar
    return null
  },
  getClientRects() { return this.shown ? [{}] : [] },
  focus() { focused = this },
})

const listeners = {}
const context = vm.createContext({
  document: {addEventListener: (type, fn) => { listeners[type] = fn }},
})
vm.runInContext(slice('// A toolbar marked data-toolbar-arrows', '// The compose toolbar folds its tools'), context)

const press = (target, key, extra = {}) => {
  focused = null
  const event = {key, target, ...extra, preventDefault() { this.defaultPrevented = true }}
  listeners.keydown(event)
  return event.defaultPrevented ? focused : null
}
const stops = () => buttons.filter(b => !b.inMenu && b.tabIndex === 0).map(b => b.name)

const undo = button('undo', {folded: true})
const bold = button('bold')
const italic = button('italic')
const color = button('color')
const swatch = button('swatch', {inMenu: true})
const strike = button('strike', {shown: false})
const attach = button('attach')
const format = button('format')
buttons = [undo, bold, italic, color, swatch, strike, attach, format]

// The first button shown is the one Tab stop; a menu's own buttons are left
// as they are.
context.syncToolbarTabStop(toolbar)
assert.deepEqual(stops(), ['bold'])
assert.equal(undo.tabIndex, -1)
assert.equal(swatch.tabIndex, 0)

// The arrows move between the buttons shown, wrapping at the ends, and the
// stop follows.
assert.equal(press(bold, 'ArrowRight').name, 'italic')
assert.deepEqual(stops(), ['italic'])
assert.equal(press(color, 'ArrowRight').name, 'attach')
assert.equal(press(bold, 'ArrowLeft').name, 'format')
assert.equal(press(format, 'ArrowRight').name, 'bold')
assert.equal(press(attach, 'Home').name, 'bold')
assert.equal(press(bold, 'End').name, 'format')
assert.deepEqual(stops(), ['format'])

// A disabled button is skipped, and a stop that gets disabled moves on.
format.disabled = true
assert.equal(press(attach, 'ArrowRight').name, 'bold')
context.syncToolbarTabStop(toolbar, attach)
assert.deepEqual(stops(), ['attach'])
attach.disabled = true
context.syncToolbarTabStop(toolbar)
assert.deepEqual(stops(), ['bold'])
attach.disabled = false
format.disabled = false

// A stop that folds away moves to the first button shown, a button that
// unfolds keeps it where it is, and a hidden toolbar keeps its stop.
context.syncToolbarTabStop(toolbar, italic)
italic.folded = true
context.syncToolbarTabStop(toolbar)
assert.deepEqual(stops(), ['bold'])
assert.equal(press(bold, 'ArrowRight').name, 'color')
assert.equal(press(color, 'ArrowLeft').name, 'bold')
italic.folded = false
undo.folded = false
context.syncToolbarTabStop(toolbar)
assert.deepEqual(stops(), ['bold'])
assert.equal(press(bold, 'Home').name, 'undo')
undo.folded = true
context.syncToolbarTabStop(toolbar)
assert.deepEqual(stops(), ['bold'])
const wasShown = buttons.map(b => b.shown)
buttons.forEach(b => { b.shown = false })
context.syncToolbarTabStop(toolbar)
assert.deepEqual(stops(), ['bold'])
buttons.forEach((b, i) => { b.shown = wasShown[i] })

// Keys inside a menu, keys with a modifier and keys another handler took
// keep their own meaning.
assert.equal(press(swatch, 'ArrowRight'), null)
assert.equal(press(bold, 'ArrowRight', {shiftKey: true}), null)
assert.equal(press(bold, 'ArrowRight', {ctrlKey: true}), null)
assert.equal(press(bold, 'ArrowRight', {defaultPrevented: true}), null)
assert.equal(press(bold, 'ArrowDown'), null)

// A button focused some other way, such as a menu handing it back, becomes
// the stop.
listeners.focusin({target: color})
assert.deepEqual(stops(), ['color'])
listeners.focusin({target: swatch})
assert.deepEqual(stops(), ['color'])

console.log('toolbar arrow keys tests passed')
