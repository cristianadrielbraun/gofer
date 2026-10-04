const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
function helper(name) {
  const start = source.indexOf('function ' + name + '(')
  const end = source.indexOf('\n}', start)
  assert(start >= 0 && end > start, 'Missing helper: ' + name)
  return source.slice(start, end + 2)
}
let reducedMotion = false
function pill(id, kind = 'calendarMonthEvent', source = 'work') {
  return {
    dataset: {[kind]: id, calendarSourceId: source}, hidden: false, parentElement: null,
    style: {setProperty(name, value) { this[name] = value }}, opacity: '1', animations: [],
    attributes: [{name: 'id', value: 'duplicate'}, {name: 'hx-get', value: '/event'}, {name: 'data-hx-target', value: '#app-pane-dialogs'}, {name: 'onclick', value: 'bad()'}, {name: 'data-calendar-event-trigger', value: ''}],
    getClientRects() { return this.hidden || this.style.display === 'none' ? [] : [this.getBoundingClientRect()] },
    getBoundingClientRect() { return {top: 150, left: 300, right: 442, bottom: 174, width: 142, height: 24} },
    closest() { return this.scroller || null },
    querySelectorAll() { return [] },
    removeAttribute(name) { this.attributes = this.attributes.filter(attr => attr.name !== name) },
    setAttribute(name, value) { this.attributes.push({name, value}) },
    cloneNode() {
      const copy = pill(id, kind, source)
      copy.attributes = this.attributes.map(attr => ({...attr}))
      copy.dataset = {...this.dataset}
      copy.style = {...this.style}
      copy.hidden = this.hidden
      return copy
    },
    remove() { this.removed = true; this.parentElement = null },
    animate(frames, timing) {
      const animation = {frames, timing, cancel() { this.cancelled = true }}
      this.animations.push(animation)
      return animation
    },
  }
}
function calendar(nodes, view = 'month', period = 'month:2026-10') {
  const root = {
    dataset: {calendarView: view, calendarPeriod: period}, nodes, ghosts: [], loading: false, animate() {},
    getBoundingClientRect() { return {top: 50, left: 256} },
    querySelectorAll() { return [...this.nodes, ...this.ghosts.filter(node => !node.removed && node.parentElement === this)] },
    hasAttribute(name) { return name === 'data-calendar-loading' && this.loading },
    appendChild(node) { this.ghosts.push(node); node.parentElement = this; node.removed = false },
  }
  nodes.forEach(node => { node.parentElement = root })
  return root
}
const context = vm.createContext({window: {
  matchMedia() { return {matches: reducedMotion} },
  getComputedStyle(node) { return {opacity: node.opacity, getPropertyValue() { return '0.5rem' }} },
}})
vm.runInContext('var _calendarPillSnapshot = null; var _calendarPillFades = new Map();\n' +
  ['captureCalendarPills', 'rememberCalendarPillGeometry', 'finishCalendarPillFade', 'clearCalendarPillFades', 'fadeCalendarPill', 'animateCalendarPills'].map(helper).join('\n'), context)
function finish() { [...context._calendarPillFades.values()].forEach(fade => fade.animation.onfinish()) }
function cadence(animation) {
  assert.equal(animation.timing.duration, 300)
  assert.equal(animation.timing.easing, 'ease')
  assert.deepEqual(Object.keys(animation.frames[0]), ['opacity'], 'Pill transitions must not slide or scale')
}

