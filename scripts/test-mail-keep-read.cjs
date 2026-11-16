const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// A message read in the Unread list stays listed: the list tells the server
// which read messages to keep (keep=) until its filters change.
const window = {location: {pathname: '/folder/inbox', search: '?unread=1'}, addEventListener() {}}
const document = {getElementById() { return null }, querySelector() { return null }, addEventListener() {}}
const context = vm.createContext({window, document, URLSearchParams, Map, Set, Promise, setTimeout() {}})
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/virtual-scroll.js'), 'utf8'), context)

function list(unread) {
  const l = Object.create(window.VirtualMailList.prototype)
  l.filters = l.emptyFilters()
  l.filters.unread = unread
  l.sidebarTag = l.emptySidebarTag()
  l.folderID = 'inbox'
  return l
}
const keep = l => new URL(l.withFilterParams('/mail/folder/inbox/items?start=0&limit=1'), 'http://localhost').searchParams.get('keep')

const unread = list(true)
assert.equal(keep(unread), null, 'nothing is kept before a message is read')
unread.keepRead('42')
unread.keepRead(7)
unread.keepRead('42')
unread.keepRead('draft-1')
assert.equal(keep(unread), '42,7', 'read messages are kept once each, message IDs only')
for (let id = 100; id < 400; id++) unread.keepRead(id)
const kept = keep(unread).split(',')
assert.equal(kept.length, 200, 'the server is sent at most 200')
assert.equal(kept[kept.length - 1], '399', 'the latest ones')

const all = list(false)
all.keepRead('42')
assert.equal(keep(all), null, 'lists without the Unread filter keep nothing')
unread.filters.unread = false
assert.equal(keep(unread), null, 'keep is sent only with the Unread filter')

console.log('Mail keep-read tests passed')
