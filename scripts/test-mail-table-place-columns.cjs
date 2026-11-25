const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// A folder or tag can keep table columns of its own ("Only for this
// folder"); every other place shows the list's columns.
const source = fs.readFileSync(path.join(__dirname, '../assets/js/ui-settings.js'), 'utf8')

const styles = {}
const scroll = {
  id: 'mail-list-scroll',
  dataset: {folderId: 'inbox'},
  style: {setProperty(name, value) { styles[name] = value }},
  querySelector() { return null },
}
const saved = []
const context = {
  location: {search: ''},
  URLSearchParams,
  localStorage: {getItem() { return null }, setItem() {}},
  CustomEvent: function (name, options) { this.detail = options && options.detail },
  fetch(url, options) {
    if (options && options.method === 'PATCH') saved.push(JSON.parse(options.body))
    return Promise.resolve({ok: true, json: () => ({}), text: () => ''})
  },
  setTimeout(callback) { callback(); return 0 },
  clearTimeout() {},
  document: {
    body: {dataset: {uiSettings: JSON.stringify({mail_table_columns: 'from,subject,date', timezone: 'UTC'})}, dispatchEvent() {}},
    documentElement: {style: {setProperty() {}}, setAttribute() {}, dataset: {}, classList: {toggle() {}, add() {}, remove() {}}},
    getElementById(id) { return id === 'mail-list-scroll' ? scroll : null },
    querySelector() { return null },
    querySelectorAll() { return [] },
  },
}
context.window = context
vm.runInNewContext(source + '\nwindow.GoferSettings = GoferSettings', context)
const table = context.GoferListLayouts.mail.table

function open(folderID, tag) {
  scroll.dataset.folderId = folderID
  scroll.dataset.tableColumnsPlace = context.mailTableColumnsPlace(folderID, tag)
  table.apply()
}
const shown = () => scroll.dataset.mailTableColumns

// Before the list sets its place, the folder comes from the page.
delete scroll.dataset.tableColumnsPlace
scroll.dataset.folderId = 'sent'
assert.equal(table.hasPlace(), true)

open('inbox')
assert.equal(shown(), 'from,subject,date')

// Sent starts from the list's columns, then keeps its own.
open('sent')
assert.equal(table.hasOwn(), false)
table.setOwn(true)
assert.equal(table.hasOwn(), true)
assert.equal(shown(), 'from,subject,date')
table.saveColumns(['to', 'subject', 'date'])
table.saveWidths('1,1,1,1,1,9,5,2')
assert.equal(shown(), 'to,subject,date')
assert.equal(context.GoferSettings.get('mail_table_columns'), 'from,subject,date', 'Sent leaves the list columns alone')

// Dragged widths show before they are saved, without saving them.
table.applyWidths('1,1,1,1,1,1,1,1')
assert.equal(table.widths(), '1,1,1,1,1,9,5,2')

open('inbox')
assert.equal(shown(), 'from,subject,date')
assert.equal(table.hasOwn(), false)

// An account folder and a tag are places of their own too.
open('acct_f_0123')
assert.equal(table.hasPlace(), true)
assert.equal(shown(), 'from,subject,date')
open('inbox', {label: 'Receipts', accountId: 'acct'})
assert.equal(scroll.dataset.tableColumnsPlace, 'tag:acct:Receipts')

// Search results are no place, so they show the list's columns.
open('search')
assert.equal(table.hasPlace(), false)
table.setOwn(true)
assert.equal(table.hasOwn(), false)
assert.equal(shown(), 'from,subject,date')

// A change made where the place has no columns of its own reaches the list.
open('inbox')
table.saveColumns(['from', 'date'])
open('sent')
assert.equal(shown(), 'to,subject,date')
open('acct_f_0123')
assert.equal(shown(), 'from,date')

// Turning it off brings Sent back to the list's columns.
open('sent')
table.setOwn(false)
assert.equal(shown(), 'from,date')
assert.deepEqual(JSON.parse(context.GoferSettings.get('mail_table_place_columns')), {})
assert(saved.length > 0 && 'mail_table_place_columns' in saved[saved.length - 1], 'The places are saved with the other settings')

console.log('Mail table columns per folder: own columns and widths, tags, search, and turning it off passed.')
