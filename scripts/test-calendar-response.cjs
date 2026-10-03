const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const handlers = {}, closed = [], toasts = [], deliveryRequests = []
let refreshes = 0
const node = () => ({disabled: false, hidden: true, textContent: '', focus() { this.focused = true }})
const choices = [node(), node(), node()], menuTrigger = node(), scope = node(), error = node(), progress = node()
const dialog = {open: true, hasAttribute() { return false }}
const form = {
  dataset: {calendarEventId: 'event-id', calendarResponseScope: 'occurrence', calendarResponseReady: 'true'}, isConnected: true,
  matches(selector) { return selector === '[data-calendar-response-form]' },
  closest() { return dialog },
  setAttribute(name, value) { this[name] = value },
  querySelectorAll(selector) { return selector === '[data-calendar-response-choice], [data-calendar-response-trigger]' ? [...choices, menuTrigger] : [scope] },
  querySelector(selector) { return selector === '[data-calendar-response-progress]' ? progress : error },
}
const loader = {matches(selector) { return selector === '[data-calendar-response-load]' }, closest() { return form }}
const context = vm.createContext({
  document: {body: {addEventListener(type, handler) { (handlers[type] ||= []).push(handler) }}},
  window: {tui: {dialog: {close(root) { closed.push(root) }}}, htmx: {ajax(method, url, options) { deliveryRequests.push({method, url, options}); return {catch() {}} }}},
  showGoferToast(toast) { toasts.push(toast) }, scheduleCalendarCacheRefresh() { refreshes++ },
})
vm.runInContext(source.slice(source.indexOf('function updateCalendarResponseForm('), source.indexOf('function updateCalendarDeleteForm(')), context)
function fire(type, detail) {
  const event = {detail, prevented: false, preventDefault() { this.prevented = true }}
  for (const handler of handlers[type] || []) handler(event)
  return event
}
function reset() {
  form._calendarResponseBusy = form._calendarResponseBlocked = false
  delete form._calendarResponseLoader
  form.isConnected = dialog.open = true
  form.dataset.calendarResponseReady = 'true'
  progress.textContent = 'Only this occurrence. The organizer will be notified.'
  closed.length = toasts.length = refreshes = 0
  deliveryRequests.length = 0
  context.updateCalendarResponseForm(form)
}
function begin() {
  const detail = {elt: form, xhr: {}, requestConfig: {parameters: {response: 'accepted'}}}
  assert.equal(fire('htmx:beforeRequest', detail).prevented, false)
  assert.equal(form['aria-busy'], 'true')
  assert.ok(choices.every(button => button.disabled))
  assert.equal(menuTrigger.disabled, true, 'Response dropdown is locked while sending')
  assert.equal(scope.disabled, true, 'Scope cannot change while sending a response')
  assert.equal(progress.textContent, 'Sending your response…')
  assert.equal(fire('htmx:beforeRequest', {...detail, xhr: {}}).prevented, true)
  return detail
}
function finish(detail, data, successful = true) {
  detail.xhr.responseText = typeof data === 'string' ? data : JSON.stringify(data)
  fire('htmx:afterRequest', {...detail, successful})
  assert.equal(form['aria-busy'], 'false')
}
const result = {responded: true, pending: false, event_id: 'event-id', scope: 'occurrence', response: 'accepted'}
reset()
const success = begin()
finish(success, result)
assert.deepEqual(closed, [dialog])
assert.equal(toasts[0].title, 'Response saved')
assert.equal(refreshes, 1, 'Refresh Calendar and Upcoming through existing HTMX path')
assert.ok(choices.every(button => button.disabled), 'Cannot send twice during modal close')
fire('htmx:afterRequest', {...success, successful: true})
assert.equal(refreshes, 1, 'Handle a detached HTMX request exactly once')

