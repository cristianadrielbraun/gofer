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
function mockScroller() {
  const header = {offsetHeight: 104}
  const timeline = {style: {}}
  const grid = {style: {}, querySelectorAll() { return [] }, querySelector(selector) { return selector === '[data-calendar-week-header]' ? header : timeline }}
  return {dataset: {}, style: {}, clientHeight: 800, clientWidth: 1000, scrollTop: 0, scrollLeft: 0, querySelector() { return grid }}
}
let scroller = mockScroller()
const calendar = {
  dataset: {calendarMonth: '2026-10', calendarDate: '2026-10-01', calendarView: 'week', calendarPeriod: 'week:2026-09-28'},
  querySelector(selector) { return selector === '[data-calendar-week-scroll]' ? scroller : null },
  querySelectorAll() { return [] },
  hasAttribute() { return false },
}
const context = vm.createContext({clearTimeout() {}, document: {getElementById() { return calendar }}})
vm.runInContext('var _calendarWeekScrollState = null; var _calendarWeekZoom = 0; var _calendarVisibility = new Map();\n' +
  ['_calendarSourceIsVisible', '_calendarWeekAxis', '_calendarWeekMinutePosition', '_calendarWeekMinuteAtPosition', '_calendarWeekBlockLayout', '_calendarAllDayEventRows', 'layoutCalendarWeekAllDay', 'cancelCalendarWeekZoom', 'initializeCalendarWeekScroll', 'configureCalendarSyncRequest', 'calendarEventRequestCurrent', 'clearCalendarEventLoading'].map(helper).join('\n'), context)

const event = {detail: {parameters: {}}}
context.configureCalendarSyncRequest(event)
assert.deepEqual(event.detail.parameters, {month: '2026-10', view: 'week', date: '2026-10-01'})
context.initializeCalendarWeekScroll()
assert.equal(scroller.scrollTop, 0, 'New week should fit the screen without vertical scrolling')
scroller.scrollTop = 600
context.initializeCalendarWeekScroll()
assert.equal(scroller.scrollTop, 600, 'Unrelated swaps must not reset the timeline')
context._calendarWeekZoom = 1
context._calendarWeekScrollState = {period: 'week:2026-09-28', zoom: 1, top: 613, left: 250}
scroller = mockScroller()
context.initializeCalendarWeekScroll()
assert.equal(scroller.scrollTop, 613, 'Refresh should preserve vertical scroll')
assert.equal(scroller.scrollLeft, 250, 'Refresh should preserve horizontal scroll')
calendar.dataset.calendarPeriod = 'week:2026-10-05'
scroller = mockScroller()
context.initializeCalendarWeekScroll()
assert.equal(scroller.scrollTop, context._calendarWeekAxis(694, [], 1).offsets[7], 'A different zoomed week should get its own scroll state')
assert.equal(scroller.scrollLeft, 0)
scroller = {dataset: {}, clientHeight: 0, scrollTop: 0, scrollLeft: 0}
context.initializeCalendarWeekScroll()
assert.equal(scroller.dataset.calendarLayoutKey, undefined, 'Hidden mobile grid should wait until Back to week before restoring scroll')

for (const [property, variable] of [['goferCalendarSyncPeriod', '_calendarSyncRequest'], ['goferCalendarEventPeriod', '_calendarEventRequest']]) {
  const marker = source.indexOf('typeof xhr.' + property)
  const start = source.lastIndexOf('document.body.addEventListener("htmx:beforeSwap"', marker)
  const end = source.indexOf('\n})', marker)
  assert(marker > 0 && start > 0 && end > start, 'Missing stale response guard: ' + property)
  let guard
  context.document.body = {addEventListener(name, callback) { guard = callback }}
  vm.runInContext(source.slice(start, end + 3), context)
  const xhr = {[property]: 'week:2026-09-28'}
  context[variable] = xhr
  calendar.dataset.calendarPeriod = 'week:2026-09-28'
  const current = {detail: {xhr, shouldSwap: true}}
  guard(current)
  assert.equal(current.detail.shouldSwap, true)
  calendar.dataset.calendarPeriod = 'week:2026-10-05'
  const stale = {detail: {xhr, shouldSwap: true}}
  guard(stale)
  assert.equal(stale.detail.shouldSwap, false, 'Late response from another week must not replace the current one')
}
console.log('Calendar Week state: refresh parameters, scroll restoration, and stale-response guards passed.')

const buttons = ['month', 'week'].map(view => ({
  dataset: {calendarViewSwitch: view},
  attributes: {},
  classes: new Set(),
  setAttribute(name, value) { this.attributes[name] = value },
  classList: {toggle(name, active) { this.owner.classes[active ? 'add' : 'delete'](name) }},
}))
buttons.forEach(button => { button.classList.owner = button })
const indicator = {style: {}}
context.document.querySelector = () => ({querySelectorAll() { return buttons }, querySelector() { return indicator }})
vm.runInContext('var _calendarViewSwitchRequest = null;\n' + helper('setCalendarViewSwitch') + '\n' + helper('handleCalendarViewSwitchResult'), context)
context.setCalendarViewSwitch('week')
assert.equal(indicator.style.transform, 'translateX(calc(100% + 2px))')
assert.equal(buttons[1].attributes['aria-current'], 'page')
assert.ok(buttons[0].classes.has('text-muted-foreground'))
const switchXHR = {}
context._calendarViewSwitchRequest = switchXHR
context.handleCalendarViewSwitchResult({detail: {xhr: switchXHR, successful: true}})
assert.equal(indicator.style.transform, 'translateX(calc(100% + 2px))', 'Fast response must not reverse the slide')
calendar.dataset.calendarView = 'month'
context.handleCalendarViewSwitchResult({detail: {xhr: switchXHR, successful: false}})
assert.equal(indicator.style.transform, 'translateX(0)', 'Failed requests must restore the actual view')
assert.equal(buttons[0].attributes['aria-current'], 'page')
console.log('Calendar view tabs: sliding indicator, active colors, and failure recovery passed.')
