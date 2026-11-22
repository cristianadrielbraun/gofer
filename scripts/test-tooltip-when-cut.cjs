const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// A tooltip that repeats a label in full (tooltip.TriggerProps.WhenCut, used
// by folder picker rows and the signature list) opens on hover only while
// the label is cut short.
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('// A tooltip that repeats a label in full')
const end = source.indexOf('}, true)', start) + 8
assert(start > 0 && end > start, 'Missing the WhenCut tooltip handler')

const listeners = {}
const document = {addEventListener(name, listener, capture) { listeners[name] = {listener, capture} }}
vm.runInNewContext(source.slice(start, end), {document})
assert.equal(listeners.pointerover.capture, true, 'it runs before the tooltip opens')

const tip = (...widths) => {
  const trigger = {attributes: {}, setAttribute(name, value) { this.attributes[name] = value },
    querySelectorAll(selector) { assert.equal(selector, '.truncate'); return widths.map(([scrollWidth, clientWidth]) => ({scrollWidth, clientWidth})) }}
  listeners.pointerover.listener({target: {closest: selector => selector === '[data-tooltip-when-cut]' ? trigger : null}})
  return trigger.attributes['data-tui-popover-type']
}
assert.equal(tip([80, 120], [200, 200]), 'manual', 'a label that fits has no tooltip')
assert.equal(tip([80, 120], [340, 300]), 'hover', 'a cut second line shows it')
assert.equal(tip([400, 300]), 'hover', 'a cut name shows it')
listeners.pointerover.listener({target: {closest: () => null}})

console.log('Cut-label tooltips: open only while the label is cut short.')
