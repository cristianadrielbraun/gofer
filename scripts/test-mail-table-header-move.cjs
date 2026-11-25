const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// Moving to a folder with columns of its own moves the column names to
// their new places instead of jumping: a name in both slides and stretches,
// a name gone fades out where it was, and a new one fades in.

// Where each column name sits in each folder's columns.
const layouts = {
  inbox: {from: {left: 40, width: 120}, subject: {left: 170, width: 300}, date: {left: 480, width: 80}},
  sent: {to: {left: 40, width: 200}, subject: {left: 250, width: 220}, date: {left: 480, width: 80}},
}
let layout = layouts.inbox
let animations = []

function element(id) {
  return {
    id,
    dataset: {mailTableColumnId: id},
    style: {},
    attributes: {},
    getBoundingClientRect() {
      const place = layout[this.dataset.mailTableColumnId]
      return place ? {left: place.left, top: 6, width: place.width, height: 20} : {left: 0, top: 0, width: 0, height: 0}
    },
    animate(keyframes, options) {
      const animation = {element: this, keyframes, options, cancelled: false, cancel() { this.cancelled = true }, finished: new Promise(() => {})}
      animations.push(animation)
      return animation
    },
    cloneNode() {
      const clone = element(id)
      clone.getBoundingClientRect = () => ({left: 0, top: 0, width: 0, height: 0})
      return clone
    },
    setAttribute(name, value) { this.attributes[name] = value },
    removeAttribute(name) { if (name === 'data-mail-table-column-id') delete this.dataset.mailTableColumnId },
    querySelectorAll() { return [] },
    remove() { header.leaving = header.leaving.filter(clone => clone !== this) },
  }
}

const cells = ['from', 'to', 'subject', 'date'].map(element)
const header = {
  leaving: [],
  isConnected: true,
  animate() {},
  getBoundingClientRect() { return {left: 0, top: 0, width: 600, height: 32} },
  querySelectorAll(selector) {
    if (selector.includes('data-mail-table-header-leaving')) return this.leaving
    return cells
  },
  appendChild(clone) { this.leaving.push(clone) },
}
const columns = {inbox: '1fr 2fr 80px', sent: '2fr 1fr 80px'}
const container = {
  dataset: {},
  style: {
    value: columns.inbox,
    getPropertyValue() { return this.value },
  },
  querySelector(selector) { return selector === '.mail-list-table-header' ? header : null },
}

const window = {
  location: {pathname: '/folder/inbox', search: ''},
  addEventListener() {},
  matchMedia() { return {matches: false} },
  mailTableColumnsPlace(folderID) { return folderID === 'search' ? '' : folderID },
  applyMailTableColumnSettings() {
    const place = container.dataset.tableColumnsPlace
    layout = layouts[place] || layouts.inbox
    container.style.value = columns[place] || columns.inbox
  },
}
const document = {getElementById() { return null }, querySelector() { return null }, addEventListener() {}}
const context = vm.createContext({
  window, document, URLSearchParams, Map, Set, Promise, setTimeout() {},
  getComputedStyle() { return {display: 'block'} },
})
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/virtual-scroll.js'), 'utf8'), context)

const list = Object.create(window.VirtualMailList.prototype)
list.container = container
list.viewMode = 'table'
list.sidebarTag = list.emptySidebarTag()
list.searchScope = null

function open(folderID) {
  animations = []
  list.folderID = folderID
  list.syncTableColumnsPlace()
}
const of = id => animations.filter(animation => animation.element.id === id)
// Keyframes are made in the script's own context, so compare them as JSON.
const plain = value => JSON.parse(JSON.stringify(value))

// The first place is set before anything is drawn: nothing moves.
open('inbox')
assert.equal(animations.length, 0)

open('sent')
const subject = of('subject')
assert.equal(subject.length, 1, 'Subject is in both folders, so it moves')
assert.deepEqual(plain(subject[0].keyframes), [
  {translate: '-80px 0', width: '300px'},
  {translate: '0 0', width: '220px'},
], 'Subject starts where it was, as wide as it was')
assert.equal(subject[0].options.duration, 300)
assert.equal(of('date').length, 0, 'Date did not move, so it is left alone')

const to = of('to')
assert.equal(to.length, 1)
assert.equal(to[0].keyframes[0].opacity, 0, 'To is new in Sent, so it fades in')
assert.equal(to[0].options.delay, 60, 'with the rows arriving below it')

assert.equal(header.leaving.length, 1, 'From is gone, so a copy fades out where it was')
const from = header.leaving[0]
assert.equal(from.style.left, '40px')
assert.equal(from.style.width, '120px')
assert.equal(from.dataset.mailTableColumnId, undefined, 'the copy is not a column the resize handles find')
assert.deepEqual(plain(of('from')[0].keyframes), [{opacity: 1}, {opacity: 0}])

// Moving again before the last move ends cancels it and starts from where
// things are.
const running = animations.slice()
open('inbox')
assert(running.every(animation => animation.cancelled), 'the previous move is cancelled')
assert.equal(header.leaving.length, 1, "the earlier fading copy is replaced by To's")
assert.equal(header.leaving[0].id, 'to')

// Two places with the same columns: nothing to move.
columns.archive = columns.inbox
layouts.archive = layouts.inbox
open('archive')
assert.equal(animations.length, 0)

// Reduced motion: the columns change at once.
window.matchMedia = () => ({matches: true})
open('sent')
assert.equal(animations.length, 0)
assert.equal(container.dataset.tableColumnsPlace, 'sent')

// The rows fading out keep the columns they were drawn with, though the
// list changes them under them.
container.style.value = columns.inbox
const overlayVars = {}
const overlay = {style: {setProperty(name, value) { overlayVars[name] = value }}}
const cell = name => ({dataset: {mailTableCell: name}, style: {}})
const rows = [[cell('from'), cell('subject')], [cell('from'), cell('subject')]]
const shown = {from: 'flex', subject: 'block'}
context.getComputedStyle = element => ({display: shown[element.dataset.mailTableCell]})
list.keepTableColumns(overlay, rows.map(cellsOfRow => ({querySelectorAll() { return cellsOfRow }})))
assert.equal(overlayVars['--mail-list-table-columns'], columns.inbox)
assert(rows.every(row => row[0].style.display === 'flex' && row[1].style.display === 'block'), 'each cell keeps showing as it did')

console.log('Mail table header: names slide, fade in and out between folders, moves restart cleanly, reduced motion, and leaving rows keeping their columns passed.')
