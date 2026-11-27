const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const loadI18n = require('./i18n-test-helper.cjs')

// The calendar color picker: Color… in a calendar's menu opens it at the
// calendar's dot, a color picked is saved at once, and the dot, the menu and
// the calendar's events take it.
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('var calendarColorTarget = null')
const end = source.indexOf('// Import events… and Export calendar… open their dialogs', start)
assert(start > 0 && end > start, 'Missing the calendar color picker')

function element(attributes = {}, extra = {}) {
  return Object.assign({
    attributes: {...attributes}, hidden: false, disabled: false, textContent: '', value: '', style: {},
    classes: new Set(),
    classList: {toggle(name, on) { on ? this.owner.classes.add(name) : this.owner.classes.delete(name) }},
    getAttribute(name) { return name in this.attributes ? this.attributes[name] : null },
    setAttribute(name, value) { this.attributes[name] = String(value) },
    hasAttribute(name) { return name in this.attributes },
    closest() { return null },
  }, extra)
}
function owned(node) { node.classList.owner = node; return node }

const options = ['#ef4444', '#2563eb'].map(color => owned(element({'data-color-picker-option': color})))
const notes = ['google_access', 'provider'].map(note => owned(element({'data-calendar-color-note': note})))
const name = owned(element()), preview = owned(element()), custom = owned(element()), status = owned(element()), code = owned(element())
const reset = owned(element({'data-calendar-color-reset': ''}))
const allow = owned(element({'data-calendar-color-allow-access': ''}))
const picker = owned(element({'data-calendar-color-picker': ''}, {
  querySelector(selector) {
    return {
      '[data-color-picker-name]': name, '[data-calendar-color-preview]': preview, '[data-color-picker-custom]': custom,
      '[data-calendar-color-reset]': reset, '[data-color-picker-status]': status, '[data-color-picker-code]': code,
    }[selector] || null
  },
  querySelectorAll(selector) {
    if (selector === '[data-color-picker-option]') return options
    if (selector === '[data-calendar-color-note]') return notes
    if (selector.startsWith('[data-color-picker-option], ')) return [...options, custom, reset]
    return []
  },
}))
// Everything in the picker finds the picker, and itself by its own attribute.
for (const node of [...options, reset, allow, custom]) {
  node.closest = selector => selector === '[data-calendar-color-picker]' ? picker
    : Object.keys(node.attributes).some(attribute => selector === '[' + attribute + ']') ? node : null
}
custom.attributes['data-color-picker-custom'] = ''
custom.closest = selector => selector === '[data-calendar-color-picker] [data-color-picker-custom]' || selector === '[data-color-picker-custom]' ? custom : null
custom.parentElement = {querySelector: selector => selector === '[data-color-picker-code]' ? code : null}

const dot = owned(element({'data-calendar-color-dot': 'club'}))
const item = owned(element({'data-calendar-source-color': 'club'}, {
  dataset: {
    calendarSourceColor: 'club', calendarSourceName: 'Book club', calendarColor: '#16a765',
    calendarColorInGofer: 'false', calendarColorNote: 'google_access',
    calendarAccountEmail: 'alex@example.com', calendarAccountName: 'Alex',
  },
}))
item.closest = selector => selector === '[data-calendar-source-color]' ? item : null

const listeners = {click: [], change: [], input: []}, timers = [], requests = [], popovers = [], offers = []
let refreshes = 0, answer = null
const context = vm.createContext({
  document: {
    addEventListener(type, callback) { listeners[type].push(callback) },
    querySelector(selector) { return selector === '[data-calendar-color-picker]' ? picker : null },
    querySelectorAll(selector) {
      return {'[data-calendar-color-dot]': [dot], '[data-calendar-source-color]': [item]}[selector] || []
    },
  },
  window: {tui: {popover: {open(id, anchor) { popovers.push(['open', id, anchor]) }, close(id) { popovers.push(['close', id]) }}}},
  setTimeout(callback) { timers.push(callback); return timers.length },
  fetch(url, options) { requests.push({url, options}); return Promise.resolve(answer()) },
  scheduleCalendarCacheRefresh() { refreshes++ },
  offerFullCalendarAccess(data, action) { offers.push([data.calendarSourceName, data.calendarAccountEmail, action]) },
  encodeURIComponent, JSON, Object,
})
loadI18n(context)
vm.runInContext(source.slice(start, end), context)

