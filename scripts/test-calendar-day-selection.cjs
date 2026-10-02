const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('function _calendarAgendaEventMatchesDay(')
const end = source.indexOf('\n}', source.indexOf('function _calendarAgendaEventIsUpcoming(', start)) + 2
assert(start >= 0 && end > start, 'Calendar day filtering helpers were not found')
class TestDate extends Date {
  static now() { return Date.parse('2026-10-25T12:00:00Z') }
}
const context = vm.createContext({ Date: TestDate })
vm.runInContext(source.slice(start, end), context)
const day = {
  calendarDay: '2026-10-25',
  calendarDayStart: '2026-10-25T00:00:00+02:00',
  calendarDayEnd: '2026-10-26T00:00:00+01:00',
}
function timed(start, end) { return { calendarEventStart: start, calendarEventEnd: end } }
function allDay(start, end) { return { calendarEventAllDay: 'true', calendarEventStartDate: start, calendarEventEndDate: end } }
const cases = [
  ['all-day spans date', allDay('2026-10-24', '2026-10-26'), true],
  ['all-day exclusive end', allDay('2026-10-24', '2026-10-25'), false],
  ['cross-midnight', timed('2026-10-24T20:00:00Z', '2026-10-26T01:00:00Z'), true],
  ['ends at day start', timed('2026-10-24T20:00:00Z', '2026-10-24T22:00:00Z'), false],
  ['point at day start', timed('2026-10-24T22:00:00Z', '2026-10-24T22:00:00Z'), true],
  ['point at next day', timed('2026-10-25T23:00:00Z', '2026-10-25T23:00:00Z'), false],
  ['DST extra hour', timed('2026-10-25T22:30:00Z', '2026-10-25T22:45:00Z'), true],
  ['invalid timestamp', timed('', ''), false],
]
for (const [name, event, expected] of cases) {
  assert.equal(context._calendarAgendaEventMatchesDay(event, day), expected, name)
}
assert.equal(context._calendarAgendaEventIsUpcoming(allDay('2026-10-24', '2026-10-25'), '2026-10-25'), false)
assert.equal(context._calendarAgendaEventIsUpcoming(allDay('2026-10-25', '2026-10-26'), '2026-10-25'), true)
assert.equal(context._calendarAgendaEventIsUpcoming(timed('2026-10-25T10:00:00Z', '2026-10-25T11:00:00Z'), '2026-10-25'), false)
assert.equal(context._calendarAgendaEventIsUpcoming(timed('2026-10-25T13:00:00Z', '2026-10-25T14:00:00Z'), '2026-10-25'), true)
console.log('Calendar day filtering: date ranges, midnight boundaries, DST, and upcoming checks passed.')
