const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const slice = (start, end) => source.slice(source.indexOf(start), source.indexOf(end))

// A page is a list of elements in document order. Each element names the
// selectors it sits inside; closest() answers from those.
let page = []
let focused = null
const element = (name, inside = [], extra = {}) => ({
  name, inside, visible: true, tabindex: null, disabled: false, ...extra,
  getAttribute(attribute) { return attribute === 'tabindex' ? this.tabindex : null },
  closest(selector) {
    if (selector === '#compose-dialog, [data-compose-pane]') return this.surface || null
    return selector.split(',').some(s => this.inside.includes(s.trim())) ? {} : null
  },
  matches() { return true },
  getClientRects() { return this.visible ? [{}] : [] },
  focus() { focused = this },
})
const surface = (pane, children) => {
  const s = {
    pane, children,
    matches: selector => selector === '[data-compose-pane]' && pane,
    querySelectorAll: () => children,
    contains: el => children.includes(el),
    compareDocumentPosition: el => page.indexOf(el) > page.indexOf(children[children.length - 1]) ? 4 : 2,
  }
  children.forEach(child => { child.surface = s })
  return s
}

let keydown
const context = vm.createContext({
  Map,
  Node: {DOCUMENT_POSITION_FOLLOWING: 4},
  document: {
    addEventListener: (type, fn) => { if (type === 'keydown') keydown = fn },
    querySelectorAll: () => page,
  },
})
vm.runInContext(slice('// Tab moves through compose in the order', 'document.addEventListener("selectionchange"'), context)

const press = (target, shiftKey = false, defaultPrevented = false) => {
  focused = null
  const event = {key: 'Tab', target, shiftKey, defaultPrevented, preventDefault() { this.defaultPrevented = true }}
  keydown(event)
  return event.defaultPrevented ? focused : null
}
const walk = (start, steps, shift) => {
  const names = []
  let at = start
  for (let i = 0; i < steps; i++) { at = press(at, shift); names.push(at.name) }
  return names
}

// The dialog, laid out in page order, with Cc and Bcc still buttons.
const expand = element('expand', ['.compose-dialog-corner'])
const discard = element('discard', ['.compose-dialog-corner'])
const from = element('from', ['[data-compose-from-row]'])
const to = element('to', ['[data-recipient-name="to"]'])
const toChip = element('to-chip-remove', ['[data-recipient-name="to"]'])
const ccBtn = element('cc-btn', ['#cc-btn'])
const bccBtn = element('bcc-btn', ['#bcc-btn'])
const cc = element('cc', ['[data-recipient-name="cc"]'], {visible: false})
const subject = element('subject', ['input[name="subject"]'])
const bold = element('bold', ['[data-compose-toolbar]'])
const italic = element('italic', ['[data-compose-toolbar]'])
const color = element('color-swatch', ['[data-compose-toolbar]', '[data-tui-popover-content]'])
const editor = element('body', ['[data-compose-editor]'])
const draft = element('draft', ['[data-compose-draft-button]'])
const schedule = element('schedule', ['[data-compose-schedule-trigger]'])
const send = element('send', ['#compose-send-btn'])
const disabled = element('disabled', ['[data-compose-toolbar]'], {disabled: true})
const skipped = element('skipped', ['[data-compose-toolbar]'], {tabindex: '-1'})
const dialog = surface(false, [expand, discard, from, toChip, to, ccBtn, bccBtn, cc, subject, bold, color, disabled, skipped, italic, editor, draft, schedule, send])
page = [...dialog.children]

const order = ['to-chip-remove', 'to', 'cc-btn', 'bcc-btn', 'subject', 'body', 'send', 'schedule', 'draft', 'bold', 'italic', 'expand', 'discard', 'from']
assert.deepEqual(walk(from, order.length, false), order)
// The dialog is modal, so the ends wrap, and From is one Shift+Tab from To.
assert.equal(press(from).name, 'to-chip-remove')
assert.equal(press(toChip, true).name, 'from')
assert.deepEqual(walk(from, order.length, true), [...order].reverse().slice(1).concat('from'))

// A shown Cc field takes its button's place.
cc.visible = true
ccBtn.visible = false
assert.deepEqual(walk(to, 3, false), ['cc', 'bcc-btn', 'subject'])

// What another handler took, what sits in an open popover, and keys with a
// modifier keep their own behaviour.
assert.equal(press(subject, false, true), null)
assert.equal(press(color), null)
focused = null
keydown({key: 'Tab', ctrlKey: true, target: subject, preventDefault() { throw new Error('taken') }})

// The pane sits in the page, so its ends lead out of it instead of wrapping.
const before = element('sidebar')
const after = element('after-pane')
const paneTo = element('pane-to', ['[data-recipient-name="to"]'])
const paneSend = element('pane-send', ['#compose-pane-send-btn'])
const paneHeader = element('pane-full-width', ['#compose-pane-size-buttons'])
const paneFrom = element('pane-from', ['[data-compose-from-row]'])
const pane = surface(true, [paneHeader, paneFrom, paneTo, paneSend])
page = [before, ...pane.children, after]
assert.deepEqual(walk(paneTo, 3, false), ['pane-send', 'pane-full-width', 'pane-from'])
assert.equal(press(paneFrom).name, 'after-pane')
assert.equal(press(paneTo, true).name, 'sidebar')

console.log('compose tab order tests passed')
