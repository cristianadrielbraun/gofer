const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// The rules search in Settings → Rules uses the folder picker's filter
// (folder-picker.js) through filterMailRules in settings.js.
const settings = fs.readFileSync(path.join(__dirname, '../assets/js/settings.js'), 'utf8')
const start = settings.indexOf('function filterMailRules(')
const end = settings.indexOf('document.body.addEventListener("htmx:beforeSwap"', start)
assert(start >= 0 && end > start)

const listeners = {}
const window = {}
// Each rule shown is 58px below the one before, in the list 20px under the
// panel's top; a rule records how it was animated, and copies of it fade
// out in the list.
const rule = (id, searchText, folders = [], tags = [], more = {}) => ({
  hidden: false, dataset: {searchText}, attrs: {}, animations: [],
  querySelector: () => null,
  hasAttribute(name) { return name in this.attrs },
  toggleAttribute(name, on) { if (on) this.attrs[name] = ''; else delete this.attrs[name] },
  getAttribute: name => ({
    'data-mail-rule-id': id, 'data-mail-rule-folders': JSON.stringify(folders), 'data-mail-rule-tags': JSON.stringify(tags),
    'data-mail-rule-accounts': JSON.stringify(more.accounts || ['*']), 'data-mail-rule-on': String(more.on !== false), 'data-mail-rule-actions': JSON.stringify(more.actions || []),
  })[name] ?? null,
  getBoundingClientRect() { return {top: 120 + rows.filter(row => !row.hidden).indexOf(this) * 58, height: 50} },
  animate(keyframes, options) { this.animations.push({keyframes, options}); return {finished: Promise.resolve()} },
  cloneNode() {
    const copy = {of: id, hidden: true, style: {cssText: ''}, attributes: [], removed: false,
      setAttribute() {}, removeAttribute() {}, querySelectorAll: () => [],
      animate(keyframes) { copy.keyframes = keyframes; return {finished: Promise.resolve()} },
      remove() { copy.removed = true }}
    return copy
  },
})
let motion = false
let rows = [rule('r1', 'shop receipts robin@example.test · move to finance / receipts', ['f-receipts']), rule('r2', 'weekly digest subject starts with lunch on friday?', ['f-archive', 'f-receipts']), rule('r3', 'travel plans move to trips', ['f-trips'])]
// The list's filters as the server drew them: the folder whose rules it
// shows, as a folder's menu in the sidebar opens it, and its categories.
let filters = null
// The menu's fields, each a selectbox whose hidden input holds a filter;
// what the menu names each value; and the pills drawn last.
const fieldInput = attribute => ({value: '', attribute, hasAttribute: name => name === attribute, closest: selector => selector === '[data-mail-rules-filter-form]' ? {} : null, dispatchEvent() {}})
const inputs = {
  '[data-mail-rules-filter-tags]': fieldInput('data-mail-rules-filter-tags'),
  '[data-mail-rules-filter-accounts]': fieldInput('data-mail-rules-filter-accounts'),
  '[data-mail-rules-filter-status]': fieldInput('data-mail-rules-filter-status'),
  '[data-mail-rules-filter-actions]': fieldInput('data-mail-rules-filter-actions'),
}
const badge = {textContent: '', hidden: true}
const setFilters = (folder, tags, more = {}) => {
  const attrs = {
    'data-filter-folder': folder, 'data-filter-folder-pill': folder ? 'Moves mail to Receipts' : '', 'data-filter-tags': JSON.stringify(tags),
    'data-filter-accounts': JSON.stringify(more.accounts || []), 'data-filter-status': more.status || '', 'data-filter-actions': JSON.stringify(more.actions || []),
  }
  filters = {
    attrs,
    getAttribute: name => attrs[name] ?? null,
    setAttribute: (name, value) => { attrs[name] = String(value) },
    querySelector: selector => inputs[selector] || (selector === '[data-mail-rules-filter-count]' ? badge : null),
  }
}
const state = () => ({folder: filters.attrs['data-filter-folder'], tags: JSON.parse(filters.attrs['data-filter-tags']), accounts: JSON.parse(filters.attrs['data-filter-accounts']), status: filters.attrs['data-filter-status'], actions: JSON.parse(filters.attrs['data-filter-actions'])})
let pills = null
window.goferSyncFilterPills = (strip, template, drawn) => { pills = drawn.map(pill => pill.name + '=' + pill.text) }
const copies = []
const host = {getBoundingClientRect: () => ({top: 120}), appendChild(copy) { copies.push(copy) }}
const list = {
  hidden: false, attrs: {},
  querySelectorAll: selector => selector === '[data-folder-picker-row]' ? rows : [],
  querySelector: () => null,
  toggleAttribute(name, on) { if (on) this.attrs[name] = ''; else delete this.attrs[name] },
  hasAttribute(name) { return name in this.attrs },
}
const search = {value: '', focused: false, matches: selector => selector === '[data-mail-rules-search]', focus() { search.focused = true }}
const empty = {hidden: true, classList: {toggle(name, on) { empty.hidden = on }}}
const panel = {
  closest: () => null,
  getBoundingClientRect: () => ({top: 100}),
  querySelector: selector => ({'[data-folder-picker-search]': search, '[data-mail-rules-search]': search, '[data-folder-picker-empty]': empty, '[data-mail-rule-order-list]': list, '[data-mail-rules-filters]': filters, '[data-mail-rules-list-host]': host})[selector] || null,
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
const mailRuleOrder = {id: row => row.getAttribute('data-mail-rule-id'), selected: () => false}
const goferT = (key, values) => key + (values ? JSON.stringify(values) : '')
const goferTn = (key, n, values) => key + ':' + n + JSON.stringify(values)
const context = vm.createContext({window, document, history, URLSearchParams, mailRuleStill: () => !motion, mailRuleOrder, goferT, goferTn, Event: class { constructor(type) { this.type = type } }})
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
// category adds it: each changes the list's filters in the page, and the
// pills and the Filters button's count follow.
const clickOn = (attribute, value) => {
  for (const listener of listeners.click) listener({target: {closest: selector => selector === '[' + attribute + ']' ? {getAttribute: name => name === attribute ? value : null, classList: {add() {}}} : null}})
}
setFilters('f-receipts', ['Finance'])
clickOn('data-mail-rules-filter-remove', 'folder')
assert.deepEqual(state(), {folder: '', tags: ['Finance'], accounts: [], status: '', actions: []}, 'the folder\'s pill keeps the categories')
assert.equal(shown(), 'r1,r3', 'and the list shows the categories\' rules from every folder')
assert.equal(pills.join('|'), 'tags=js.settings.rules.filter_categories:1{"categories":"Finance"}', 'one pill is left')
assert.equal(badge.textContent, '1')
assert.equal(window.location.search, '', 'the folder leaves the address')
clickOn('data-mail-rule-category-filter', 'Bills')
assert.deepEqual(state().tags, ['Finance', 'Bills'], 'a rule\'s category is added')
assert.equal(shown(), 'r1,r3,r4')
clickOn('data-mail-rule-category-filter', 'Finance')
assert.deepEqual(state().tags, ['Finance', 'Bills'], 'a category already chosen changes nothing')
assert.equal(inputs['[data-mail-rules-filter-tags]'].value, 'Finance,Bills', 'the menu shows what the list shows')
clickOn('data-mail-rules-filters-clear', '')
assert.deepEqual(state(), {folder: '', tags: [], accounts: [], status: '', actions: []})
assert.equal(shown(), 'r1,r2,r3,r4')
assert.equal(pills.length, 0, 'no pills are left')
assert.ok(badge.hidden, 'and the count hides')

// A pick in the Filters menu applies at once.
const pick = (attribute, value) => {
  const input = inputs['[' + attribute + ']']
  input.value = value
  for (const listener of listeners.input) listener({target: input})
}
pick('data-mail-rules-filter-tags', 'Travel')
assert.deepEqual(state().tags, ['Travel'])
assert.equal(shown(), 'r3')
pick('data-mail-rules-filter-status', 'any')
assert.equal(state().status, '', 'any status is no filter')

// Accounts, on or off, and kinds of action.
rows = [
  rule('r1', 'old offers', [], [], {accounts: ['*'], actions: ['archive']}),
  rule('r2', 'receipts', [], [], {accounts: ['acc-work'], on: false, actions: ['move']}),
  rule('r3', 'spam words', [], [], {accounts: ['acc-home'], actions: ['trash', 'star']}),
  rule('r4', 'newsletters', [], [], {accounts: ['acc-work', 'acc-home'], actions: ['mark_read']}),
]
const filtered = more => { setFilters('', [], more); context.filterMailRules(); return shown() }
assert.equal(filtered({accounts: ['acc-work']}), 'r1,r2,r4', 'an account shows its rules and those for every account')
assert.equal(filtered({status: 'off'}), 'r2', 'off shows the rules turned off')
assert.equal(filtered({status: 'on'}), 'r1,r3,r4', 'on, the rules turned on')
assert.equal(filtered({actions: ['trash', 'archive']}), 'r1,r3', 'actions show the rules doing any of them')
assert.equal(filtered({accounts: ['acc-home'], status: 'on', actions: ['star', 'mark_read']}), 'r3,r4', 'filters together show what passes them all')
assert.ok('data-searching' in list.attrs, 'and rules are not dragged meanwhile')
pick('data-mail-rules-filter-status', 'off')
assert.equal(shown(), '', 'picking a status applies at once, even when nothing is left')
assert.equal(empty.hidden, false, 'which says so')
clickOn('data-mail-rules-filter-remove', 'status')
assert.deepEqual(state(), {folder: '', tags: [], accounts: ['acc-home'], status: '', actions: ['star', 'mark_read']}, 'a pill turns off only its own filter')
assert.equal(inputs['[data-mail-rules-filter-status]'].value, 'any', 'and the menu says so')
clickOn('data-mail-rules-filters-clear', '')
assert.equal(shown(), 'r1,r2,r3,r4')

// With motion, rules that stay slide from where they were, rules that go
// fade out on copies where they were, and rules that come fade in.
const slide = row => row.animations.map(a => a.keyframes[0].translate).join(' ')
rows = [rule('r1', 'shop receipts', ['f-receipts'], ['Finance']), rule('r2', 'weekly digest', ['f-archive', 'f-receipts']), rule('r3', 'travel plans', ['f-trips'], ['Travel', 'finance']), rule('r4', 'invoices', [], ['Bills'])]
motion = true
setFilters('', [])
context.filterMailRules()
rows.forEach(row => { row.animations = [] })
setFilters('', ['Finance'])
context.filterMailRules()
assert.equal(shown(), 'r1,r3')
assert.equal(slide(rows[0]), '', 'the first rule stays where it was')
assert.equal(slide(rows[2]), '0 58px', 'the third slides up from the place it had')
assert.deepEqual(copies.map(copy => copy.of + '@' + /top:(\d+)px/.exec(copy.style.cssText)[1]), ['r2@58', 'r4@174'], 'the rules going leave copies where they were')
assert.ok(copies.every(copy => !copy.hidden && copy.keyframes[copy.keyframes.length - 1].opacity === 0), 'which show and fade out')
rows.forEach(row => { row.animations = [] })
copies.length = 0
setFilters('', [])
context.filterMailRules()
assert.equal(slide(rows[2]), '0 -58px', 'back to every rule, the third slides down')
assert.equal(slide(rows[1]), '0 -4px', 'and the second comes in, dropping into place')
assert.equal(rows[1].animations[0].keyframes[0].opacity, 0)
assert.equal(copies.length, 0, 'nothing leaves')

// When no rule is left, those going still leave copies, and when they come
// back they move in.
rows.forEach(row => { row.animations = [] })
copies.length = 0
setFilters('', ['Nothing like it'])
context.filterMailRules()
assert.equal(shown(), '')
assert.equal(copies.length, 4, 'every rule going leaves a copy')
setFilters('', [])
context.filterMailRules()
assert.ok(rows.every(row => slide(row) === '0 -4px'), 'every rule coming back moves in')

// A list drawn again for some other change keeps its rules where it draws them.
rows.forEach(row => { row.animations = [] })
copies.length = 0
setFilters('', ['Finance'])
context.searchMailRulesAgain({loaded: true, rules: ['r1', 'r2', 'r3', 'r4'], places: null})
assert.ok(rows.every(row => row.animations.length === 0) && copies.length === 0, 'nothing moves')
motion = false
console.log('Rules search: name, condition, folder, empty, redraw, folder and category filters, and rules moving into place passed.')
