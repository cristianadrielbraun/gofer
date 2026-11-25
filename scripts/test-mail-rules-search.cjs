const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// The rules search in Settings → Rules uses the folder picker's filter
// (folder-picker.js) through filterMailRules in settings.js.
const settings = fs.readFileSync(path.join(__dirname, '../assets/js/settings.js'), 'utf8')
const start = settings.indexOf('function filterMailRules(still) {')
const end = settings.indexOf('document.body.addEventListener("htmx:beforeSwap"', start)
assert(start >= 0 && end > start)

const listeners = {}
const window = {}
const rule = (id, searchText, folders = [], tags = []) => ({
  hidden: false, dataset: {searchText}, attrs: {},
  querySelector: () => null,
  hasAttribute(name) { return name in this.attrs },
  toggleAttribute(name, on) { if (on) this.attrs[name] = ''; else delete this.attrs[name] },
  getAttribute: name => ({'data-mail-rule-id': id, 'data-mail-rule-folders': JSON.stringify(folders), 'data-mail-rule-tags': JSON.stringify(tags)})[name] ?? null,
})
let rows = [rule('r1', 'shop receipts robin@example.test · move to finance / receipts', ['f-receipts']), rule('r2', 'weekly digest subject starts with lunch on friday?', ['f-archive', 'f-receipts']), rule('r3', 'travel plans move to trips', ['f-trips'])]
// The list's filters as the server drew them: the folder whose rules it
// shows, as a folder's menu in the sidebar opens it, and its categories.
let filters = null
const setFilters = (folder, tags) => { filters = {getAttribute: name => name === 'data-filter-folder' ? folder : name === 'data-filter-tags' ? JSON.stringify(tags) : null} }
// Changing them asks for the list again with them.
const requests = []
window.htmx = {ajax(method, url, options) { requests.push({method, url, values: options.values}) }}
const list = {
  hidden: false, attrs: {},
  querySelectorAll: selector => selector === '[data-folder-picker-row]' ? rows : [],
  querySelector: () => null,
  toggleAttribute(name, on) { if (on) this.attrs[name] = ''; else delete this.attrs[name] },
}
const search = {value: '', focused: false, matches: selector => selector === '[data-mail-rules-search]', focus() { search.focused = true }}
const empty = {hidden: true, classList: {toggle(name, on) { empty.hidden = on }}}
const panel = {
  closest: () => null,
  querySelector: selector => ({'[data-folder-picker-search]': search, '[data-mail-rules-search]': search, '[data-folder-picker-empty]': empty, '[data-mail-rule-order-list]': list, '[data-mail-rules-filters]': filters})[selector] || null,
  querySelectorAll: selector => selector === '[data-folder-picker-account]' ? [list] : selector === '[data-mail-rule-row]' ? rows : [],
}
const document = {
  addEventListener(name, listener) { (listeners[name] = listeners[name] || []).push(listener) },
  getElementById: id => id === 'mail-rules-panel' ? panel : null,
  querySelector: selector => selector === '#mail-rules-panel [data-mail-rules-search]' ? search : null,
  querySelectorAll: selector => selector === '#mail-rules-panel [data-mail-rule-row]' ? rows : [],
}
// The page's address, which keeps the folder the list shows the rules of.
window.location = {pathname: '/settings/rules', search: '?folder=f-receipts', hash: ''}
const history = {state: null, replaceState(state, title, url) { const [pathname, query] = url.split('?'); window.location = {pathname, search: query ? '?' + query : '', hash: ''} }}
const context = vm.createContext({window, document, history, URLSearchParams})
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