for (const [kind, view] of [['calendarMonthEvent', 'month'], ['calendarWeekAllDay', 'week'], ['calendarWeekEvent', 'week']]) {
  context.animateCalendarPills(null)
  const first = pill('first', kind), second = pill('second', kind)
  let root = calendar([first, second], view)
  context.animateCalendarPills(root)
  assert.equal(first.animations.length, 1)
  assert.equal(first.animations[0].frames[0].opacity, 0)
  assert.equal(first.animations[0].frames[1].opacity, '1')
  cadence(first.animations[0])
  context.animateCalendarPills(root)
  assert.equal(first.animations.length, 1, 'Repeated layout passes must not restart entry fades')
  finish()

  // Cache swaps preserve unchanged event IDs instead of flashing every pill.
  const nextFirst = pill('first', kind), nextSecond = pill('second', kind), added = pill('added', kind)
  context.rememberCalendarPillGeometry(root)
  root = calendar([nextFirst, nextSecond, added], view)
  context.animateCalendarPills(root)
  assert.equal(nextFirst.animations.length, 0)
  assert.equal(added.animations.length, 1, 'Newly synchronized events must fade in')
  finish()

  context.rememberCalendarPillGeometry(root)
  nextFirst.hidden = true
  nextFirst.style.display = 'none'
  context.animateCalendarPills(root)
  const ghost = root.ghosts.at(-1)
  assert.ok(ghost.inert && !ghost.hidden)
  assert.equal(ghost.style.pointerEvents, 'none')
  assert.equal(ghost.style.top, '100px')
  assert.equal(ghost.style.left, '44px')
  assert.equal(ghost.style.width, '142px')
  assert.equal(ghost.style['--calendar-day-padding'], '0.5rem', 'Multi-day seam geometry must survive copying')
  assert.ok(ghost.attributes.some(attr => attr.name === 'aria-hidden' && attr.value === 'true'))
  assert.deepEqual(ghost.attributes.filter(attr => attr.name !== 'aria-hidden').map(attr => attr.name), ['data-calendar-event-trigger'], 'Exit copies retain theme styling but have no duplicate IDs or executable actions')
  cadence(ghost.animations[0])
  assert.equal(ghost.animations[0].frames[1].opacity, 0)

  // Reverse an exit from its visible opacity, without a flash or stale cleanup.
  ghost.opacity = '0.4'
  const oldFinish = ghost.animations[0].onfinish
  nextFirst.hidden = false
  nextFirst.style.display = ''
  context.animateCalendarPills(root)
  assert.ok(ghost.removed)
  assert.equal(nextFirst.animations.at(-1).frames[0].opacity, '0.4')
  oldFinish()
  assert.equal(context._calendarPillFades.size, 1, 'An old completion must not cancel a reversed entry')

  nextFirst.opacity = '0.6'
  context.rememberCalendarPillGeometry(root)
  const replacement = pill('first', kind)
  root = calendar([replacement, nextSecond, added], view)
  context.animateCalendarPills(root)
  assert.equal(replacement.animations[0].frames[0].opacity, '0.6', 'A refresh during entry continues the current opacity')
  finish()

  // Deletion has the same exit fade even when the old node is detached.
  context.rememberCalendarPillGeometry(root)
  root = calendar([nextSecond, added], view)
  context.animateCalendarPills(root)
  const deleted = root.ghosts.at(-1)
  assert.ok(deleted && !deleted.removed)
  const refreshed = calendar([pill('second', kind), pill('added', kind)], view)
  context.animateCalendarPills(refreshed)
  assert.equal(deleted.parentElement, refreshed, 'A refresh must not cut short an exit fade')
  finish()
  assert.ok(deleted.removed)
}

context.animateCalendarPills(null)
const clipped = pill('clipped', 'calendarWeekEvent')
clipped.scroller = {getBoundingClientRect() { return {top: 160, left: 320, right: 420, bottom: 170} }}
const clippedWeek = calendar([clipped], 'week')
context.animateCalendarPills(clippedWeek)
finish()
context.rememberCalendarPillGeometry(clippedWeek)
clipped.hidden = true
context.animateCalendarPills(clippedWeek)
assert.equal(clippedWeek.ghosts[0].style.clipPath, 'inset(10px 22px 4px 20px)', 'Exit copies must respect the original scroll viewport, never covering headers or adjacent days')
finish()
context.animateCalendarPills(null)
const segments = [pill('trip'), pill('trip'), pill('trip')]
const trip = calendar(segments)
context.animateCalendarPills(trip)
assert.equal(context._calendarPillFades.size, 3, 'Each multi-day segment must fade with the same cadence')
finish()
context.rememberCalendarPillGeometry(trip)
segments.forEach(node => { node.hidden = true })
context.animateCalendarPills(trip)
assert.equal(context._calendarPillFades.size, 3)
assert.equal(context.captureCalendarPills(trip).pills.size, 0, 'Exit copies must not become live event pills')
reducedMotion = true
segments.forEach(node => { node.hidden = false })
context.animateCalendarPills(trip)
assert.equal(context._calendarPillFades.size, 0)
assert.ok(trip.ghosts.every(node => node.removed), 'Reduced motion immediately removes exit copies')
reducedMotion = false
const nextPeriod = calendar([pill('next')], 'month', 'month:2026-11')
context.animateCalendarPills(nextPeriod)
assert.equal(nextPeriod.ghosts.length, 0, 'Period/view navigation must not overlay pills from an unrelated grid')
nextPeriod.loading = true
context.animateCalendarPills(nextPeriod)
assert.equal(context._calendarPillSnapshot, null)
assert.equal(context._calendarPillFades.size, 0)
console.log('Calendar pill fades: month/week, matching cadence, stable refreshes, additions/deletions, visibility reversal, multi-day segments, inert exits, cleanup, and reduced motion passed.')
