const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const path = require('node:path')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
function helper(name) {
  const start = source.indexOf('function ' + name + '('), end = source.indexOf('\n}', start)
  assert(start > 0 && end > start, 'Missing helper ' + name)
  return source.slice(start, end + 2)
}
const requests = []
const selected = {dataset: {calendarSourceProvider: 'outlook'}}
const sourceInput = {value: 'first'}
let toggle = {dataset: {calendarTeamsAvailable: 'false'}, disabled: true, checked: false}
const repeat = {value: 'none'}
const root = {
  id: 'calendar-teams-options', dataset: {}, isConnected: true,
  replaceChildren() { toggle = {dataset: {calendarTeamsAvailable: 'false'}, disabled: true, checked: false} },
  querySelectorAll(selector) { return selector.includes('retry') ? [] : [toggle] },
  closest() { return form },
}
const form = {
  isConnected: true,
  querySelector(selector) {
    if (selector === '[data-calendar-teams-options]') return root
    if (selector === '[name="source_id"]') return sourceInput
    if (selector === '[name="teams_meeting"]') return toggle
    if (selector === '[name="repeat_frequency"]') return repeat
    if (selector === '[name="all_day"]') return {checked: true}
    if (selector === '[data-calendar-teams-checking]') return {content: {cloneNode() { return {} }}}
  },
}
let message = ''
const context = vm.createContext({
  window: {htmx: {trigger(root, name) { requests.push({source: sourceInput.value, name}) }}},
  updateCalendarCreateForm(form) { context.updateCalendarTeamsForm(form, selected, false) },
  adjustCalendarCreateAllDayRange() {},
  setCalendarCreateError(form, value) { message = value },
})
vm.runInContext(['updateCalendarTeamsForm', 'retryCalendarTeams', 'calendarTeamsSwapAllowed', 'validateCalendarCreatePickers'].map(helper).join('\n'), context)
context.updateCalendarTeamsForm(form, selected, false)
assert.equal(root.hidden, false)
assert.equal(requests.filter(r => r.name === 'calendar-teams-source-changed').length, 1)
context.updateCalendarTeamsForm(form, selected, false)
assert.equal(requests.filter(r => r.name === 'calendar-teams-source-changed').length, 1, 'Other form changes must not refetch or reset Teams')
toggle.dataset.calendarTeamsAvailable = 'true'
toggle.checked = true
context.updateCalendarTeamsForm(form, selected, false)
assert.equal(toggle.disabled, false)
const detail = {target: root, xhr: {getResponseHeader() { return 'first' }}}
assert.equal(context.calendarTeamsSwapAllowed(detail), true)
sourceInput.value = 'second'
context.updateCalendarTeamsForm(form, selected, false)
assert.equal(toggle.checked, false, 'Changing calendars must clear the previous selection')
assert.equal(context.calendarTeamsSwapAllowed(detail), false, 'Old calendar responses must not overwrite the current control')
detail.xhr.getResponseHeader = () => 'second'
assert.equal(context.calendarTeamsSwapAllowed(detail), true)
for (const lock of ['_calendarCreateBusy', '_calendarCreateUncertain', '_calendarCreateConflict']) {
  form[lock] = true
  context.updateCalendarTeamsForm(form, selected, true)
  assert.equal(toggle.disabled, true)
  assert.equal(context.calendarTeamsSwapAllowed(detail), false, 'Requests must not change a frozen save/retry')
  const count = requests.length
  context.retryCalendarTeams(form)
  assert.equal(requests.length, count)
  form[lock] = false
}
context.updateCalendarTeamsForm(form, selected, false)
selected.dataset.calendarSourceProvider = 'gmail'
context.updateCalendarTeamsForm(form, selected, false)
assert.equal(root.hidden, true)
assert.equal(toggle.checked, false, 'No Teams flag on non-Outlook event')
selected.dataset.calendarSourceProvider = 'outlook'
root.dataset.calendarTeamsSource = 'second'
root.dataset.calendarTeamsState = 'existing'
delete root._calendarTeamsSource
toggle.checked = true
const count = requests.length
context.updateCalendarTeamsForm(form, selected, false)
assert.equal(requests.length, count, 'Known existing Teams meetings do not need a capability request')
assert.equal(toggle.disabled, true, 'Generic form updates must not enable the irreversible existing switch')
root.dataset.calendarTeamsState = ''
context.retryCalendarTeams(form)
assert.equal(requests.at(-1).name, 'calendar-teams-source-changed')
toggle.dataset.calendarTeamsAvailable = 'true'
toggle.checked = true
toggle.disabled = false
toggle.focus = () => {}
repeat.value = 'weekly'
assert.equal(context.validateCalendarCreatePickers(form), false)
assert.match(message, /Teams meetings.*do not repeat/)
form.isConnected = false
assert.equal(context.calendarTeamsSwapAllowed(detail), false)
console.log('Calendar Teams capability, source switching, locking, and recurrence checks passed')
