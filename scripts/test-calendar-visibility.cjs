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
const requests = [], errors = []
let restores = 0
const context = vm.createContext({
  document: {querySelectorAll() { return [] }},
  fetch(url, options) { return new Promise((resolve, reject) => requests.push({url, options, resolve, reject})) },
  initializeCalendarDaySelection() { restores++ },
  showGoferToast(message) { errors.push(message) },
})
vm.runInContext('var _calendarVisibility = new Map();\n' +
  ['_calendarSourceIsVisible', 'initializeCalendarVisibility', 'saveCalendarVisibility', '_calendarMonthVisibleCount', 'layoutCalendarMonth'].map(helper).join('\n'), context)
function state(visible = false) { return {confirmed: true, visible, saving: false, revision: 1} }
async function settle() { await new Promise(resolve => setImmediate(resolve)) }

async function main() {
  const first = state()
  context.saveCalendarVisibility('work', first)
  assert.equal(requests.length, 1)
  assert.equal(first.saving, true)
  assert.equal(JSON.parse(requests[0].options.body).visible, false)
  first.visible = true
  first.revision++
  context.saveCalendarVisibility('work', first)
  assert.equal(requests.length, 1, 'Rapid clicks must serialize writes per calendar')
  requests[0].resolve({ok: true})
  await settle()
  assert.equal(requests.length, 2)
  assert.equal(JSON.parse(requests[1].options.body).visible, true, 'Latest choice must win')
  const personal = state()
  context.saveCalendarVisibility('personal', personal)
  assert.equal(requests.length, 3, 'Other calendars must save independently')
  requests[1].resolve({ok: true})
  requests[2].resolve({ok: true})
  await settle()
  assert.equal(first.confirmed, true)
  assert.equal(personal.confirmed, false)
  assert.equal(first.saving, false)

  const failed = state()
  context.saveCalendarVisibility('failed', failed)
  requests[3].resolve({ok: false})
  await settle()
  assert.equal(failed.visible, true, 'Failed save must restore the confirmed state')
  assert.equal(restores, 1)
  assert.equal(errors.length, 1)
  assert.equal(errors[0].variant, 'error')

  const superseded = state()
  context.saveCalendarVisibility('superseded', superseded)
  superseded.visible = true
  superseded.revision++
  requests[4].reject(new Error('lost response'))
  await settle()
  assert.equal(requests.length, 6, 'A newer choice must be sent after a failed older write')
  assert.equal(JSON.parse(requests[5].options.body).visible, true)
  requests[5].resolve({ok: true})
  await settle()
  assert.equal(errors.length, 1, 'An obsolete failure must not revert or alarm about the newer choice')

  const buttons = ['work', 'work', 'work', 'personal', 'personal', 'personal'].map(sourceID => ({
    dataset: {calendarSourceId: sourceID, calendarSourceHidden: 'false'}, style: {}, hidden: false, offsetHeight: 21,
  }))
  const day = {dataset: {calendarDayEventCount: '6'}}
  const overflow = {offsetHeight: 18, hidden: false, textContent: ''}
  const container = {clientHeight: 150, hidden: false, querySelectorAll() { return buttons }, querySelector() { return overflow }, closest() { return day }}
  const grid = {clientHeight: 500, querySelectorAll(selector) { return selector === '[data-calendar-month-events]' ? [container] : [] }}
  const calendar = {querySelector() { return grid }}
  context.layoutCalendarMonth(calendar)
  assert.equal(buttons.filter(node => !node.hidden).length, 3)
  assert.equal(overflow.textContent, '+3 more')
  context._calendarVisibility.set('work', {visible: false})
  context.layoutCalendarMonth(calendar)
  assert.equal(day.dataset.calendarDayEventCount, '3')
  assert.ok(buttons.slice(0, 3).every(node => node.hidden))
  assert.ok(buttons.slice(3).every(node => !node.hidden), 'Events beyond the original three must appear without refetching')
  assert.equal(overflow.hidden, true)
  context._calendarVisibility.set('personal', {visible: false})
  context.layoutCalendarMonth(calendar)
  assert.equal(day.dataset.calendarDayEventCount, '0')
  assert.equal(container.hidden, true)
  context._calendarVisibility.set('personal', {visible: true})
  context.layoutCalendarMonth(calendar)
  assert.equal(container.hidden, false)
  assert.ok(buttons.slice(3).every(node => !node.hidden), 'Revealing must reuse the intact cached DOM')
  assert.equal(context._calendarSourceIsVisible({dataset: {calendarSourceId: 'work', calendarSourceHidden: 'false'}}), false, 'An older response must not undo the optimistic preference')
  assert.equal(context._calendarSourceIsVisible({dataset: {calendarSourceId: 'unknown', calendarSourceHidden: 'true'}}), false, 'Initial server preferences must be respected')
  console.log('Calendar visibility: source filtering, month overflow/reveal, serialized/coalesced saves, independent calendars, stale responses, and rollback passed.')
}
main().catch(error => { console.error(error); process.exitCode = 1 })
