const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('  function setupMailFilters() {')
const end = source.indexOf('  function setupBodyPrefetch()', start)
assert(start >= 0 && end > start, 'Mail filter setup was not found')

function node(attributes = {}) {
  const classes = new Set()
  return {
    value: '', textContent: '', type: 'text', dataset: {},
    getAttribute(name) { return attributes[name] ?? null },
    setAttribute(name, value) { attributes[name] = value },
    removeAttribute(name) { delete attributes[name] },
    classList: {
      toggle(name, active) { if (active) classes.add(name); else classes.delete(name) },
      contains(name) { return classes.has(name) },
      add(name) { classes.add(name) },
      remove(name) { classes.delete(name) },
    },
  }
}

const listeners = {}, timers = new Map(), applied = [], closed = []
let timerID = 0
const counter = node(), filterButton = node(), filterBadge = node()
const controls = Object.fromEntries(['status', 'attachments', 'tags', 'threads'].map(name => {
  const control = node({'data-mail-tristate': name, 'data-mail-tristate-value': ''})
  const values = name === 'status' ? ['unread', '', 'read'] : ['no', '', 'yes']
  control.options = values.map(value => {
    const option = node({'data-mail-tristate-option': value})
    option.closest = selector => selector === '[data-mail-tristate-option]' ? option :
      selector === '[data-mail-tristate]' ? control : selector === '[data-mail-advanced-filter-form]' ? form : null
    return option
  })
  control.querySelectorAll = selector => selector === '[data-mail-tristate-option]' ? control.options : []
  return [name, control]
}))
const inputNames = ['participant', 'account_id', 'after_date', 'before_date', 'from', 'from_domain', 'to',
  'recipient_type', 'recipient_domain', 'subject', 'body', 'attachment', 'attachment_type',
  'attachment_extension', 'min_size_mb', 'max_size_mb', 'tag']
const inputs = Object.fromEntries(inputNames.map(name => [name, node()]))
const form = {
  closest(selector) { return selector === '[data-mail-advanced-filter-form]' ? form : null },
  querySelector(selector) {
    const tri = selector.match(/^\[data-mail-tristate="([^"]+)"\]$/)
    if (tri) return controls[tri[1]] || null
    const input = selector.match(/^(?:input)?\[name="([^"]+)"\]$/)
    return input ? inputs[input[1]] || null : null
  },
  querySelectorAll(selector) {
    if (selector === 'input') return Object.values(inputs)
    if (selector === '[data-mail-tristate]') return Object.values(controls)
    return []
  },
}
const document = {
  body: {addEventListener() {}},
  getElementById() { return null },
  querySelector(selector) {
    return {
      '[data-mail-advanced-filter-form]': form,
      '[data-mail-advanced-filter-count]': counter,
      '[data-mail-filter-button]': filterButton,
      '[data-mail-filter-count]': filterBadge,
    }[selector] || null
  },
  querySelectorAll(selector) {
    const tri = selector.match(/^\[data-mail-tristate="([^"]+)"\]$/)
    return tri && controls[tri[1]] ? [controls[tri[1]]] : []
  },
  addEventListener(type, callback) { (listeners[type] ||= []).push(callback) },
}
const window = {
  addEventListener() {},
  setTimeout(callback) { timers.set(++timerID, callback); return timerID },
  clearTimeout(id) { timers.delete(id) },
  tui: {popover: {close(id) { closed.push(id) }}},
}
const context = vm.createContext({document, window, URLSearchParams, virtualMailList: null,
  setTimeout: window.setTimeout, clearTimeout: window.clearTimeout})
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/virtual-scroll.js'), 'utf8'), context)
vm.runInContext(source.slice(start, end) + '\nsetupMailFilters()', context)
context.virtualMailList = {
  applyFilters(filters) {
    const serializer = Object.create(window.VirtualMailList.prototype)
    serializer.filters = filters
    applied.push({filters, query: new URLSearchParams(serializer.filterQueryString()),
      request: new URL(serializer.withFilterParams('/api/emails?folder=inbox'), 'http://localhost')})
    return Promise.resolve()
  },
}

