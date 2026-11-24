const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// The rules search in Settings → Rules uses the folder picker's filter
// (folder-picker.js) through filterMailRules in settings.js.
const settings = fs.readFileSync(path.join(__dirname, '../assets/js/settings.js'), 'utf8')
const start = settings.indexOf('function filterMailRules() {')
const end = settings.indexOf('document.body.addEventListener("htmx:beforeSwap"', start)
assert(start >= 0 && end > start)

const listeners = {}
const window = {}
const rule = (id, searchText) => ({hidden: false, dataset: {searchText}, getAttribute: name => name === 'data-mail-rule-id' ? id : null})
let rows = [rule('r1', 'shop receipts robin@example.test · move to finance / receipts'), rule('r2', 'weekly digest subject starts with lunch on friday?'), rule('r3', 'travel plans move to trips')]
const list = {
  hidden: false, attrs: {},
  querySelectorAll: selector => selector === '[data-folder-picker-row]' ? rows : [],
  querySelector: () => null,
  toggleAttribute(name, on) { if (on) this.attrs[name] = ''; else delete this.attrs[name] },
}
const search = {value: '', matches: selector => selector === '[data-mail-rules-search]'}
const empty = {hidden: true, classList: {toggle(name, on) { empty.hidden = on }}}
const panel = {
  closest: () => null,
  querySelector: selector => ({'[data-folder-picker-search]': search, '[data-folder-picker-empty]': empty, '[data-mail-rule-order-list]': list})[selector] || null,
  querySelectorAll: selector => selector === '[data-folder-picker-account]' ? [list] : [],
}
const document = {
  addEventListener(name, listener) { (listeners[name] = listeners[name] || []).push(listener) },
  getElementById: id => id === 'mail-rules-panel' ? panel : null,
  querySelector: selector => selector === '#mail-rules-panel [data-mail-rules-search]' ? search : null,
  querySelectorAll: selector => selector === '#mail-rules-panel [data-mail-rule-row]' ? rows : [],
}
const context = vm.createContext({window, document})
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/folder-picker.js'), 'utf8'), context)
vm.runInContext(settings.slice(start, end), context)
const type = text => {
  search.value = text
  for (const listener of listeners.input) listener({target: search})
}
const shown = () => rows.filter(row => !row.hidden).map(row => row.getAttribute('data-mail-rule-id')).join(',')

type('Receipts')
assert.equal(shown(), 'r1', 'a destination folder finds its rule')
assert.ok('data-searching' in list.attrs, 'rules are not dragged while some are hidden')
type('friday')
assert.equal(shown(), 'r2', 'a condition finds its rule')
type('nothing like it')
assert.equal(shown(), '')
assert.equal(list.hidden, true)
assert.equal(empty.hidden, false, 'no match says so')
type('')
assert.equal(shown(), 'r1,r2,r3', 'an empty search shows every rule')
assert.ok(!('data-searching' in list.attrs), 'and rules can be dragged again')

// The list drawn again after an edit keeps the search and what it finds.
type('trips')
rows = [rule('r1', 'shop receipts'), rule('r2', 'weekly digest'), rule('r3', 'travel plans move to trips')]
context.searchMailRulesAgain({loaded: true, rules: ['r1', 'r2', 'r3']})
assert.equal(search.value, 'trips')
assert.equal(shown(), 'r3', 'a list drawn again is searched again')

// A rule just added shows even when it doesn't match: the search is cleared.
rows = [...rows, rule('r4', 'invoices')]
context.searchMailRulesAgain({loaded: true, rules: ['r1', 'r2', 'r3']})
assert.equal(search.value, '')
assert.equal(shown(), 'r1,r2,r3,r4')
console.log('Rules search: name, condition, folder, empty and redraw searches passed.')