// Opened from a folder's menu, the list shows only the rules that move mail
// into that folder, and the search looks among them.
rows = [rule('r1', 'shop receipts', ['f-receipts'], ['Finance']), rule('r2', 'weekly digest', ['f-archive', 'f-receipts']), rule('r3', 'travel plans', ['f-trips'], ['Travel', 'finance']), rule('r4', 'invoices', [], ['Bills'])]
setFilters('f-receipts', [])
context.searchMailRulesAgain({loaded: false, rules: []})
assert.equal(shown(), 'r1,r2', 'only the folder\'s rules show, one moving mail to several folders too')
assert.ok('data-searching' in list.attrs, 'rules are not dragged while some are hidden')
type('digest')
assert.equal(shown(), 'r2', 'the search looks among the folder\'s rules')
type('travel')
assert.equal(shown(), '', 'and finds none of the other rules')
assert.equal(empty.hidden, false, 'no match says so')
type('')
assert.equal(shown(), 'r1,r2')
context.searchMailRulesAgain({loaded: true, rules: ['r1', 'r2', 'r3', 'r4']})
assert.equal(shown(), 'r1,r2', 'a list drawn again keeps the folder')
assert.equal(window.location.search, '?folder=f-receipts')

// Categories show the rules in any of them, whatever their capitals, and
// with a folder too, only that folder's.
setFilters('', ['Finance', 'Bills'])
context.filterMailRules()
assert.equal(shown(), 'r1,r3,r4', 'rules in any of the categories show')
assert.equal(window.location.search, '', 'a list showing every folder\'s rules leaves the folder out of the address')
setFilters('f-receipts', ['Finance'])
context.filterMailRules()
assert.equal(shown(), 'r1', 'with a folder, only its rules in the categories')
assert.equal(window.location.search, '?folder=f-receipts', 'and the address names the folder again')
setFilters('', [])
context.filterMailRules()
assert.equal(shown(), 'r1,r2,r3,r4', 'no filters show every rule')
assert.ok(!('data-searching' in list.attrs), 'and rules can be dragged again')

// A pill turns its filter off, Clear turns them all off, and a rule's
// category adds it: each asks for the list with the filters changed.
const clickOn = (attribute, value) => {
  for (const listener of listeners.click) listener({target: {closest: selector => selector === '[' + attribute + ']' ? {getAttribute: name => name === attribute ? value : null} : null}})
}
// Made in the script's own context; compared as plain values.
const asked = () => JSON.parse(JSON.stringify(requests.pop().values))
setFilters('f-receipts', ['Finance'])
clickOn('data-mail-rules-filter-remove', 'folder')
assert.deepEqual(asked(), {filter_set: '1', filter_folder: '', filter_tag: 'Finance'}, 'the folder\'s pill keeps the categories')
clickOn('data-mail-rules-filter-remove', 'tags')
assert.deepEqual(asked(), {filter_set: '1', filter_folder: 'f-receipts', filter_tag: ''}, 'the categories\' pill keeps the folder')
clickOn('data-mail-rules-filters-clear', '')
assert.deepEqual(asked(), {filter_set: '1', filter_folder: '', filter_tag: ''})
clickOn('data-mail-rule-category-filter', 'Bills')
assert.deepEqual(asked(), {filter_set: '1', filter_folder: 'f-receipts', filter_tag: 'Finance,Bills'}, 'a rule\'s category is added')
clickOn('data-mail-rule-category-filter', 'Finance')
assert.equal(requests.length, 0, 'a category already chosen asks nothing')

// Categories picked in the Filters menu apply once their list closes, and
// only when they changed.
const closeCategories = value => {
  const form = {querySelector: selector => selector === '[data-mail-rules-filter-tags]' ? {value} : null}
  for (const listener of listeners.toggle) listener({newState: 'closed', target: {matches: selector => selector === '[data-tui-selectbox-content]', closest: selector => selector === '[data-mail-rules-filter-form]' ? form : null}})
}
closeCategories('Finance,Travel')
assert.deepEqual(asked(), {filter_set: '1', filter_folder: 'f-receipts', filter_tag: 'Finance,Travel'})
closeCategories('Finance')
assert.equal(requests.length, 0, 'the same categories ask nothing')
console.log('Rules search: name, condition, folder, empty, redraw, folder and category filters passed.')
