const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
function helper(name) {
  const start = source.indexOf('function ' + name + '('), end = source.indexOf('\n}', start)
  assert(start > 0 && end > start, 'Missing helper ' + name)
  return source.slice(start, end + 2)
}
const inputs = ['source_id', 'request_id', 'summary', 'start_date', 'end_date', 'start_time', 'end_time', 'timezone', 'all_day'].map(name => ({name, value: name, disabled: false, required: false, checked: false}))
const sourceInput = inputs[0]
const sourceChoice = {dataset: {tuiSelectboxValue: sourceInput.value, tuiSelectboxDisabled: 'false', calendarSourceWritable: 'true', calendarSourceAuthorized: 'true'}}
const sourceTrigger = {disabled: false}
const choices = [sourceChoice]
const sourceSelect = {querySelectorAll() { return choices }, querySelector() { return sourceTrigger }}
const node = () => ({hidden: false, textContent: '', setAttribute(name, value) { this[name] = value }})
const error = node(), access = node(), submit = node(), spinner = node(), label = node(), timezone = node(), help = node()
const times = inputs.filter(input => ['start_time', 'end_time'].includes(input.name)).map(input => ({hidden: false, querySelector() { return input }}))
const form = {
  isConnected: true, reportValidity() { return true }, setAttribute(name, value) { this[name] = value },
  querySelector(selector) {
    const name = selector.match(/^\[name="([^"]+)"\]$/)?.[1]
    if (name) return inputs.find(input => input.name === name)
    if (selector === '[data-calendar-create-source-select]') return sourceSelect
    return {'[data-calendar-create-error]': error, '[data-calendar-create-access]': access, '[data-calendar-create-submit]': submit,
      '[data-calendar-create-spinner]': spinner, '[data-calendar-create-submit-label]': label, '[data-calendar-create-timezone]': timezone, '[data-calendar-create-date-help]': help}[selector]
  }, querySelectorAll(selector) { return selector === 'input, select, textarea' ? inputs : times },
}
const requests = [], toasts = [], closed = []
let refreshes = 0
const calendar = {dataset: {calendarPeriod: 'week:2026-09-28', calendarView: 'week', calendarDate: '2026-10-02', calendarTodayDate: '2026-10-02', calendarMonth: '2026-10'}}
const context = vm.createContext({
  document: {getElementById() { return calendar }}, URLSearchParams,
  FormData: class {constructor() { this.entries = inputs.filter(input => !input.disabled && input.name !== 'all_day').map(input => [input.name, input.value]) } [Symbol.iterator]() { return this.entries[Symbol.iterator]() }},
  window: {crypto: {randomUUID() { return 'fresh-request' }}, tui: {dialog: {close(id) { closed.push(id) }}}},
  fetch(url, options) { return new Promise((resolve, reject) => requests.push({url, options, resolve, reject})) },
  showGoferToast(data) { toasts.push(data) }, scheduleCalendarCacheRefresh() { refreshes++ },
})
vm.runInContext('var _calendarSelectedDay = {period: "week:2026-09-28", date: "2026-10-03"};\n' +
  ['configureCalendarCreateDialog', 'updateCalendarCreateForm', 'setCalendarCreateError', 'submitCalendarCreate'].map(helper).join('\n'), context)
async function main() {
  const event = {detail: {parameters: {}}}
  context.configureCalendarCreateDialog(event)
  assert.equal(event.detail.parameters.date, '2026-10-03', 'New event must use the selected day')
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, false)
  assert.equal(sourceTrigger.disabled, false)
  sourceChoice.dataset.calendarSourceAuthorized = 'false'
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, true, 'A read-only OAuth grant must not be writable')
  assert.match(access.textContent, /Reconnect.*Accounts/)
  sourceChoice.dataset.calendarSourceAuthorized = 'true'
  sourceChoice.dataset.tuiSelectboxDisabled = 'true'
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, true, 'Read-only templUI items must not enable creation')
  sourceChoice.dataset.tuiSelectboxDisabled = 'false'
  sourceInput.value = ''
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, true, 'An empty templUI selection must not enable creation')
  choices.length = 0
  context.updateCalendarCreateForm(form)
  assert.equal(sourceTrigger.disabled, true, 'A selector without configured calendars must remain disabled')
  choices.push(sourceChoice)
  sourceInput.value = sourceChoice.dataset.tuiSelectboxValue
  inputs.find(input => input.name === 'all_day').checked = true
  context.updateCalendarCreateForm(form)
  assert.ok(times.every(node => node.hidden && node.querySelector().disabled))
  assert.equal(timezone.hidden, true)
  assert.equal(help.hidden, false)
  inputs.find(input => input.name === 'all_day').checked = false
  const first = context.submitCalendarCreate(form)
  context.submitCalendarCreate(form)
  assert.equal(requests.length, 1, 'Double submit must be ignored')
  assert.equal(label.textContent, 'Creating…')
  assert.equal(spinner.hidden, false)
  assert.ok(inputs.every(input => input.disabled))
  assert.equal(sourceTrigger.disabled, true, 'The templUI trigger must be locked while saving')
  const stablePayload = requests[0].options.body
  requests[0].reject(new Error('lost response'))
  await first
  assert.equal(label.textContent, 'Retry safely')
  assert.equal(submit.disabled, false)
  assert.ok(inputs.every(input => input.disabled), 'Uncertain writes must keep the same draft')
  assert.equal(sourceTrigger.disabled, true, 'Safe retry must not allow another calendar to be selected')
  assert.match(error.textContent, /duplicate/)
  const retry = context.submitCalendarCreate(form)
  assert.equal(requests[1].options.body, stablePayload, 'Retry must use the exact request ID and details')
  requests[1].resolve({ok: true, json: async () => ({event_id: 'created', hidden: true})})
  await retry
  assert.deepEqual(closed, ['calendar-create-dialog'])
  assert.equal(refreshes, 1)
  assert.equal(toasts[0].variant, 'success')
  assert.match(toasts[0].description, /hidden calendar/)

  form._calendarCreateUncertain = false
  context.updateCalendarCreateForm(form)
  const rejected = context.submitCalendarCreate(form)
  requests[2].resolve({ok: false, json: async () => ({error: 'Write access denied', uncertain: false})})
  await rejected
  assert.equal(inputs.find(input => input.name === 'request_id').value, 'fresh-request')
  assert.equal(inputs.find(input => input.name === 'summary').disabled, false, 'Definitive failure must keep an editable draft')
  assert.equal(sourceTrigger.disabled, false, 'Definitive failure must unlock the templUI selector')
  assert.equal(error.textContent, 'Write access denied')
  assert.equal(toasts.length, 1, 'Failed requests must not announce success')

  form.isConnected = false
  const detached = context.submitCalendarCreate(form)
  requests[3].resolve({ok: true, json: async () => ({event_id: 'another'})})
  await detached
  assert.equal(closed.length, 1, 'Late save must not close a newer dialog')
  assert.match(source, /source\.addEventListener\("calendar-changed"/, 'Other sessions need cache change events')
  console.log('Calendar creation: selected-date prefill, permissions, all-day fields, busy feedback, duplicate protection, safe retry, hidden sources, and detached dialogs passed.')
}
main().catch(error => { console.error(error); process.exitCode = 1 })
