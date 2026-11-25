const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// The tags field (assets/js/tagsinput.js), as the rule form's categories
// use it: a typed tag is text, never markup; a chip goes with its hidden
// input whatever its text; and text left in the field becomes a tag when
// the field is left, so sending the form keeps it.

// A small stand-in for the DOM: elements with attributes, children and the
// selectors the script uses.
class El {
  constructor(tag, attrs = {}) {
    this.tagName = tag
    this.attrs = {...attrs}
    this.children = []
    this.parent = null
    this.textContent = ''
    this.innerHTML = ''
    this.value = ''
    this.disabled = false
  }
  setAttribute(n, v) { this.attrs[n] = String(v) }
  getAttribute(n) { return n in this.attrs ? this.attrs[n] : null }
  hasAttribute(n) { return n in this.attrs }
  appendChild(child) { child.parent = this; this.children.push(child); return child }
  remove() {
    if (!this.parent) return
    this.parent.children = this.parent.children.filter(c => c !== this)
    this.parent = null
  }
  get lastElementChild() { return this.children[this.children.length - 1] || null }
  matchesOne(selector) {
    const attr = /^\[([\w-]+)\]$/.exec(selector)
    return attr ? this.hasAttribute(attr[1]) : this.tagName === selector
  }
  matches(selector) {
    const parts = selector.trim().split(/\s+/)
    if (!this.matchesOne(parts.pop())) return false
    let node = this.parent
    for (let i = parts.length - 1; i >= 0; i--) {
      while (node && !node.matchesOne(parts[i])) node = node.parent
      if (!node) return false
      node = node.parent
    }
    return true
  }
  closest(selector) {
    for (let node = this; node; node = node.parent) if (node.matches(selector)) return node
    return null
  }
  descendants() { return this.children.flatMap(c => [c, ...c.descendants()]) }
  querySelectorAll(selector) { return this.descendants().filter(e => e.matches(selector)) }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null }
  contains(node) {
    for (; node; node = node.parent) if (node === this) return true
    return false
  }
  focus() {}
}

const listeners = {}
const document = {
  addEventListener(name, listener) { (listeners[name] = listeners[name] || []).push(listener) },
  createElement: tag => new El(tag),
  getElementById: () => null,
  querySelector: () => null,
}
const context = vm.createContext({document, window: {}})
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/tagsinput.js'), 'utf8'), context)
const fire = (name, event) => { for (const listener of listeners[name] || []) listener({preventDefault() {}, ...event}) }

const page = new El('body')
const container = page.appendChild(new El('div', {'data-tui-tagsinput': '', 'data-tui-tagsinput-name': 'tags', 'data-tui-tagsinput-remove-label': 'Quitar {tag}'}))
const chips = container.appendChild(new El('div', {'data-tui-tagsinput-chips': ''}))
const input = container.appendChild(new El('input', {'data-tui-tagsinput-text-input': ''}))
const hidden = container.appendChild(new El('div', {'data-tui-tagsinput-hidden-inputs': ''}))
const outside = page.appendChild(new El('button'))
const tags = () => hidden.children.map(h => h.value)
const type = text => { input.value = text; fire('keydown', {key: 'Enter', target: input}) }

type('<b>Finance</b>')
const chip = chips.children[0]
assert.equal(chip.querySelector('span').textContent, '<b>Finance</b>', 'a tag is shown as the text typed')
assert.equal(chip.querySelector('span').innerHTML, '', 'never as markup')
assert.deepEqual(tags(), ['<b>Finance</b>'])
assert.equal(hidden.children[0].name, 'tags', 'it is sent with the field\'s name')
assert.equal(chip.querySelector('[data-tui-tagsinput-remove]').getAttribute('aria-label'), 'Quitar <b>Finance</b>', 'its remove button says what it removes')

type('Say "hi"')
type('trips')
const quoted = chips.children[1]
fire('click', {target: quoted.querySelector('[data-tui-tagsinput-remove]')})
assert.deepEqual(tags(), ['<b>Finance</b>', 'trips'], 'removing a chip with quotes takes its value too')
assert.equal(chips.children.length, 2)

type('TRIPS')
assert.deepEqual(tags(), ['<b>Finance</b>', 'trips'], 'a tag already there, whatever its capitals, is not added twice')

// Text left in the field becomes a tag when the field is left for the rest
// of the page, as for the form's Save button, but not within the field.
input.value = 'Bills'
fire('focusout', {target: input, relatedTarget: chips})
assert.deepEqual(tags(), ['<b>Finance</b>', 'trips'], 'moving within the field adds nothing')
fire('focusout', {target: input, relatedTarget: outside})
assert.deepEqual(tags(), ['<b>Finance</b>', 'trips', 'Bills'], 'leaving it adds what was typed')
assert.equal(input.value, '')
fire('focusout', {target: input, relatedTarget: null})
assert.equal(tags().length, 3, 'leaving it empty adds nothing')

input.value = ''
fire('keydown', {key: 'Backspace', target: input})
assert.deepEqual(tags(), ['<b>Finance</b>', 'trips'], 'backspace in an empty field takes the last tag off')
console.log('Tags input: text not markup, quoted removal, no duplicates, typed text kept on leaving, backspace passed.')
