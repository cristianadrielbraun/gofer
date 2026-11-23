// Closing a thread in the mail list: rows under it rise from below, and a
// pooled (hidden) row shell, which still holds the row it last showed, is
// never taken for a row on screen. Taking its empty box dropped the rows
// under a closed thread in from the top of the list.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const window = {location: {pathname: '/folder/inbox', search: ''}, addEventListener() {}, matchMedia: () => ({matches: false})}
const document = {getElementById() { return null }, querySelector() { return null }, addEventListener() {}}
const context = vm.createContext({window, document, URLSearchParams, Map, Set, Promise, setTimeout() {}})
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/virtual-scroll.js'), 'utf8'), context)

// shell is a pooled row shell showing the row id at top..top+height.
function shell(id, top, height, hidden) {
  const transforms = []
  const style = {transition: ''}
  Object.defineProperty(style, 'transform', {
    get() { return this._transform || '' },
    set(value) { this._transform = value; transforms.push(value) },
  })
  const row = {dataset: {emailId: id}, outerHTML: '<div></div>', querySelector: () => null}
  return {
    hidden: !!hidden,
    style,
    transforms,
    classList: {contains: () => false},
    querySelector: selector => (selector === '.mail-list-item' ? row : null),
    getBoundingClientRect: () => (hidden
      ? {top: 0, bottom: 0, left: 0, height: 0, width: 0}
      : {top, bottom: top + height, left: 0, height, width: 600}),
  }
}

function list(children) {
  const l = Object.create(window.VirtualMailList.prototype)
  l.itemsContainer = {children, offsetHeight: 0}
  l.container = {getBoundingClientRect: () => ({top: 0, bottom: 900})}
  return l
}

// A hidden shell still holding row "below" is left out of the layout.
{
  const l = list([shell('above', 100, 113), shell('below', 0, 0, true)])
  const layout = l.captureRenderedLayout()
  assert.deepEqual([...layout.keys()], ['above'], 'only rows on screen are measured')
  const snapshot = l.captureListTransition()
  assert.deepEqual([...snapshot.rows.keys()], ['above'], 'the list transition skips pooled shells too')
}

// Closing the thread "t" (3000px tall, now 113px): the row under it wasn't on
// screen, so it rises from the list's bottom edge to its place.
{
  const previous = new Map([['t', {top: 500, bottom: 3500, height: 3000}]])
  const below = shell('below', 613, 113)
  const thread = shell('t', 500, 113)
  const l = list([thread, below])
  l.animateLayoutShift(previous, 't')
  assert.equal(below.transforms[0], ' translateY(287px)', 'it starts at the bottom edge (900 - 613)')
  assert.equal(below.transforms[1], '', 'and slides to its place')
  assert.deepEqual(thread.transforms, [], 'the thread row itself stays put')

  // Without a thread, a row that wasn't on screen just appears.
  const other = shell('below', 613, 113)
  list([shell('t', 500, 113), other]).animateLayoutShift(previous)
  assert.deepEqual(other.transforms, [], 'rows not on screen before have nowhere to move from')
}

console.log('List thread collapse tests passed')