function fire(type, target) {
  const event = {target, prevented: false, preventDefault() { this.prevented = true }}
  for (const listener of listeners[type] || []) listener(event)
  return event
}
function button(attribute) {
  return {closest(selector) { return selector === '[' + attribute + ']' ? this : null }}
}
function click(name, value) {
  const option = controls[name].options.find(option => option.getAttribute('data-mail-tristate-option') === value)
  assert.equal(fire('click', option).prevented, true)
  assert.equal(controls[name].getAttribute('data-mail-tristate-value'), value)
}
function count(expected) {
  assert.equal(counter.textContent, String(expected))
  assert.equal(counter.classList.contains('hidden'), expected === 0)
}
function submit() {
  assert.equal(fire('submit', form).prevented, true)
  for (const [id, callback] of timers) { timers.delete(id); callback() }
  assert.equal(closed.at(-1), 'mail-filters-popover')
  return applied.at(-1)
}

fire('click', button('data-mail-filter-button'))
count(0)
const cases = [
  ['status', 'unread', 'unread', 'unread', 'read'],
  ['status', 'read', 'read', 'read', 'unread'],
  ['attachments', 'no', 'noAttachments', 'no_attachments', 'attachments'],
  ['attachments', 'yes', 'attachments', 'attachments', 'no_attachments'],
  ['tags', 'no', 'noTags', 'no_tags', 'has_tags'],
  ['tags', 'yes', 'hasTags', 'has_tags', 'no_tags'],
  ['threads', 'no', 'noThreads', 'no_threads', 'threads_only'],
  ['threads', 'yes', 'threadsOnly', 'threads_only', 'no_threads'],
]
for (const [name, value, key, param, opposite] of cases) {
  const before = applied.length
  click(name, value)
  count(1)
  assert.equal(applied.length, before, 'Changing a status tab waits for Apply')
  const result = submit()
  assert.equal(result.filters[key], true)
  for (const params of [result.query, result.request.searchParams]) {
    assert.equal(params.get(param), '1', 'Apply sends the selected filter to the server')
    assert.equal(params.has(opposite), false, 'Opposite status must be cleared')
  }
  assert.equal(filterBadge.textContent, '1')
  click(name, '')
  count(0)
  const cleared = submit()
  assert.equal(cleared.filters[key], false)
  assert.equal(cleared.query.has(param), false, 'Any removes the filter')
  assert.equal(cleared.request.searchParams.has(param), false)
}

click('status', 'unread')
click('status', 'read')
count(1)
assert.equal(submit().query.has('unread'), false, 'Switching sides replaces the previous status')
click('attachments', 'yes')
click('tags', 'no')
click('threads', 'yes')
inputs.subject.value = 'invoice'
fire('input', form)
count(5)
const combined = submit().query
for (const name of ['read', 'attachments', 'no_tags', 'threads_only']) assert.equal(combined.get(name), '1')
assert.equal(combined.get('subject'), 'invoice', 'Status filters combine with text filters')

fire('click', button('data-mail-advanced-filter-clear'))
count(0)
assert.equal(inputs.subject.value, '')
const cleared = submit().query
for (const [, , , param] of cases) assert.equal(cleared.has(param), false, 'Clear all resets every status')

window.syncMailFilterControls({unread: true, attachments: true, hasTags: true, noThreads: true})
fire('click', button('data-mail-filter-button'))
count(4)
const restored = submit().query
for (const name of ['unread', 'attachments', 'has_tags', 'no_threads']) assert.equal(restored.get(name), '1')
console.log('Mail filters: all status choices, Apply counters, request parameters, Any, replacement, combined filters, Clear all, and restored state passed.')
