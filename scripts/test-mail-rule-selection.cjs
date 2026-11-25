const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// Selecting rules in Settings → Rules, and moving several at once
// (settings.js).
const settings = fs.readFileSync(path.join(__dirname, '../assets/js/settings.js'), 'utf8')
const slice = (from, to) => {
  const start = settings.indexOf(from)
  const end = settings.indexOf(to, start)
  assert(start >= 0 && end > start, from)
  return settings.slice(start, end)
}

// Selected rules move one place, past the rule beside them, keeping their
// order; a block at the end stays.
const order = vm.createContext({})
vm.runInContext(slice('function orderStepSelected(', 'function moveOrderSelected('), order)
const step = (ids, selected, by) => order.orderStepSelected(ids.split(''), Object.fromEntries(selected.split('').map(id => [id, true])), by).join('')
assert.equal(step('abcde', 'bd', -1), 'badce', 'each selected rule moves up past the one above')
assert.equal(step('abcde', 'bd', 1), 'acbed', 'and down past the one below')
assert.equal(step('abcde', 'ab', -1), 'abcde', 'a block at the top stays')
assert.equal(step('abcde', 'ade', 1), 'bacde', 'a block at the bottom stays, the rest moves')
assert.equal(step('abcde', 'ace', -1), 'acbed', 'the top one stays, the others move')

// The list: three rules, a box that selects them all, and the bar.
const box = () => ({checked: false, matches: s => s === '[data-mail-rule-select]'})
const rule = (id, name, tags = []) => {
  const each = box()
  return {id, name, box: each, hidden: false, attrs: {},
    toggleAttribute(n, on) { if (on) this.attrs[n] = ''; else delete this.attrs[n] },
    getAttribute: n => n === 'data-mail-rule-id' ? id : n === 'data-mail-rule-tags' ? JSON.stringify(tags) : null,
    querySelector: s => s === '[data-mail-rule-select]' ? each : s === '[data-mail-rule-name]' ? {getAttribute: () => name} : null}
}
const rows = [rule('r1', 'Offers'), rule('r2', 'Orders', ['Shopping']), rule('r3', 'Lunch on Friday')]
const removeTags = {disabled: false}
const all = {checked: false, indeterminate: false, disabled: false, matches: s => s === '[data-mail-rules-select-all]'}
const count = {textContent: ''}
const moves = [{disabled: false}, {disabled: false}]
const list = {attrs: {}, hasAttribute(n) { return n in this.attrs }, toggleAttribute(n, on) { if (on) this.attrs[n] = ''; else delete this.attrs[n] }}
const bar = {hidden: true, style: {}, querySelector: s => ({'[data-mail-rules-select-all]': all, '[data-mail-rules-selected-count]': count})[s] || null}
const panel = {
  querySelector: s => ({'[data-mail-rules-selection]': bar, '[data-mail-rule-order-list]': list})[s] || null,
  querySelectorAll: s => s === '[data-mail-rule-row]' ? rows : s === '[data-mail-rule-select]' ? rows.map(r => r.box) : [],
}
const listeners = {}
const requests = []
const confirms = []
const moved = []
let typed = ''
const context = vm.createContext({
  document: {
    addEventListener(name, listener) { (listeners[name] = listeners[name] || []).push(listener) },
    getElementById: id => id === 'mail-rules-panel' ? panel : null,
    querySelectorAll: s => s.includes('data-mail-rules-selection-action="up"') ? moves : [],
    querySelector: s => {
      if (s === '[data-mail-rules-selection-action="tags-remove"]') return removeTags
      const id = /data-mail-rule-id="([^"]+)"\] \[data-mail-rule-select\]$/.exec(s)
      return id ? (rows.find(r => r.id === id[1]) || {}).box || null : null
    },
  },
  window: {
    goferFolderPicker: {filter() { for (const r of rows) r.hidden = typed !== '' && !r.name.toLowerCase().includes(typed); return typed }},
    htmx: {ajax(method, url, options) { requests.push({method, url, values: options.values}) }},
    goferConfirm(options) { confirms.push(options); return {then(next) { next(true) }} },
  },
  mailRuleOrder: {id: row => row.id, selected: row => row.box.checked},
  moveOrderSelected(kind, target, by) { moved.push(by) },
  mailRuleFold: () => null,
  goferT: (key, params) => key + (params ? JSON.stringify(params) : ''),
  goferTn: (key, n) => key + ':' + n,
  cssEscapeSettings: v => v,
})
vm.runInContext(slice('function filterMailRules(still) {', 'document.body.addEventListener("htmx:beforeSwap"'), context)
const fire = (name, event) => { for (const listener of listeners[name] || []) listener(event) }
const click = attrs => fire('click', {target: {closest: s => {
  for (const [key, value] of Object.entries(attrs)) if (s === `[${key}]`) return {disabled: false, getAttribute: () => value}
  return null
}}})
const pick = (row, on) => { row.box.checked = on; fire('change', {target: row.box}) }

