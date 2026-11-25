// An open thread in the mail list is as tall as its row, its messages and
// the gap before the next row, whatever the guess was. The shell is drawn at
// the guessed height and scrollHeight never reads less than that, so a guess
// that was too tall stayed, and the space under an open thread grew with
// every message in it.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const subStyle = {borderTopWidth: '0px', borderBottomWidth: '2px', marginTop: '4px', marginBottom: '0px'}
const window = {
  location: {pathname: '/folder/inbox', search: ''},
  addEventListener() {},
  getComputedStyle: () => subStyle,
}
const document = {getElementById() { return null }, querySelector() { return null }, addEventListener() {}}
const context = vm.createContext({window, document, URLSearchParams, Map, Set, Promise, setTimeout() {}, requestAnimationFrame() {}})
vm.runInContext(fs.readFileSync(path.join(__dirname, '../assets/js/virtual-scroll.js'), 'utf8'), context)

function openThread(viewMode) {
  const l = Object.create(window.VirtualMailList.prototype)
  l.viewMode = viewMode
  l.expandedThreadGap = 15
  l.expandedThreads = new Map([['t', {subCount: 60, measuredHeight: 0}]])
  // Drawn at a guess of 3008px: 60 messages of 48px, each really 46.375px.
  const main = {getBoundingClientRect: () => ({height: 113})}
  const sub = {scrollHeight: 2789}
  const shell = {
    scrollHeight: 3008,
    firstElementChild: main,
    querySelector: selector => (selector === ':scope > .thread-sub-items' ? sub : null),
  }
  l.updateExpandedThreadMeasuredHeight('t', shell)
  return l.expandedThreads.get('t').measuredHeight
}

assert.equal(openThread('cards'), 113 + 4 + 2789 + 2 + 15, 'cards: row, messages and the gap, not the guess')
assert.equal(openThread('table'), 113 + 4 + 2789 + 2, 'table rows have no gap under a thread')

console.log('List thread height tests passed')
