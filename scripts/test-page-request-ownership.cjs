const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const end = source.indexOf('document.addEventListener("DOMContentLoaded"')
assert(end > 0, 'Request ownership must be installed before DOMContentLoaded initialization')

function surface() {
  const listeners = new Map()
  return {
    addEventListener(name, callback, capture) {
      if (!listeners.has(name)) listeners.set(name, [])
      listeners.get(name).push({callback, capture})
    },
    emit(name, detail) {
      const event = {detail, defaultPrevented: false, preventDefault() { this.defaultPrevented = true }}
      for (const listener of listeners.get(name) || []) {
        assert.equal(listener.capture, true, 'Ownership checks must run before bubbling swap handlers')
        listener.callback(event)
      }
      return event
    },
    listeners,
  }
}
function harness() {
  const document = surface(), window = surface()
  const context = vm.createContext({document, window})
  vm.runInContext(source.slice(0, end), context)
  return {
    document, window, context,
    request(attributes = {}, target = 'main-content', verb = 'get') {
      const xhr = {}
      document.emit('htmx:beforeRequest', {
        xhr, elt: {hasAttribute(name) { return Object.hasOwn(attributes, name) }},
        target: {id: target}, requestConfig: {verb},
      })
      return xhr
    },
    response(xhr, name = 'htmx:beforeSwap') {
      return document.emit(name, {xhr, shouldSwap: true, target: {id: 'main-content'}})
    },
  }
}

// No DOMContentLoaded callbacks have run: reproduce initial HTMX load requests.
for (const from of ['mail', 'contacts', 'calendar']) {
  for (const to of ['mail', 'contacts', 'calendar']) {
    if (from === to) continue
    const h = harness()
    const initial = h.request()
    const dialog = h.request({}, 'app-pane-dialogs')
    const navigation = h.request({'data-sidebar-app-button': to, href: '/' + to}, 'mail-list')
    assert.equal(h.response(navigation).defaultPrevented, false)
    for (const late of [initial, dialog]) {
      for (const event of ['htmx:beforeOnLoad', 'htmx:beforeSwap']) {
        const response = h.response(late, event)
        assert.equal(response.defaultPrevented, true, `${from} must not overwrite ${to} through ${event}`)
        assert.equal(response.detail.shouldSwap, false)
      }
    }
    const current = h.request()
    assert.equal(h.response(current).defaultPrevented, false, 'Current-page background refresh must still work')
    h.request({'data-sidebar-app-button': from}, 'mail-list')
    assert.equal(h.response(initial).defaultPrevented, true, 'Returning to the same app must not revive old requests')
    assert.equal(h.response(navigation).defaultPrevented, true, 'Rapid navigation must discard superseded tab responses')
  }
}

const settings = harness()
const initialSettings = settings.request({}, 'settings-content')
const nextSettings = settings.request({href: '/settings/appearance'}, 'settings-content')
assert.equal(settings.response(initialSettings).defaultPrevented, true, 'Settings section changes also invalidate previous-page requests')
assert.equal(settings.response(nextSettings).defaultPrevented, false)
const saved = settings.request({href: '/settings/appearance'}, 'settings-content', 'post')
assert.equal(settings.response(nextSettings).defaultPrevented, false, 'Saving must not be mistaken for navigation')
assert.equal(settings.response(saved).defaultPrevented, false)

const period = harness()
const refresh = period.request()
const nextPeriod = period.request({href: '/calendar?view=week&date=2026-10-12'})
assert.equal(period.response(refresh).defaultPrevented, true, 'Same-app page navigation invalidates previous-page refreshes')
assert.equal(period.response(nextPeriod).defaultPrevented, false)
period.window.emit('popstate', {})
assert.equal(period.response(nextPeriod).defaultPrevented, true, 'History navigation invalidates pending responses')
assert.equal(period.response(period.request()).defaultPrevented, false)

// Responses without a tracked request (e.g. synthetic swaps) remain untouched.
assert.equal(period.response({}).defaultPrevented, false)
vm.runInContext('setupPageRequestOwnership()', period.context)
assert.equal(period.document.listeners.get('htmx:beforeRequest').length, 1, 'Guard installation must be idempotent')
console.log('Page request ownership: early load triggers, all app pairs, stale dialogs, current refreshes, rapid/return navigation, settings, periods, and history passed.')
