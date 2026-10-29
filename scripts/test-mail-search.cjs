const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const urls = []
const events = {}, timers = [], requests = []
let ownership = 0
const cleared = []
// Clearing resolves at once here, so a request it waits for is visible right away.
const controller = {searchScope: {kind: 'all'}, indexById: new Map(), cache: new Map(),
  clearForTransition() { cleared.push(requests.length); return {then(next) { next(); return this }} }}
const location = {pathname: '/mail/search', search: '?search_from=projects&q=invoice'}
const history = {state: {folder: 'search'}, pushState(state, _, url) { this.state = state; urls.push(url) },
  replaceState(state, _, url) { this.state = state; urls.push(url) }}
const document = {getElementById(id) { return id === 'mail-list-scroll' ? {_virtualMailList: controller} : null },
  querySelector() { return null }, addEventListener() {}}
const window = {location, addEventListener(name, listener) { (events[name] ||= []).push(listener) }}
const swaps = []
const context = vm.createContext({window, document, history, URLSearchParams, Map, Set, Promise,
  setTimeout(callback) { timers.push(callback) },
  fetch(url) { requests.push({url, ownership}); return Promise.resolve({ok: true, text() { return Promise.resolve('<div id="main-content"></div>') }}) },
  htmx: {ajax(method, url) { requests.push({url, ownership}) }, swap(target, html) { swaps.push(target) }}})
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/mail-search.js'), 'utf8'), context)
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/virtual-scroll.js'), 'utf8'), context)
const search = window.GoferMailSearch

const folders = {kind: 'folders', folder_ids: ['Projects/2026 & 2027', 'work:finance'], include_descendants: true, exclude_spam_trash: true}
const encoded = new URL(search.scopeURL('/mail/search?q=invoice&scope=account&scope_account_id=old', folders), 'http://localhost')
assert.equal(encoded.searchParams.get('q'), 'invoice')
assert.equal(encoded.searchParams.get('scope_account_id'), null)
assert.deepEqual(encoded.searchParams.getAll('scope_folder_id'), folders.folder_ids)
assert.equal(encoded.searchParams.get('include_descendants'), '1')
assert.equal(encoded.searchParams.get('exclude_spam_trash'), '1')
assert.equal(new URL(search.scopeURL(encoded.pathname + encoded.search, null), 'http://localhost').searchParams.has('scope'), false)

search.remember([{id: '12', folder_id: 'work:finance'}])
const request = {targets: [{id: '12', thread: true}], folder_id: 'search', source_folder_id: 'search'}
const scoped = search.payload(request)
assert.equal(scoped.search_scope.kind, 'all')
assert.equal(scoped.targets[0].folder_id, 'work:finance')
assert.equal(scoped.folder_id, undefined)
assert.equal(scoped.source_folder_id, undefined)
assert.equal(request.folder_id, 'search', 'payload must not mutate caller state')
assert.equal(search.payload({targets: request.targets, folder_id: 'destination'}).folder_id, 'destination')
assert.equal(search.payload({targets: request.targets, search_scope: folders}).search_scope, folders, 'queued action keeps its original scope')
assert.equal(search.payload({targets: request.targets, search_scope: null}).search_scope, null, 'ordinary queued action must not adopt a later search scope')

const list = Object.create(window.VirtualMailList.prototype)
list.filters = list.emptyFilters()
list.filters.query = 'invoice'
list.sidebarTag = list.emptySidebarTag()
list.searchScope = folders
list.folderID = 'search'
list.selectedEmailId = '12'
list.totalCount = 42
for (const endpoint of ['/mail/folder/search/items?start=50&limit=50', '/mail/folder/search/ids']) {
  const url = new URL(list.withFilterParams(endpoint), 'http://localhost')
  assert.ok(url.pathname.startsWith('/mail/search/'))
  assert.equal(url.searchParams.get('q'), 'invoice')
  assert.deepEqual(url.searchParams.getAll('scope_folder_id'), folders.folder_ids)
}
list.pushUrl()
let url = new URL(urls.at(-1), 'http://localhost')
assert.equal(url.pathname, '/mail/search')
assert.equal(url.searchParams.get('selected'), '12')
assert.equal(url.searchParams.get('search_from'), 'projects')
assert.deepEqual(url.searchParams.getAll('scope_folder_id'), folders.folder_ids)
list.replaceUrl()
assert.equal(new URL(urls.at(-1), 'http://localhost').searchParams.get('selected'), '12')
list.searchScope = null
list.folderID = 'inbox'
assert.equal(new URL(list.withFilterParams('/mail/folder/inbox/items'), 'http://localhost').pathname, '/mail/folder/inbox/items')
list.pushUrl()
assert.equal(new URL(urls.at(-1), 'http://localhost').pathname, '/folder/inbox/12')

// History navigation must start after later popstate listeners update request
// ownership. A same-event request can otherwise reject its own response.
events.popstate[0]({state: {folder: 'search'}})
assert.equal(requests.length, 0)
ownership++
timers.shift()()
assert.equal(requests[0].ownership, 1)
assert.equal(new URL(requests[0].url, 'http://localhost').pathname, '/mail/search')
assert.deepEqual(cleared, [0], 'the old list starts clearing as the new one is requested')
// Sync events refresh an open search when their folder is in its scope.
const pickerFolders = [['projects', ''], ['client', 'projects'], ['invoices', 'client'], ['elsewhere', ''], ['trash', '']]
  .map(([value, parentId]) => ({value, dataset: {parentId}}))
const picker = {querySelectorAll(selector) { return selector === '[data-mail-scope-folder]' ? pickerFolders : [] }}
const getElementById = document.getElementById
document.getElementById = id => id === 'mail-search-folders-dialog' ? picker : getElementById(id)
const event = (folder_id, account_id = 'a', folder_role = 'custom') => ({folder_id, account_id, folder_role})
for (const [scope, touched, untouched] of [
  [{kind: 'all'}, [event('elsewhere', 'b'), event('trash', 'a', 'trash')], []],
  [{kind: 'all', exclude_spam_trash: true}, [event('elsewhere')], [event('trash', 'a', 'trash'), event('spam', 'a', 'junk')]],
  [{kind: 'account', account_id: 'a'}, [event('elsewhere')], [event('elsewhere', 'b')]],
  [{kind: 'folders', folder_ids: ['projects']}, [event('projects')], [event('client'), event('elsewhere')]],
  [{kind: 'folders', folder_ids: ['projects'], include_descendants: true}, [event('projects'), event('invoices')], [event('elsewhere')]],
]) {
  controller.searchScope = scope
  for (const data of touched) assert.equal(search.touches(data), true, JSON.stringify(scope) + ' reads ' + data.folder_id)
  for (const data of untouched) assert.equal(search.touches(data), false, JSON.stringify(scope) + ' does not read ' + data.folder_id)
}
controller.searchScope = null
assert.equal(search.touches(event('projects')), false, 'a folder list matches its own events')
document.getElementById = getElementById
controller.searchScope = {kind: 'all'}

// The new list replaces the old one only once both the request and the
// old list's exit are done.
Promise.resolve().then(() => new Promise(resolve => setImmediate(resolve))).then(() => {
  assert.deepEqual(swaps, ['#main-content'])
console.log('Mail search: scope URLs, folder encoding, paging requests, action context, captured scopes, selection history, ordinary folders, list replacement, and sync refreshes passed.')
})
