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
function node() {
  return {
    style: {}, attributes: [], children: [], scrollTop: 180, scrollLeft: 120, clientHeight: 700,
    setAttribute(name, value) { this.attributes.push({name, value}) },
    removeAttribute(name) { this.attributes = this.attributes.filter(attribute => attribute.name !== name) },
    querySelectorAll() { return this.children },
    querySelector() { return null },
    hasAttribute(name) { return this.attributes.some(attribute => attribute.name === name) },
    remove() { this.removed = true },
    getBoundingClientRect() { return {top: 100, left: 256, width: 1000, height: 700} },
    cloneNode() {
      const clone = node(), child = node()
      clone.attributes = [{name: 'data-calendar-surface', value: ''}]
      child.attributes = [{name: 'id', value: 'duplicate'}, {name: 'data-calendar-week-event', value: 'event'}, {name: 'hx-get', value: '/event'}, {name: 'class', value: 'event'}]
      clone.children = [child]
      return clone
    },
    animate(frames, timing) {
      const animation = {frames, timing, currentTime: 0, cancel() { this.cancelled = true }}
      this.animation = animation
      return animation
    },
  }
}
const surface = node()
// The period slide is for views without a month grid; months only fade their
// days' contents (checked further down).
const calendar = {
  dataset: {calendarView: 'week'},
  querySelector(selector) { return selector === '[data-calendar-week-header]' ? null : surface },
  getBoundingClientRect() { return {top: 0, left: 256} },
  appendChild(snapshot) { this.snapshot = snapshot; snapshot.removed = false },
}
let reducedMotion = false
const context = vm.createContext({
  document: {querySelector() { return surface }, getElementById() { return calendar }},
  window: {matchMedia() { return {matches: reducedMotion} }, getComputedStyle() { return {opacity: '0.6', transform: 'matrix(0.99, 0, 0, 0.99, 3, 0)'} }},
})
vm.runInContext('var CALENDAR_MONTH_FADE_MS = 240; var _calendarNavigationRequest = null; var _calendarNavigationTransition = null; var _calendarWeekSlide = null;\n' +
  source.slice(source.indexOf('var CALENDAR_VIEWS'), source.indexOf('\n', source.indexOf('var CALENDAR_VIEWS'))) + '\n' +
  ['calendarScrollArea', 'calendarSnapshotScrollArea', 'finishCalendarNavigationTransition', 'fadeInCalendarMonthContents', 'configureCalendarNavigationRequest', 'prepareCalendarNavigation', 'animateCalendarNavigation', 'handleCalendarNavigationResult'].map(helper).join('\n'), context)
function event(xhr, shouldSwap = true) { return {detail: {xhr, shouldSwap, target: {id: 'main-content'}}} }
function navigate(direction) {
  const xhr = {goferCalendarNavigationDirection: direction}
  context._calendarNavigationRequest = xhr
  context.prepareCalendarNavigation(event(xhr))
  context.animateCalendarNavigation(event(xhr))
  return context._calendarNavigationTransition
}