function fire(type, target) {
  const event = {target, defaultPrevented: false, preventDefault() { this.defaultPrevented = true }}
  listeners[type].forEach(callback => callback(event))
  return event
}
function flush() { timers.splice(0).forEach(callback => callback()) }
const settle = () => new Promise(resolve => setImmediate(resolve))
function respond(body, ok = true) {
  answer = () => ({ok, json: () => Promise.resolve(body), text: () => Promise.resolve(body)})
}
const shown = () => notes.filter(n => !n.hidden).map(n => n.getAttribute('data-calendar-color-note'))

;(async () => {
  // Opening it paints the calendar, then opens at the dot once the menu's
  // click is over.
  assert.equal(fire('click', item).defaultPrevented, true)
  assert.equal(name.textContent, 'Book club')
  assert.equal(preview.style.backgroundColor, '#16a765')
  assert.deepEqual(options.map(o => o.getAttribute('data-account-color-active')), ['false', 'false'])
  assert.deepEqual(shown(), ['google_access'], 'the note for a Google account without full access')
  assert.equal(reset.hidden, true, 'nothing to go back to yet')
  assert.equal(custom.value, '#16a765')
  assert.equal(code.textContent, '#16a765', 'the color code shows the color')
  assert.deepEqual(popovers, [])
  flush()
  assert.deepEqual(popovers, [['open', 'calendar-color-popover', dot]])

  // A color is saved at once, kept only in Gofer as the note said. The
  // picker stays open, saying it saved, and the dot, the menu and the events
  // take the color.
  popovers.length = 0
  respond({color: '#2563eb', in_gofer_only: true, reason: 'google_access'})
  fire('click', options[1])
  assert.equal(requests.length, 1)
  assert.equal(requests[0].url, '/api/calendar/sources/club/color')
  assert.equal(requests[0].options.method, 'POST')
  assert.equal(requests[0].options.body, '{"color":"#2563eb"}')
  await settle(); await settle()
  assert.equal(dot.style.backgroundColor, '#2563eb')
  assert.equal(item.getAttribute('data-calendar-color'), '#2563eb')
  assert.equal(item.getAttribute('data-calendar-color-in-gofer'), 'true')
  assert.equal(refreshes, 1)
  assert.equal(reset.hidden, false, 'it can go back to the calendar\'s own color')
  assert.deepEqual(options.map(o => o.getAttribute('aria-pressed')), ['false', 'true'])
  assert.equal(status.textContent, context.goferT('js.settings.status.saved'))
  flush()
  assert.deepEqual(popovers, [], 'the picker stays open')
  assert.equal(status.textContent, '', 'the status clears after a moment')

  // Going back to the calendar's own color sends an empty one.
  popovers.length = 0
  respond({color: '#16a765', in_gofer_only: false})
  fire('click', reset)
  assert.equal(requests[1].options.body, '{"color":""}')
  await settle(); await settle()
  assert.equal(dot.style.backgroundColor, '#16a765')
  assert.equal(item.getAttribute('data-calendar-color-in-gofer'), 'false')
  assert.equal(reset.hidden, true)

  // A provider refusing a color Gofer expected it to take shows the note
  // that says so.
  item.dataset.calendarColorNote = ''
  fire('click', item); flush()
  assert.deepEqual(shown(), [])
  popovers.length = 0
  respond({color: '#ef4444', in_gofer_only: true, reason: 'google_access'})
  fire('click', options[0])
  await settle(); await settle()
  flush()
  assert.deepEqual(shown(), ['google_access'])
  assert.deepEqual(popovers, [], 'the picker stays open')
  assert.equal(item.getAttribute('data-calendar-color-note'), 'google_access')

  // A failure shows in the status line and changes nothing.
  respond('Google Calendar is not answering.', false)
  custom.value = '#123456'
  fire('input', custom)
  assert.equal(code.textContent, '#123456', 'the color code follows the custom color as it is picked')
  fire('change', custom)
  assert.equal(requests.at(-1).options.body, '{"color":"#123456"}')
  await settle(); await settle()
  assert.equal(status.textContent, 'Google Calendar is not answering.')
  assert.equal(status.classes.has('text-destructive'), true)
  assert.equal(dot.style.backgroundColor, '#ef4444')

  // Allowing full calendar access closes the picker and asks first.
  fire('click', allow)
  assert.deepEqual(popovers.at(-1), ['close', 'calendar-color-popover'])
  assert.deepEqual(offers, [['Book club', 'alex@example.com', 'color']])

  console.log('calendar color picker tests passed')
})().catch(error => { console.error(error); process.exit(1) })
