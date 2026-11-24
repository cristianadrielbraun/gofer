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
const rule = (id, searchText, folders = []) => ({
  hidden: false, dataset: {searchText}, attrs: {},
  querySelector: () => null,
  hasAttribute(name) { return name in this.attrs },
  toggleAttribute(name, on) { if (on) this.attrs[name] = ''; else delete this.attrs[name] },
  getAttribute: name => name === 'data-mail-rule-id' ? id : name === 'data-mail-rule-folders' ? JSON.stringify(folders) : null,
})
let rows = [rule('r1', 'shop receipts robin@example.test · move to finance / receipts', ['f-receipts']), rule('r2', 'weekly digest subject starts with lunch on friday?', ['f-archive', 'f-receipts']), rule('r3', 'travel plans move to trips', ['f-trips'])]
// The line saying the list shows one folder's rules, when it was opened
// for one.
let folderFilter = null
const showFolder = folder => { folderFilter = {getAttribute: name => name === 'data-mail-rules-folder-filter' ? folder : null, remove() { folderFilter = null }} }
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
  querySelector: selector => ({'[data-folder-picker-search]': search, '[data-mail-rules-search]': search, '[data-folder-picker-empty]': empty, '[data-mail-rule-order-list]': list, '[data-mail-rules-folder-filter]': folderFilter})[selector] || null,
  querySelectorAll: selector => selector === '[data-folder-picker-account]' ? [list] : selector === '[data-mail-rule-row]' ? rows : [],
}
const document = {
  addEventListener(name, listener) { (listeners[name] = listeners[name] || []).push(listener) },
  getElementById: id => id === 'mail-rules-panel' ? panel : null,
  querySelector: selector => selector === '#mail-rules-panel [data-mail-rules-search]' ? search : null,
  querySelectorAll: selector => selector === '#mail-rules-panel [data-mail-rule-row]' ? rows : [],
}
// The page's address, which the folder leaves when every rule shows again.
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
rows = [rule('r1', 'shop receipts', ['f-receipts']), rule('r2', 'weekly digest', ['f-archive', 'f-receipts']), rule('r3', 'travel plans', ['f-trips']), rule('r4', 'invoices')]
showFolder('f-receipts')
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

// The list drawn again keeps the folder.
context.searchMailRulesAgain({loaded: true, rules: ['r1', 'r2', 'r3', 'r4']})
assert.equal(shown(), 'r1,r2', 'a list drawn again keeps the folder')

// Show all shows every rule and takes the folder out of the address.
// Pressed from the keyboard (a click with no pointer: detail 0).
const showAll = detail => { for (const listener of listeners.click) listener({detail, target: {closest: selector => selector === '[data-mail-rules-folder-filter-clear]' ? {} : null}}) }
showAll(0)
assert.equal(folderFilter, null)
assert.equal(shown(), 'r1,r2,r3,r4', 'show all shows every rule')
assert.ok(!('data-searching' in list.attrs), 'and rules can be dragged again')
assert.equal(window.location.search, '', 'reloading shows every rule too')
assert.ok(search.focused, 'the search takes the focus from the button that went')

// A tap leaves the focus alone, so a phone's keyboard doesn't open.
search.focused = false
showFolder('f-receipts')
showAll(1)
assert.equal(shown(), 'r1,r2,r3,r4')
assert.ok(!search.focused, 'a tap doesn\'t focus the search')

// A rule just added shows even when it moves no mail to the folder.
showFolder('f-receipts')
context.filterMailRules()
rows = [...rows, rule('r5', 'lunch on friday?', ['f-trips'])]
context.searchMailRulesAgain({loaded: true, rules: ['r1', 'r2', 'r3', 'r4']})
assert.equal(folderFilter, null)
assert.equal(shown(), 'r1,r2,r3,r4,r5', 'adding a rule shows every rule')
console.log('Rules search: name, condition, folder, empty, redraw and one-folder searches passed.')