for (const direction of [1, -1]) {
  const transition = navigate(direction)
  assert.equal(transition.incoming.frames[0].transform, 'translateX(' + direction * 12 + 'px) scale(0.98)')
  assert.equal(transition.incoming.frames[1].transform, 'translateX(0) scale(1)', 'Incoming pane must settle at its full size')
  assert.equal(transition.outgoing.frames[0].transform, 'translateX(0) scale(1)')
  assert.equal(transition.outgoing.frames[1].transform, 'translateX(' + -direction * 12 + 'px) scale(0.98)')
  assert.equal(transition.incoming.timing.duration, 220)
  assert.equal(transition.snapshot.style.top, '100px', 'Only the content viewport should be overlaid')
  assert.equal(transition.snapshot.scrollTop, 180, 'Zoomed scroll position must survive in the snapshot')
  assert.equal(transition.snapshot.scrollLeft, 120)
  assert.ok(transition.snapshot.inert)
  assert.ok(transition.snapshot.attributes.some(attribute => attribute.name === 'aria-hidden' && attribute.value === 'true'))
  assert.deepEqual(transition.snapshot.children[0].attributes.map(attribute => attribute.name), ['class'])
  transition.incoming.onfinish()
  assert.equal(context._calendarNavigationTransition, null)
  assert.ok(transition.snapshot.removed && transition.incoming.cancelled && transition.outgoing.cancelled)
}
// The tabs run Day, 3 days, Work week, Week, Month: moving right slides forward.
for (const [from, to, direction] of [['month', 'week', -1], ['week', 'month', 1], ['day', '3day', 1], ['workweek', 'day', -1]]) {
  // A week change still in flight, then the view switch.
  calendar.dataset.calendarView = 'week'
  const previous = navigate(1)
  calendar.dataset.calendarView = from
  const xhr = {}, request = event(xhr)
  request.detail.elt = {dataset: {calendarViewSwitch: to}, hasAttribute(name) { return name === 'data-calendar-view-switch' }}
  context.configureCalendarNavigationRequest(request)
  assert.equal(xhr.goferCalendarNavigationDirection, direction, 'View switches must follow the tab direction')
  assert.ok(previous.snapshot.removed && previous.incoming.cancelled, 'View switches must stop any existing grid transition')
  context.prepareCalendarNavigation(request)
  assert.equal(xhr.goferCalendarNavigationSnapshot.bounds.height, 700, 'The outgoing view must retain its original geometry')
  calendar.dataset.calendarView = to
  context.animateCalendarNavigation(request)
  const transition = context._calendarNavigationTransition
  assert.equal(transition.snapshot.style.backgroundColor, 'var(--color-card)', 'The outgoing surface must cover the new grid until it dissolves')
  for (const frame of transition.incoming.frames) assert.equal(frame.opacity, 1, 'The new view must stay fully opaque to avoid flashing the page background')
  assert.equal(transition.outgoing.frames[0].opacity, 1)
  assert.equal(transition.outgoing.frames[1].opacity, 0)
  for (const frame of [...transition.incoming.frames, ...transition.outgoing.frames]) {
    assert.equal(frame.transform, undefined, 'The crossover must not move or scale either view')
  }
  assert.equal(transition.incoming.timing.duration, 140)
  assert.equal(context._calendarNavigationRequest, null)
  transition.incoming.currentTime = 40
  const refresh = {}
  context.prepareCalendarNavigation(event(refresh))
  context.animateCalendarNavigation(event(refresh))
  const resumed = context._calendarNavigationTransition
  assert.equal(resumed.snapshot, transition.snapshot)
  assert.equal(resumed.outgoing.frames[0].opacity, '0.6', 'Refresh must continue from the existing dissolve')
  for (const frame of resumed.incoming.frames) assert.equal(frame.opacity, 1, 'Refresh must keep the new view opaque')
  for (const frame of [...resumed.incoming.frames, ...resumed.outgoing.frames]) assert.equal(frame.transform, undefined)
  assert.equal(resumed.incoming.timing.duration, 100, 'Refresh must only run the remaining crossover time')
  resumed.incoming.onfinish()
  const unchanged = event({})
  unchanged.detail.elt = request.detail.elt
  context.configureCalendarNavigationRequest(unchanged)
  assert.equal(unchanged.detail.xhr.goferCalendarNavigationDirection, undefined, 'The active tab must not replay the transition')
  assert.equal(context._calendarNavigationRequest, null)
}
calendar.dataset.calendarView = 'week'
const first = navigate(1)
const second = navigate(-1)
assert.ok(first.snapshot.removed && first.incoming.cancelled, 'Rapid navigation must retire the previous overlay')
first.incoming.onfinish()
assert.equal(context._calendarNavigationTransition, second, 'An old completion cannot remove a newer transition')
second.incoming.currentTime = 70
const refresh = {}
context.prepareCalendarNavigation(event(refresh))
context.animateCalendarNavigation(event(refresh))
const resumed = context._calendarNavigationTransition
assert.equal(resumed.snapshot, second.snapshot, 'Background refresh must retain the outgoing period')
assert.equal(resumed.incoming.frames[0].opacity, '0.6', 'Refresh must continue from the current fade')
assert.equal(resumed.incoming.frames[0].transform, 'matrix(0.99, 0, 0, 0.99, 3, 0)', 'Refresh must preserve the current incoming scale')
assert.equal(resumed.outgoing.frames[0].transform, 'matrix(0.99, 0, 0, 0.99, 3, 0)', 'Refresh must preserve the current outgoing scale')
assert.equal(resumed.incoming.timing.duration, 150, 'Refresh should only animate the remaining time')
resumed.incoming.onfinish()

