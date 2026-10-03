const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const handlers = {}, clicks = {}, closed = [], toasts = []
let refreshes = 0
const node = () => ({disabled: false, hidden: true, textContent: '', focus() { this.focused = true }})
const submit = node(), cancel = node(), spinner = node(), label = node(), error = node(), edit = node(), trigger = node(), check = node()
const dialog = {querySelectorAll() { return [edit] }}
const popover = {querySelector() { return trigger }}
const form = {
  dataset: {calendarEventId: 'event-id'}, isConnected: true,
  matches(selector) { return selector === '[data-calendar-delete-form]' },
  closest(selector) { return selector === '[data-tui-dialog]' ? dialog : popover },
  setAttribute(name, value) { this[name] = value },
  querySelectorAll() { return [trigger] },
  querySelector(selector) { return {
    '[data-calendar-delete-submit]': submit, '[data-calendar-delete-spinner]': spinner,
    '[data-calendar-delete-label]': label, '[data-calendar-delete-error]': error,
    '[data-calendar-delete-cancel]': cancel,
    '[data-calendar-delete-check]': check,
  }[selector] },
}
const context = vm.createContext({
  document: {body: {addEventListener(type, handler) { (handlers[type] ||= []).push(handler) }}, addEventListener(type, handler) { clicks[type] = handler }},
  window: {tui: {dialog: {close(root) { closed.push(['dialog', root]) }}, popover: {closeElement(root) { closed.push(['popover', root]) }}}},
  showGoferToast(toast) { toasts.push(toast) }, scheduleCalendarCacheRefresh() { refreshes++ },
})
vm.runInContext(source.slice(source.indexOf('function updateCalendarDeleteForm('), source.indexOf('var _calendarSyncRequest = null')), context)

function fire(type, detail) {
  const event = {detail, prevented: false, preventDefault() { this.prevented = true }}
  for (const handler of handlers[type] || []) handler(event)
  return event
}
function reset() {
  form._calendarDeleteBusy = form._calendarDeleteBlocked = false
  form.isConnected = true
  form.dataset.calendarDeleteReady = 'true'
  form.dataset.calendarDeleteSeries = 'false'
  form.dataset.calendarDeleteOccurrence = 'false'
  closed.length = toasts.length = refreshes = 0
  context.updateCalendarDeleteForm(form)
}
function begin() {
  const detail = {elt: form, xhr: {}}
  assert.equal(fire('htmx:beforeRequest', detail).prevented, false)
  assert.equal(form['aria-busy'], 'true')
  assert.equal(submit.disabled, true)
  assert.equal(cancel.disabled, true, 'Do not imply that an accepted provider deletion can be cancelled')
  assert.equal(edit.disabled, true, 'Do not allow editing during deletion')
  assert.equal(spinner.hidden, false)
  assert.equal(label.textContent, 'Deleting…')
  assert.equal(fire('htmx:beforeRequest', {elt: form, xhr: {}}).prevented, true, 'Prevent repeated submissions')
  return detail
}
function finish(detail, data, successful = true) {
  detail.xhr.responseText = typeof data === 'string' ? data : JSON.stringify(data)
  fire('htmx:afterRequest', {...detail, successful})
  assert.equal(form['aria-busy'], 'false')
  assert.equal(spinner.hidden, true)
  assert.equal(cancel.disabled, false)
}

reset()
const success = begin()
finish(success, {deleted: true, event_id: 'event-id'})
assert.deepEqual(closed, [['popover', form], ['dialog', dialog]])
assert.equal(toasts[0].title, 'Event deleted')
assert.equal(refreshes, 1, 'Refresh the grid and Upcoming via the existing HTMX cache refresh')
assert.equal(submit.disabled, true, 'A successful request cannot be resubmitted during the close animation')
fire('htmx:afterRequest', {...success, successful: true})
assert.equal(refreshes, 1, 'A detached HTMX trigger must not process the same response twice')

for (const result of [{uncertain: true}, {conflict: true}, 'invalid JSON', {deleted: true, event_id: 'different-event'}]) {
  reset()
  const request = begin()
  const ok = !result.uncertain && !result.conflict
  finish(request, result, ok)
  assert.equal(form._calendarDeleteBlocked, true)
  assert.equal(submit.disabled, true)
  assert.equal(edit.disabled, true)
  assert.equal(error.hidden, false)
  assert.equal(error.focused, true)
  assert.equal(closed.length + toasts.length + refreshes, 0, 'Never remove an unconfirmed event from the view')
  assert.equal(fire('htmx:beforeRequest', {elt: form, xhr: {}}).prevented, true)
}

