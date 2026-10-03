const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const css = fs.readFileSync(path.join(__dirname, '../assets/css/input.css'), 'utf8')
const listeners = {}, windowListeners = {}
const selector = '[data-calendar-month-event], [data-calendar-week-all-day], [data-calendar-week-event]'
const calendar = {pills: [], queries: 0, querySelectorAll(value) { assert.equal(value, selector); this.queries++; return this.pills }}
function pill(kind, id, source = 'work', hidden = false) {
  const attributes = new Set()
  return {
    dataset: {[kind]: id, calendarSourceId: source}, hidden,
    closest(value) { return value === '#calendar-main' ? calendar : value === selector ? this : null },
    toggleAttribute(name, value) { if (value) attributes.add(name); else attributes.delete(name) },
    removeAttribute(name) { attributes.delete(name) },
    get highlighted() { return attributes.has('data-calendar-event-hover') },
  }
}
const context = vm.createContext({
  document: {
    addEventListener(type, handler) { listeners[type] = handler },
    querySelectorAll(value) { assert.equal(value, '#calendar-main [data-calendar-event-hover]'); return calendar.pills.filter(pill => pill.highlighted) },
  },
  window: {addEventListener(type, handler) { windowListeners[type] = handler }},
})
vm.runInContext(source.slice(source.indexOf('function updateCalendarEventHover('), source.indexOf('var _calendarEventRequest = null')), context)
function pointer(type, target, relatedTarget = null, pointerType = 'mouse') {
  listeners[type]({type, target, relatedTarget, pointerType})
}

for (const kind of ['calendarMonthEvent', 'calendarWeekAllDay', 'calendarWeekEvent']) {
  const segments = [0, 1, 2, 3].map(() => pill(kind, 'multi-day'))
  const other = pill(kind, 'different-event')
  const otherSource = pill(kind, 'multi-day', 'personal')
  const hidden = pill(kind, 'multi-day', 'work', true)
  calendar.pills = [...segments, other, otherSource, hidden]
  pointer('pointerover', segments[1])
  assert(segments.every(pill => pill.highlighted), kind + ': highlight the entire event, including wrapped rows')
  assert.equal(other.highlighted, false)
  assert.equal(otherSource.highlighted, false, 'Never group events from different calendars')
  assert.equal(hidden.highlighted, false, 'Do not expose hidden or overflowed event segments')

  const queries = calendar.queries
  const label = {closest(value) { return segments[1].closest(value) }}
  pointer('pointerout', segments[1], label)
  pointer('pointerover', label, segments[1])
  assert.equal(calendar.queries, queries, 'Ignore movement between children of one pill')

  pointer('pointerout', segments[1], segments[2])
  assert(segments.every(pill => pill.highlighted), 'Moving across day boundaries must not clear the shared highlight')
  pointer('pointerover', segments[2], segments[1])
  assert(segments.every(pill => pill.highlighted))

  pointer('pointerout', segments[2], other)
  assert(segments.every(pill => !pill.highlighted))
  assert(other.highlighted, 'Moving to another event transfers the hover')
  pointer('pointerout', other)
  assert(calendar.pills.every(pill => !pill.highlighted), 'Leaving the event must clear every segment')

  pointer('pointerover', segments[0], null, 'touch')
  assert(calendar.pills.every(pill => !pill.highlighted), 'Touch taps must not leave sticky hover state')
  pointer('pointerover', segments[0])
  pointer('pointercancel', segments[0])
  assert(calendar.pills.every(pill => !pill.highlighted))
  pointer('pointerover', segments[0])
  windowListeners.blur()
  assert(calendar.pills.every(pill => !pill.highlighted), 'Clear hover when the window loses focus')
}

calendar.pills = [pill('calendarMonthEvent', 'id["with\\selectors]'), pill('calendarMonthEvent', 'id["with\\selectors]')]
pointer('pointerover', calendar.pills[0])
assert(calendar.pills.every(pill => pill.highlighted), 'Use literal event identity even after HTMX replaces the grid')
const outside = {closest() { return null }}
pointer('pointerout', calendar.pills[0], outside)
assert(calendar.pills.every(pill => !pill.highlighted), 'Upcoming and other non-grid elements must not keep a pill highlighted')
pointer('pointerover', outside)

assert(css.includes(':is([data-calendar-month-event], [data-calendar-week-all-day]):is(:hover, [data-calendar-event-hover])'))
assert(css.includes(':root:not(.dark) :is([data-calendar-month-event], [data-calendar-week-all-day], [data-calendar-week-event]):is(:hover, [data-calendar-event-hover])'))
assert(css.includes('[data-calendar-week-event][data-calendar-event-hover]'))
console.log('Calendar hover: joined month/week segments, wrapped rows, scoped identities, child transitions, hidden events, touch, cancellation, blur, and light/dark styles passed.')