const pending = {goferCalendarNavigationDirection: 1}
context._calendarNavigationRequest = pending
context.prepareCalendarNavigation(event(pending))
const original = pending.goferCalendarNavigationSnapshot.element
context.prepareCalendarNavigation(event(pending))
assert.equal(pending.goferCalendarNavigationSnapshot.element, original, 'A pending skeleton must not replace the real outgoing snapshot')
context.animateCalendarNavigation(event(pending))
context._calendarNavigationTransition.incoming.onfinish()

const stale = event({goferCalendarNavigationDirection: 1})
context._calendarNavigationRequest = {}
context.prepareCalendarNavigation(stale)
assert.equal(stale.detail.shouldSwap, false, 'A replaced navigation response must not animate or swap')
const rejected = {goferCalendarNavigationDirection: 1}
context._calendarNavigationRequest = rejected
context.prepareCalendarNavigation(event(rejected, false))
assert.equal(rejected.goferCalendarNavigationSnapshot, undefined)
context.handleCalendarNavigationResult({detail: {xhr: rejected, successful: false}})
assert.equal(context._calendarNavigationRequest, null, 'Errors/aborts must not leave a pending navigation')
// Month to month: no outgoing copy and no moving grid; only the days' contents
// fade in, drifting a few pixels from the side being moved towards.
calendar.dataset.calendarView = 'month'
const contents = [node(), node(), node()]
let asked = ''
calendar.querySelector = selector => selector === '[data-calendar-month-grid]' ? {animate() {}, querySelectorAll(query) { asked = query; return contents }} : surface
for (const direction of [1, -1]) {
  const xhr = {goferCalendarNavigationDirection: direction}
  context._calendarNavigationRequest = xhr
  context.prepareCalendarNavigation(event(xhr))
  assert.equal(xhr.goferCalendarNavigationSnapshot, undefined, 'Months must not copy the outgoing grid')
  context.animateCalendarNavigation(event(xhr))
  assert.equal(context._calendarNavigationTransition, null, 'Months must not slide the grid')
  for (const node of contents) {
    assert.equal(JSON.stringify(node.animation.frames), JSON.stringify([{opacity: 0, transform: 'translateX(' + direction * 4 + 'px)'}, {opacity: 1, transform: 'translateX(0)'}]))
    assert.equal(node.animation.timing.duration, 240)
  }
  assert.ok(asked.includes('[data-calendar-day-head]') && asked.includes('[data-calendar-day-dots]'), 'Only day numbers, dots and overflow fade')
}
// A view switch away from Month still dissolves the whole view.
const leaving = {}, leave = event(leaving)
leave.detail.elt = {dataset: {calendarViewSwitch: 'week'}, hasAttribute(name) { return name === 'data-calendar-view-switch' }}
context.configureCalendarNavigationRequest(leave)
context.prepareCalendarNavigation(leave)
assert.ok(leaving.goferCalendarNavigationSnapshot, 'Leaving Month must keep its crossover')
context.handleCalendarNavigationResult({detail: {xhr: leaving}})
calendar.querySelector = selector => selector === '[data-calendar-week-header]' ? null : surface

reducedMotion = true
const immediate = {goferCalendarNavigationDirection: 1}
context._calendarNavigationRequest = immediate
context.prepareCalendarNavigation(event(immediate))
context.animateCalendarNavigation(event(immediate))
assert.equal(immediate.goferCalendarNavigationSnapshot, undefined)
assert.equal(context._calendarNavigationTransition, null)
assert.equal(context._calendarNavigationRequest, null)
console.log('Calendar navigation: month contents fade, period slide/fade/scale, opaque view crossover, active-tab no-op, inert snapshots, cleanup, refresh continuation, rapid requests, failures, and reduced motion passed.')
