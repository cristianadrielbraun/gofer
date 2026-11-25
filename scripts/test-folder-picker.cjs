const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const window = {}
const listeners = {}
const document = {addEventListener(name, listener, capture) { listeners[name] = {listener, capture} }}
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/folder-picker.js'), 'utf8'), vm.createContext({window, document}))
const picker = window.goferFolderPicker

const row = searchText => ({hidden: false, dataset: {searchText}, attrs: {}, hasAttribute(name) { return name in this.attrs }})
const account = (rows, keep) => ({hidden: false, rows,
  querySelectorAll(selector) { return selector === '[data-folder-picker-row]' ? rows : [] },
  querySelector(selector) { return selector === '[data-folder-picker-keep]' && keep ? {} : null }})
const work = account([row('robin@example.test inbox'), row('robin@example.test inbox / trips'), row('robin@example.test inbox / trips / lisbon')])
const local = account([row('local folders archive')], true)
const other = account([row('sam@example.test notes')])
const search = {value: ''}
const empty = {hidden: true, classList: {toggle(name, on) { empty.hidden = on }}}
const box = {open: true, offsetWidth: 512, style: {width: ''}}
const root = {
  closest() { return null },
  querySelector(selector) { return selector === '[data-folder-picker-search]' ? search : selector === '[data-folder-picker-empty]' ? empty : selector === '[data-tui-dialog-content]' ? box : null },
  querySelectorAll(selector) { return selector === '[data-folder-picker-account]' ? [work, local, other] : [] },
}
const shown = list => list.rows.filter(r => !r.hidden).length

search.value = '  Trips '
assert.equal(picker.filter(root), 'Trips', 'returns the text typed, trimmed')
assert.equal(shown(work), 2, 'a parent name finds the folders inside it')
assert.equal(work.hidden, false)
assert.equal(local.hidden, false, 'an account with a row that stays is kept')
assert.equal(other.hidden, true, 'an account with no match is hidden')
assert.equal(empty.hidden, true)
assert.equal(box.style.width, '512px', 'an open dialog keeps its width while searching')
box.offsetWidth = 300
picker.filter(root)
assert.equal(box.style.width, '512px', 'and keeps it as folders hide')

search.value = 'nothing like it'
picker.filter(root)
assert.equal(empty.hidden, false, 'no match says so')

search.value = 'sam@'
picker.filter(root)
assert.equal(shown(other), 1, 'the account matches too')

search.value = ''
picker.filter(root)
assert.equal(shown(work) + shown(local) + shown(other), 5, 'an empty search shows every folder')

// A row another filter left out stays hidden, with or without a search.
other.rows[0].attrs['data-folder-picker-out'] = ''
picker.filter(root)
assert.equal(shown(other), 0, 'a row left out stays hidden with no search')
assert.equal(other.hidden, true, 'and its account with it')
search.value = 'sam@'
picker.filter(root)
assert.equal(shown(other), 0, 'and with a search it matches')
assert.equal(empty.hidden, false)
console.log('Folder picker: path, account, empty and left-out searches passed.')
