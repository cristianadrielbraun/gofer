const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('function _calendarWeekAxis(')
const end = source.indexOf('function layoutCalendarMonth(', start)
assert(start >= 0 && end > start)
const context = vm.createContext({})
vm.runInContext(source.slice(start, end), context)
function near(actual, expected) { assert.ok(Math.abs(actual - expected) < 0.001, `${actual} != ${expected}`) }

for (const height of [150, 420, 694, 1100]) {
  const periods = [{start: 540, end: 600}, {start: 1410, end: 1440}, {start: 0, end: 30}]
  const axis = context._calendarWeekAxis(height, periods, 0)
  near(axis.offsets[24], height)
  assert.ok(axis.heights.every(value => value > 0))
  assert.ok(axis.heights[9] > axis.heights[3] * 3, 'Busy hours must be larger than empty hours')
  assert.ok(axis.expanded[0] && axis.expanded[9] && axis.expanded[23], 'Midnight and late events must not be hidden')
  assert.equal(axis.expanded[10], false, 'End times are exclusive')
  for (const minute of [0, 30, 539, 540, 570, 600, 1439, 1440]) {
    near(context._calendarWeekMinuteAtPosition(context._calendarWeekMinutePosition(minute, axis), axis), minute)
  }
  for (const zoom of [1, 2, 3]) {
    const expanded = context._calendarWeekAxis(height, periods, zoom)
    assert.ok(expanded.height > height, 'Vertical scrolling must be an explicit zoom choice')
    expanded.heights.forEach(value => near(value, expanded.height / 24))
  }
}
const empty = context._calendarWeekAxis(694, [], 0)
assert.ok(empty.heights[9] > empty.heights[2], 'An empty week should favor ordinary daytime hours')
const full = context._calendarWeekAxis(694, [{start: 0, end: 1440}], 0)
full.heights.forEach(value => near(value, 694 / 24))
const point = context._calendarWeekAxis(694, [{start: 720, end: 720}], 0)
assert.equal(point.expanded[12], true)
assert.equal(point.expanded[11], false)

const axis = context._calendarWeekAxis(420, [{start: 540, end: 660}], 0)
const blocks = context._calendarWeekBlockLayout([
  {start: 540, end: 660}, {start: 570, end: 600}, {start: 600, end: 660},
  {start: 1410, end: 1440}, {start: 1439, end: 1439},
  {start: 660, end: 660}, {start: 665, end: 665},
], axis)
const byIndex = Object.fromEntries(blocks.map(block => [block.index, block]))
assert.equal(byIndex[0].columns, 2)
assert.equal(byIndex[1].column, 1)
assert.equal(byIndex[2].column, 1, 'Touching endpoints should reuse an overlap column')
for (const block of blocks) {
  assert.ok(block.top >= 0 && block.end <= axis.height + 0.01)
  assert.ok(block.height >= 22 - 0.01, 'Every event must stay clickable in Fit mode')
}
for (let left = 0; left < blocks.length; left++) {
  for (let right = left + 1; right < blocks.length; right++) {
    if (blocks[left].top < blocks[right].end - 0.01 && blocks[right].top < blocks[left].end - 0.01) {
      assert.notEqual(blocks[left].column, blocks[right].column, 'Expanded short-event hitboxes must not cover one another')
    }
  }
}
const count = context._calendarMonthVisibleCount
assert.equal(count(12, 150, 21, 19, 4), 3)
assert.equal(count(3, 40, 21, 19, 4), 0)
assert.equal(count(3, 45, 21, 19, 4), 1)
assert.equal(count(2, 46, 21, 19, 4), 2)
assert.equal(count(3, 10, 21, 19, 4), 0)

const span = (id, start, end) => ({dataset: {calendarMonthEvent: id, calendarEventAllDay: 'true', calendarEventStartDate: start, calendarEventEndDate: end}})
const early = [span('early', '2026-10-01', '2026-10-03'), span('early', '2026-10-01', '2026-10-03')]
const trip = [span('trip', '2026-10-02', '2026-10-05'), span('trip', '2026-10-02', '2026-10-05')]
const timed = {dataset: {}}, single = span('single', '2026-10-02', '2026-10-03')
const days = [{buttons: [timed, early[0]]}, {buttons: [early[1], single, trip[0]]}, {buttons: [trip[1]]}]
const rows = context._calendarAllDayEventRows(days)
assert.equal(rows.get(early[0]), rows.get(early[1]))
assert.equal(rows.get(trip[0]), rows.get(trip[1]), 'A continuous event must not jump rows when another event ends')
assert.notEqual(rows.get(trip[0]), rows.get(early[1]), 'Overlapping all-day events need separate rows')
assert.notEqual(rows.get(single), rows.get(trip[0]), 'Single-day events must not overlap a spanning event')
assert.notEqual(rows.get(timed), rows.get(early[0]), 'Timed events must leave room for all-day bars')
const filtered = context._calendarAllDayEventRows(days.map(day => ({buttons: day.buttons.filter(button => !early.includes(button))})))
assert.equal(filtered.get(trip[0]), 0, 'Hiding another calendar must reclaim its row')
assert.equal(filtered.get(trip[1]), 0)
console.log('Calendar layout: screen fit, occupied-hour weighting, zoom, time mapping, overlap hitboxes, and month overflow passed.')
