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
const callbacks = new Map(), timers = [], requests = []
const node = () => ({textContent: '', className: '', parentElement: {className: ''}})
const status = node(), label = node(), dot = node(), detail = node()
const icons = ['pending', 'syncing', 'ok', 'failed'].map(state => ({dataset: {calendarSourceSyncIcon: state}, hidden: false}))
const indicator = {
  dataset: {calendarSourceSync: 'personal', calendarSyncState: 'pending', calendarSyncAttempt: '0'},
  querySelectorAll() { return icons }, setAttribute(name, value) { this[name] = value },
}
const button = {disabled: false, setAttribute(name, value) { this[name] = value }, querySelector() { return null }}
const selectors = {
  '[data-calendar-source-sync]': [indicator], '[data-calendar-sync-status]': [status],
  '[data-calendar-sync-label]': [label], '[data-calendar-sync-dot]': [dot],
  '[data-calendar-sync-detail]': [detail], '[data-calendar-sync-button]': [button],
}
const trigger = {}, agenda = {scrollTop: 217}, week = {scrollTop: 643, scrollLeft: 28}
let calendar = {
  dataset: {calendarPeriod: 'week:2026-09-28', calendarTimezone: 'Europe/Prague', calendarSyncSources: JSON.stringify([
    {source_id: 'personal', state: 'pending', attempt: 0, last_synced_at: ''},
  ])},
  loading: false,
  hasAttribute(name) { return name === 'data-calendar-loading' && this.loading },
  querySelector(selector) { return selector === '[data-calendar-cache-refresh]' ? trigger : week },
}
const context = vm.createContext({
  document: {
    readyState: 'loading', addEventListener() {},
    getElementById(id) { return id === 'calendar-main' ? calendar : id === 'calendar-agenda-list' ? agenda : null },
    querySelectorAll(selector) { return selectors[selector] || [] },
    body: {addEventListener(name, callback) { if (!callbacks.has(name)) callbacks.set(name, []); callbacks.get(name).push(callback) }},
  },
  setTimeout(callback) { timers.push(callback); return timers.length },
  window: {htmx: {trigger(element, name) { requests.push({element, name}) }}},
})
const start = source.indexOf('var _calendarSyncRequest = null')
const end = source.indexOf('\nfunction configureCalendarSyncRequest(', start)
assert(start > 0 && end > start)
vm.runInContext('var _calendarContentRequest = null; var _calendarWeekZoom = 2; var _calendarWeekScrollState = null;\n' +
  helper('_setCalendarSyncBusy') + '\n' + source.slice(start, end), context)
function emit(name, detail) { (callbacks.get(name) || []).forEach(callback => callback({detail})) }
function flush() { const pending = timers.splice(0); pending.forEach(callback => callback()) }
function event(state, attempt, extra = {}) {
  context.handleCalendarSyncEvent({source_id: 'personal', state, attempt, last_synced_at: '2026-10-02T10:00:00Z', ...extra})
}

context.updateCalendarSyncPresentation()
assert.equal(label.textContent, 'Calendar sync pending', 'Cache alone must not claim a successful sync')
event('syncing', 1, {last_synced_at: ''})
assert.equal(label.textContent, 'Refreshing calendars')
assert.equal(button.disabled, true)
assert.equal(icons.find(icon => !icon.hidden).dataset.calendarSourceSyncIcon, 'syncing')
event('ok', 1, {changed: true})
assert.equal(label.textContent, 'Calendar synchronized')
assert.equal(button.disabled, false)
assert.match(status.textContent, /^Last synced 12:00$/)
assert.match(dot.className, /bg-emerald-500/)
event('syncing', 1)
event('failed', 0)
assert.equal(label.textContent, 'Calendar synchronized', 'Stale page/snapshot must not rewind a completed attempt')
event('ok', 1, {changed: true})
flush()
assert.equal(requests.length, 1, 'Concurrent source completion events must coalesce cache reads')
assert.equal(requests[0].name, 'calendar-cache-refresh')

event('failed', 2, {error: 'Provider unavailable', next_attempt_at: '2026-10-02T10:05:00Z'})
assert.equal(label.textContent, 'Calendar sync needs attention')
assert.match(indicator.title, /Provider unavailable.*Cached events remain available.*Retrying at 12:05/)
assert.match(dot.className, /bg-destructive/)
assert.equal(requests.length, 1, 'Failure must not replace the cached view')
event('syncing', 3)
event('ok', 3, {snapshot: true, last_synced_at: '2026-10-02T10:07:00Z'})
flush()
assert.equal(requests.length, 2, 'Reconnect snapshot must recover a missed successful update')

context._calendarContentRequest = {}
event('ok', 4, {changed: true})
flush()
assert.equal(requests.length, 2, 'Updates during navigation must wait')
assert.equal(context._calendarCacheRefreshPending, true)
context._calendarContentRequest = null
emit('htmx:afterSwap', {})
flush()
assert.equal(requests.length, 3, 'Deferred updates must resume once navigation settles')

const xhr = {}
context.configureCalendarCacheRequest({detail: {xhr}})
assert.equal(xhr.goferCalendarCachePeriod, 'week:2026-09-28')
const swap = {xhr, shouldSwap: true}
emit('htmx:beforeSwap', swap)
assert.equal(swap.shouldSwap, true)
assert.equal(xhr.goferCalendarAgendaScroll, 217)
assert.equal(context._calendarWeekScrollState.top, 643)
assert.equal(context._calendarWeekScrollState.left, 28)
assert.equal(context._calendarWeekScrollState.zoom, 2)
agenda.scrollTop = 0
emit('htmx:afterSwap', {xhr})
assert.equal(agenda.scrollTop, 217, 'Passive refresh must restore agenda scroll')
event('ok', 5, {changed: true})
flush()
assert.equal(requests.length, 3, 'Only one cache request may run at a time')
emit('htmx:afterRequest', {xhr})
flush()
assert.equal(requests.length, 4, 'Changes arriving during a cache read must not be lost')

context.configureCalendarCacheRequest({detail: {xhr}})
calendar.dataset.calendarPeriod = 'week:2026-10-05'
const stale = {xhr, shouldSwap: true}
emit('htmx:beforeSwap', stale)
assert.equal(stale.shouldSwap, false, 'A late update must not replace a newly chosen period')
calendar.loading = true
assert.equal(!!context.calendarCacheResponseCurrent(xhr), false)
emit('htmx:sendAbort', {xhr})
assert.equal(context._calendarCacheRequest, null)

calendar = null
event('ok', 6, {changed: true})
flush()
assert.equal(requests.length, 4, 'Calendar updates must not swap Mail or Contacts')
assert.equal(context._calendarSyncStates.get('personal').attempt, 6, 'Status remains current while another app is open')
assert.match(helper('showCalendarContentPending'), /data-calendar-cache-refresh/, 'Passive updates must not introduce a loading skeleton')
console.log('Calendar background sync: real status, source versions, recovery snapshots, coalesced cache reads, navigation guards, and scroll preservation passed.')
