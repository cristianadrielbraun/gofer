const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/resize.js'), 'utf8')
function helper(name) {
  const start = source.indexOf('  function ' + name + '(')
  const end = source.indexOf('\n  }', start)
  assert(start >= 0 && end > start)
  return source.slice(start, end + 4)
}

// A response can arrive before the first rendering frame. Both original
// animations must already have a resolved, shared clock at that point.
const initial = []
const creation = vm.createContext({SLIDE_MS: 260, SLIDE_EASING: 'ease', trackSlide: animation => animation})
vm.runInContext(helper('slideKeyframes') + '\n' + helper('startPanelSlide'), creation)
for (const prop of ['width', 'height']) {
  const element = {animate() {
    const animation = {pending: true, _startTime: null,
      get startTime() { return this._startTime },
      set startTime(value) { this._startTime = value; this.pending = false },
    }
    initial.push(animation)
    return animation
  }}
  creation.startPanelSlide(element, prop, 400, 700, 0)
}
assert(initial.every(animation => animation.startTime === 0 && !animation.pending), 'Resolve all panel clocks before an early response can replace the list')

function handover(startTime, currentTime, playState = 'running') {
  const old = {startTime, currentTime, playState, cancelled: false, cancel() { this.cancelled = true }}
  const oldElement = {}
  const animations = []
  const list = {style: {}, dataset: {panelSection: 'contacts'}, animate(frames, options) {
    const animation = {pending: true, currentTime: 0, frames, options, _startTime: null,
      get startTime() { return this._startTime },
      set startTime(value) { this._startTime = value; this.pending = false },
    }
    animations.push(animation)
    return animation
  }}
  const context = vm.createContext({
    document: {getElementById: () => list},
    listPlaced: {el: oldElement, section: 'contacts', width: '70%', height: ''},
    listSlide: {el: oldElement, animation: old, prop: 'width', from: 420, to: 800},
    SLIDE_MS: 260, SLIDE_EASING: 'cubic-bezier(0.2, 0.8, 0.2, 1)', trackSlide: animation => animation,
  })
  vm.runInContext(helper('slideKeyframes') + '\n' + helper('continueListSlide'), context)
  context.continueListSlide()
  return {old, list, animations, context}
}

for (const startTime of [0, 500, 1200.25]) {
  const {old, list, animations, context} = handover(startTime, 95)
  assert.equal(list.style.width, '70%', 'The loaded pane retains its saved target width')
  assert.equal(animations.length, 1)
  assert.equal(animations[0].startTime, startTime, 'Replacement must share the original wall clock, including an origin of zero')
  assert.equal(animations[0].pending, false, 'Replacement must not wait for a new playback clock')
  assert.equal(animations[0].options.duration, 260, 'Keep the original duration and easing')
  assert.equal(animations[0].options.easing, 'cubic-bezier(0.2, 0.8, 0.2, 1)')
  assert.equal(old.cancelled, true)
  context.continueListSlide()
  assert.equal(animations.length, 1, 'Unrelated OOB swaps must not restart the slide')
}
const early = handover(null, 0)
assert.equal(early.animations[0].currentTime, 0, 'Before the first frame there is no timeline origin to inherit')
const completed = handover(500, 260, 'finished')
assert.equal(completed.animations.length, 0, 'A response arriving after the slide must not animate again')
assert.equal(completed.list.style.width, '70%')
console.log('Panel slides: continuous timeline across pane replacement, saved widths, unchanged timing, early/late replies, and unrelated OOB swaps passed.')