context.updateMailRuleSelection()
assert.equal(bar.hidden, true, 'the bar stays closed until a rule is selected')

pick(rows[0], true)
assert.equal(count.textContent, 'js.settings.rules.selected:1')
assert.equal(bar.hidden, false, 'and opens with the first')
assert.equal(all.indeterminate, true, 'some selected: the box is partly checked')

all.checked = true
fire('change', {target: all})
assert.deepEqual(rows.map(r => r.box.checked), [true, true, true], 'select all selects every rule shown')
assert.equal(all.checked, true)
assert.equal(all.indeterminate, false)

// A search keeps only the rules it shows selected, and the moves are off.
typed = 'o'
context.filterMailRules()
typed = 'orders'
context.filterMailRules()
assert.deepEqual(rows.map(r => r.box.checked), [false, true, false], 'rules the search hides are no longer selected')
assert.equal(count.textContent, 'js.settings.rules.selected:1')
assert.ok(moves.every(item => item.disabled), 'moving is off while searching')
typed = ''
context.filterMailRules()
assert.ok(moves.every(item => !item.disabled))

all.checked = false
fire('change', {target: all})
assert.deepEqual(rows.map(r => r.box.checked), [false, false, false], 'unchecking the box selects none')
assert.equal(bar.hidden, true, 'and closes the bar')

// What the menu does to the selected rules.
pick(rows[0], true)
pick(rows[2], true)
click({'data-mail-rules-selection-action': 'off'})
assert.equal(JSON.stringify(requests.pop()), JSON.stringify({method: 'POST', url: '/api/settings/rules/selected/enabled', values: {rule_ids: 'r1,r3', enabled: ''}}))
click({'data-mail-rules-selection-action': 'on'})
assert.equal(requests.pop().values.enabled, '1')
click({'data-mail-rules-selection-action': 'up'})
click({'data-mail-rules-selection-action': 'down'})
assert.deepEqual(moved, [-1, 1])
click({'data-mail-rules-selection-action': 'delete'})
assert.equal(confirms.pop().title, 'js.settings.rules.delete_selected_title:2', 'deleting several asks, saying how many')
assert.equal(JSON.stringify(requests.pop()), JSON.stringify({method: 'POST', url: '/api/settings/rules/selected/delete', values: {rule_ids: 'r1,r3'}}))
pick(rows[2], false)
click({'data-mail-rules-selection-action': 'delete'})
assert.match(confirms.pop().message, /Offers/, 'deleting one names it')

// Categories: the dialogs load for the selected rules, and removing is off
// until one of them has a category.
click({'data-mail-rules-selection-clear': ''})
pick(rows[0], true)
assert.equal(removeTags.disabled, true, 'no selected rule has a category: nothing to remove')
pick(rows[1], true)
assert.equal(removeTags.disabled, false, 'one does: its categories can be removed')
click({'data-mail-rules-selection-action': 'tags-add'})
assert.deepEqual(requests.pop(), {method: 'GET', url: '/api/settings/rules/selected/tags?rule_ids=r1%2Cr2', values: undefined})
click({'data-mail-rules-selection-action': 'tags-remove'})
assert.equal(requests.pop().url, '/api/settings/rules/selected/tags?rule_ids=r1%2Cr2&remove=1')

click({'data-mail-rules-selection-clear': ''})
assert.deepEqual(rows.map(r => r.box.checked), [false, false, false], 'clear selects none')
assert.equal(bar.hidden, true)

// The list drawn again keeps the rules selected before, those still there.
context.searchMailRulesAgain({loaded: true, rules: ['r1', 'r2', 'r3'], selected: ['r2', 'gone']})
assert.deepEqual(rows.map(r => r.box.checked), [false, true, false], 'a redrawn list keeps the selection')
assert.equal(count.textContent, 'js.settings.rules.selected:1')
console.log('Rules selection: select all, search, menu actions, categories, moving several and redraws passed.')
