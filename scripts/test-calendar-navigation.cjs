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
const calendar = {
  dataset: {calendarView: 'month'},
  querySelector() { return surface },
  getBoundingClientRect() { return {top: 0, left: 256} },
  appendChild(snapshot) { this.snapshot = snapshot; snapshot.removed = false },
}
let reducedMotion = false
const context = vm.createContext({
  document: {querySelector() { return surface }, getElementById() { return calendar }},
  window: {matchMedia() { return {matches: reducedMotion} }, getComputedStyle() { return {opacity: '0.6', transform: 'matrix(0.99, 0, 0, 0.99, 3, 0)'} }},
})
vm.runInContext('var _calendarNavigationRequest = null; var _calendarNavigationTransition = null;\n' +
  ['finishCalendarNavigationTransition', 'configureCalendarNavigationRequest', 'prepareCalendarNavigation', 'animateCalendarNavigation', 'handleCalendarNavigationResult'].map(helper).join('\n'), context)
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
for (const [from, to, direction] of [['month', 'week', 1], ['week', 'month', -1]]) {
  calendar.dataset.calendarView = from
  const xhr = {}, request = event(xhr)
  request.detail.elt = {dataset: {calendarViewSwitch: to}, hasAttribute(name) { return name === 'data-calendar-view-switch' }}
  context.configureCalendarNavigationRequest(request)
  assert.equal(xhr.goferCalendarNavigationDirection, direction, 'View switches must follow the tab direction')
  context.prepareCalendarNavigation(request)
  calendar.dataset.calendarView = to
  context.animateCalendarNavigation(request)
  const transition = context._calendarNavigationTransition
  assert.equal(transition.incoming.frames[0].transform, 'translateX(' + direction * 12 + 'px) scale(0.98)')
  assert.equal(transition.outgoing.frames[1].transform, 'translateX(' + -direction * 12 + 'px) scale(0.98)')
  assert.equal(transition.incoming.timing.duration, 220, 'View switches must reuse the period transition')
  transition.incoming.onfinish()
  const unchanged = event({})
  unchanged.detail.elt = request.detail.elt
  context.configureCalendarNavigationRequest(unchanged)
  assert.equal(unchanged.detail.xhr.goferCalendarNavigationDirection, undefined, 'The active tab must not replay the transition')
  assert.equal(context._calendarNavigationRequest, null)
}
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
reducedMotion = true
const immediate = {goferCalendarNavigationDirection: 1}
context._calendarNavigationRequest = immediate
context.prepareCalendarNavigation(event(immediate))
context.animateCalendarNavigation(event(immediate))
assert.equal(immediate.goferCalendarNavigationSnapshot, undefined)
assert.equal(context._calendarNavigationTransition, null)
assert.equal(context._calendarNavigationRequest, null)
console.log('Calendar navigation: period/view slide/fade/scale, active-tab no-op, inert snapshots, cleanup, refresh continuation, rapid requests, failures, and reduced motion passed.')
