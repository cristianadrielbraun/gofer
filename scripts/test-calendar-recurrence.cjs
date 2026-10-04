const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
function helper(name) {
  const start = source.indexOf('function ' + name + '('), end = source.indexOf('\n}', start)
  assert(start > 0 && end > start)
  return source.slice(start, end + 2)
}
const inputs = Object.fromEntries(Object.entries({
  request_id: 'stable-request', source_id: 'work', summary: 'Planning', start_date: '2026-10-02', end_date: '2026-10-02',
  start_time: '09:00', end_time: '10:00', timezone: 'Europe/Prague', all_day: 'true',
  repeat_frequency: 'none', repeat_interval: '1', repeat_end: 'never', repeat_until: '', repeat_count: '10',
}).map(([name, value]) => [name, {name, value, disabled: false, checked: false}]))
const groups = Object.fromEntries(['options', 'until', 'count', 'interval-group'].map(name => [name, {hidden: true, disabled: true}]))
const row = {dataset: {}}
const unit = {textContent: ''}, summary = {textContent: ''}, untilTrigger = {focus() { this.focused = true }}
let message = '', validity = true, refreshes = 0
const form = {
  dataset: {}, isConnected: true, reportValidity() { return validity },
  querySelector(selector) {
    const name = selector.match(/^\[name="([^"]+)"\]$/)?.[1]
    if (name) return inputs[name]
    if (selector === '[data-calendar-repeat-row]') return row
    if (selector === '[data-calendar-repeat-unit]') return unit
    if (selector === '[data-calendar-repeat-summary]') return summary
    if (selector === '#calendar-create-repeat-until') return untilTrigger
    return groups[selector.match(/^\[data-calendar-repeat-(.+)\]$/)?.[1]]
  },
}
function enabled(input) {
  if (input.disabled) return false
  if (input.name === 'all_day') return input.checked
  if (input.name === 'repeat_frequency' || !input.name.startsWith('repeat_')) return true
  if (input.name === 'repeat_interval') return !groups['interval-group'].disabled
  if (groups.options.disabled) return false
  if (input.name === 'repeat_until') return !groups.until.disabled
  if (input.name === 'repeat_count') return !groups.count.disabled
  return true
}
const requests = [], toasts = [], closes = []
const context = vm.createContext({
  URLSearchParams, Date, Intl,
  FormData: class { constructor() { this.entries = Object.values(inputs).filter(enabled).map(input => [input.name, input.value]) } [Symbol.iterator]() { return this.entries[Symbol.iterator]() } },
  window: {tui: {dialog: {close(id) { closes.push(id) }}}},
  fetch(url, options) { return new Promise((resolve, reject) => requests.push({url, options, resolve, reject})) },
  adjustCalendarCreateAllDayRange() {}, setCalendarCreateError(_form, value) { message = value },
  showGoferToast(value) { toasts.push(value) }, scheduleCalendarCacheRefresh() { refreshes++ },
})
vm.runInContext(['updateCalendarRecurrenceForm', 'validateCalendarCreatePickers', 'syncCalendarDescription', 'submitCalendarCreate'].map(helper).join('\n'), context)
context.updateCalendarCreateForm = form => {
  const locked = !!(form._calendarCreateBusy || form._calendarCreateUncertain || form._calendarCreateConflict)
  Object.values(inputs).forEach(input => { input.disabled = locked })
  context.updateCalendarRecurrenceForm(form, locked)
}
async function main() {
  context.updateCalendarCreateForm(form)
  assert.equal(row.dataset.repeating, 'false', 'Does not repeat uses the full row')
  assert.equal(groups.options.hidden, true)
  assert.equal(groups.options.disabled, true)
  assert.deepEqual(Object.values(inputs).filter(enabled).filter(input => input.name.startsWith('repeat_')).map(input => input.name), ['repeat_frequency'], 'Hidden options must not enter FormData or native validity checks')
  inputs.repeat_frequency.value = 'weekly'
  context.updateCalendarCreateForm(form)
  assert.equal(row.dataset.repeating, 'true', 'Repeating switches to two equal-width columns')
  assert.equal(groups.options.hidden, false)
  assert.equal(groups.options.disabled, false)
  assert.equal(groups.until.disabled, true)
  assert.equal(groups.count.disabled, true)
  assert.equal(unit.textContent, 'week')
  assert.match(summary.textContent, /Every 1 week on Friday, with no end date/)
  assert.match(summary.textContent, /Europe\/Prague/)
  inputs.repeat_interval.value = '2'
  inputs.repeat_end.value = 'count'
  inputs.repeat_count.value = '4'
  context.updateCalendarCreateForm(form)
  assert.equal(unit.textContent, 'weeks')
  assert.equal(groups.count.disabled, false)
  assert.equal(groups.until.disabled, true)
  assert.match(summary.textContent, /4 occurrences including the first/)
  inputs.repeat_end.value = 'until'
  context.updateCalendarCreateForm(form)
  assert.equal(groups.count.disabled, true, 'Switching range must stop submitting the old count')
  assert.equal(groups.until.disabled, false)
  assert.equal(context.validateCalendarCreatePickers(form), false)
  assert.match(message, /last date/)
  assert.equal(untilTrigger.focused, true)
  inputs.repeat_until.value = '2026-10-01'
  assert.equal(context.validateCalendarCreatePickers(form), false)
  assert.match(message, /on or after/)
  inputs.repeat_until.value = '2026-10-02'
  assert.equal(context.validateCalendarCreatePickers(form), true, 'The last start date is inclusive')
  inputs.repeat_frequency.value = 'monthly'
  inputs.start_date.value = '2026-01-31'
  inputs.all_day.checked = true
  context.updateCalendarCreateForm(form)
  assert.match(summary.textContent, /day 31/)
  assert.match(summary.textContent, /last day in shorter months/)
  assert.doesNotMatch(summary.textContent, /Times follow/)
  inputs.repeat_frequency.value = 'yearly'
  inputs.start_date.value = '2024-02-29'
  context.updateCalendarCreateForm(form)
  assert.match(summary.textContent, /February 29/)
  inputs.repeat_frequency.value = 'none'
  context.updateCalendarCreateForm(form)
  assert.equal(row.dataset.repeating, 'false', 'Returning to Does not repeat restores full width')
  assert.equal(groups.options.disabled, true)
  assert.equal(groups.until.disabled, true)
  assert.equal(context.validateCalendarCreatePickers(form), true)
  inputs.repeat_frequency.value = 'daily'
  inputs.repeat_end.value = 'count'
  inputs.repeat_interval.value = '1'
  context.updateCalendarCreateForm(form)
  const first = context.submitCalendarCreate(form)
  assert.equal(groups.options.disabled, true, 'Busy state must lock the entire repeat configuration')
  const body = new URLSearchParams(requests[0].options.body)
  assert.equal(body.get('repeat_frequency'), 'daily')
  assert.equal(body.get('repeat_interval'), '1')
  assert.equal(body.get('repeat_count'), '4')
  assert.equal(body.has('repeat_until'), false, 'A previous end date must not conflict with the count')
  requests[0].reject(new Error('lost response'))
  await first
  assert.equal(groups.options.disabled, true, 'Uncertain writes must freeze repeat controls')
  const retry = context.submitCalendarCreate(form)
  assert.equal(requests[1].options.body, requests[0].options.body, 'Retry the identical recurrence and request ID')
  requests[1].resolve({ok: true, json: async () => ({series_id: 'confirmed-series', refresh_pending: true})})
  await retry
  assert.equal(toasts.at(-1).title, 'Recurring event created')
  assert.equal(toasts.at(-1).variant, 'warning')
  assert.match(toasts.at(-1).description, /Series saved.*refresh/)
  assert.equal(closes.length, 1)
  assert.equal(refreshes, 1, 'Use the existing HTMX cache refresh after a confirmed series')
  // Convert an existing event through PATCH, preserving its version and identity.
  form.dataset.calendarEventId = 'existing'
  form.action = '/api/calendar/events/existing'
  form._calendarCreateUncertain = false
  inputs.version = {name: 'version', value: 'version-1', disabled: false}
  context.updateCalendarCreateForm(form)
  assert.equal(groups.options.disabled, false)
  const edit = context.submitCalendarCreate(form)
  const update = requests.at(-1)
  const editBody = new URLSearchParams(update.options.body)
  assert.equal(update.url, '/api/calendar/events/existing')
  assert.equal(update.options.method, 'PATCH', 'Enabling repeat must update, not create a second event')
  assert.equal(editBody.get('version'), 'version-1')
  assert.equal(editBody.get('repeat_frequency'), 'daily')
  assert.equal(editBody.get('repeat_count'), '4')
  update.resolve({ok: true, json: async () => ({saved: true, event_id: 'existing', series_id: 'remote-existing', refresh_pending: false})})
  await edit
  assert.equal(toasts.at(-1).title, 'Event updated')
  assert.equal(toasts.at(-1).variant, 'success')
  assert.equal(refreshes, 2)
  // Whole-series edits explicitly carry the scope and the master's version.
  form.dataset.calendarEditSeries = 'true'
  inputs.edit_scope = {name: 'edit_scope', value: 'series', disabled: false}
  inputs.version.value = 'master-version'
  const seriesEdit = context.submitCalendarCreate(form)
  const seriesBody = new URLSearchParams(requests.at(-1).options.body)
  assert.equal(seriesBody.get('edit_scope'), 'series')
  assert.equal(seriesBody.get('version'), 'master-version')
  requests.at(-1).resolve({ok: true, json: async () => ({saved: true, event_id: 'existing', series_id: 'master'})})
  await seriesEdit
  assert.equal(toasts.at(-1).title, 'Series updated')
  assert.equal(refreshes, 3, 'Series edits refresh the grid and Upcoming through the existing HTMX path')
  const conflict = context.submitCalendarCreate(form)
  requests.at(-1).resolve({ok: false, json: async () => ({conflict: true, error: 'Event changed; reopen it.'})})
  await conflict
  assert.equal(groups.options.disabled, true, 'A stale conversion must lock repeat controls')
  const requestCount = requests.length
  await context.submitCalendarCreate(form)
  assert.equal(requests.length, requestCount, 'Never replay an unconfirmed/stale edit automatically')
  // Stale forms without a repeat section must still initialize safely.
  context.updateCalendarRecurrenceForm({querySelector() { return null }}, false)
  assert(source.includes('updateCalendarRecurrenceForm(form, locked)'), 'Normal initialization and form changes must wire repeat controls')
  console.log('Calendar recurrence: templUI fields, summaries, inclusive dates, creation retries, versioned edit conversion, and series refresh feedback passed.')
}
main().catch(error => { console.error(error); process.exitCode = 1 })