reset()
finish(begin(), {error: 'Write access denied.', uncertain: false, conflict: false}, false)
assert.equal(error.textContent, 'Write access denied.')
assert.equal(submit.disabled, false, 'A definite failure may be retried')
assert.equal(edit.disabled, false)

reset()
const detached = begin()
form.isConnected = false
finish(detached, {deleted: true, event_id: 'event-id'})
assert.equal(closed.length, 0, 'A delayed deletion must not close a newer dialog or popover')
assert.equal(refreshes, 1)

reset()
clicks.click({target: {closest() { return {closest() { return popover }} }}})
assert.deepEqual(closed, [['popover', popover]], 'Cancel dismisses only the confirmation without any request')
assert.equal(trigger.focused, true)
assert.equal(form._calendarDeleteBusy, false)
assert.equal(refreshes, 0)

// Series confirmation is a read-only HTMX load inside the existing popover.
reset()
form.dataset.calendarDeleteSeries = 'true'
form.dataset.calendarDeleteSeriesId = 'master-id'
const loader = {
  matches(selector) { return selector === '[data-calendar-delete-load]' },
  closest() { return {querySelector() { return form }} },
}
let load = {elt: loader, xhr: {}}
fire('htmx:beforeRequest', load)
assert.equal(form.dataset.calendarDeleteReady, 'false')
assert.equal(check.hidden, false)
assert.equal(submit.disabled, true, 'Deletion must wait for the master version')
assert.equal(cancel.disabled, false, 'Loading confirmation is still cancellable')
assert.equal(label.textContent, 'Delete series')
assert.equal(fire('htmx:beforeRequest', {elt: form, xhr: {}}).prevented, true)
load.xhr.status = 503
fire('htmx:afterRequest', {...load, successful: false})
assert.equal(check.hidden, true)
assert.match(error.textContent, /Could not check the selection/)
assert.equal(submit.disabled, true, 'A failed read must not enable deletion with an older version')
assert.equal(closed.length + toasts.length + refreshes, 0)
load = {elt: loader, xhr: {}}
fire('htmx:beforeRequest', load)
form.isConnected = false
const swap = {xhr: load.xhr, shouldSwap: true}
fire('htmx:beforeSwap', swap)
assert.equal(swap.shouldSwap, false, 'Late confirmation must not modify a newer dialog')
fire('htmx:afterRequest', {...load, successful: true})
form.isConnected = true
form.dataset.calendarDeleteReady = 'true' // New, server-rendered confirmation.
context.updateCalendarDeleteForm(form)
const seriesRequest = begin()
assert.equal(fire('htmx:beforeRequest', {elt: loader, xhr: {}}).prevented, true, 'Cannot reload confirmation during deletion')
finish(seriesRequest, {deleted: true, event_id: 'event-id', series_id: 'master-id'})
assert.equal(toasts.at(-1).title, 'Series deleted')
assert.match(toasts.at(-1).description, /All occurrences/)
assert.equal(refreshes, 1)
for (const seriesID of [undefined, 'different-master']) {
  reset()
  form.dataset.calendarDeleteSeries = 'true'
  finish(begin(), {deleted: true, event_id: 'event-id', series_id: seriesID})
  assert.equal(form._calendarDeleteBlocked, true, 'Require confirmation of the selected series, not just an occurrence')
  assert.equal(refreshes + toasts.length, 0)
}
reset()
form.dataset.calendarDeleteOccurrence = 'true'
finish(begin(), {deleted: true, event_id: 'event-id', scope: 'occurrence'})
assert.match(toasts.at(-1).description, /Only this occurrence/)
assert.equal(refreshes, 1)
for (const scope of [undefined, 'series']) {
  reset()
  form.dataset.calendarDeleteOccurrence = 'true'
  finish(begin(), {deleted: true, event_id: 'event-id', scope})
  assert.equal(form._calendarDeleteBlocked, true)
  assert.equal(refreshes, 0, 'Do not accept a different mutation scope')
}
reset()
const oldScope = {elt: loader, xhr: {}}
const latestScope = {elt: loader, xhr: {}}
fire('htmx:beforeRequest', oldScope)
fire('htmx:beforeRequest', latestScope)
const oldSwap = {xhr: oldScope.xhr, shouldSwap: true}
fire('htmx:beforeSwap', oldSwap)
assert.equal(oldSwap.shouldSwap, false, 'An older scope response cannot replace the latest choice')
fire('htmx:afterRequest', {...oldScope, successful: false})
assert.equal(form._calendarDeleteLoader, latestScope.xhr)
assert.equal(submit.disabled, true)
console.log('Calendar delete: scope selection, stale loaders, cancellation, busy state, conflicts, detached responses, and HTMX refresh passed.')