for (const bad of [{...result, event_id: 'different'}, {...result, scope: 'series'}, {...result, response: 'declined'}, {...result, pending: true}, {responded: false}, 'malformed JSON']) {
  reset(); finish(begin(), bad)
  assert.equal(form._calendarResponseBlocked, true)
  assert.equal(error.hidden, false)
  assert.equal(error.focused, true)
  assert.equal(closed.length + toasts.length + refreshes, 0, 'Never show an unconfirmed result as success')
}
for (const data of [{uncertain: true}, {conflict: true}, {error: 'Access denied'}]) {
  reset(); finish(begin(), data, false)
  assert.equal(form._calendarResponseBlocked, !!data.uncertain || !!data.conflict)
  assert.equal(choices[0].disabled, form._calendarResponseBlocked)
  assert.equal(menuTrigger.disabled, form._calendarResponseBlocked, 'Dropdown follows retry/conflict state')
}
reset(); finish(begin(), {...result, responded: false, pending: true})
assert.equal(toasts[0].title, 'Response submitted')
assert.equal(toasts[0].variant, 'warning', 'A Graph acknowledgement is not a confirmed response')
assert.equal(form._calendarResponseBlocked, true)
reset(); finish(begin(), {...result, responded: false, pending: true, delivery: 'email', delivery_id: '12345678-abcd-1234-abcd-123456789012'})
assert.equal(closed.length, 0, 'Keep the invitation open while email delivery progresses')
assert.equal(deliveryRequests.length, 1)
assert.equal(deliveryRequests[0].url, '/api/calendar/replies/12345678-abcd-1234-abcd-123456789012')
assert.equal(deliveryRequests[0].options.target, dialog, 'Bind HTMX to the existing target element, not a future dialog ID')
assert.equal(toasts[0].title, 'Reply queued')
assert.equal(refreshes, 0, 'Queued email is not a confirmed calendar change')
reset(); const closedEmail = begin(); form.isConnected = false; finish(closedEmail, {...result, responded: false, pending: true, delivery: 'email', delivery_id: '12345678-abcd-1234-abcd-123456789012'})
assert.equal(deliveryRequests.length, 0, 'Do not replace a newer dialog with an old reply status')
reset(); finish(begin(), {...result, responded: false, pending: true, delivery: 'email', delivery_id: '../unsafe'})
assert.equal(error.hidden, false)
assert.equal(deliveryRequests.length, 0)
reset(); const detached = begin(); form.isConnected = false; finish(detached, result)
assert.equal(closed.length, 0, 'Delayed reply must not close a newer event')
assert.equal(refreshes, 1)

reset()
const load = {elt: loader, xhr: {}}
fire('htmx:beforeRequest', load)
assert.equal(form.dataset.calendarResponseReady, 'false')
assert.equal(choices[0].disabled, true)
assert.equal(menuTrigger.disabled, true, 'Dropdown stays disabled until the invitation is ready')
assert.equal(scope.disabled, false, 'Allow choosing a different scope during a read')
const newerLoad = {elt: loader, xhr: {}}
fire('htmx:beforeRequest', newerLoad)
const staleSwap = {xhr: load.xhr, shouldSwap: true}
fire('htmx:beforeSwap', staleSwap)
assert.equal(staleSwap.shouldSwap, false)
fire('htmx:afterRequest', {...load, successful: false})
assert.equal(form._calendarResponseLoader, newerLoad.xhr)
newerLoad.xhr.status = 409; newerLoad.xhr.responseText = 'Invitation changed.'
fire('htmx:afterRequest', {...newerLoad, successful: false})
assert.equal(error.textContent, 'Invitation changed.')
assert.equal(choices[0].disabled, true, 'A failed scope load must not use the previous version')
assert.equal(form['aria-busy'], 'false')
for (const closing of ['detached', 'closed']) {
  reset(); const pending = {elt: loader, xhr: {}}; fire('htmx:beforeRequest', pending)
  if (closing === 'detached') form.isConnected = false; else dialog.open = false
  const swap = {xhr: pending.xhr, shouldSwap: true}; fire('htmx:beforeSwap', swap)
  assert.equal(swap.shouldSwap, false)
}
console.log('Calendar RSVP feedback, scope, retries, and HTMX refresh checks passed')
