const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const source = fs.readFileSync(require('node:path').join(__dirname, '../assets/js/app.js'), 'utf8')
function fn(name) {
  const start = source.indexOf('function ' + name + '(')
  const end = source.indexOf('\n}', start)
  assert(start >= 0 && end > start)
  return source.slice(start, end + 2)
}
const aborts = [], events = []
let cleared = 0, focused = 0
const input = {value: '"Last, First" <first@example.com>, sec', disabled: false, dispatchEvent(event) { events.push(event.type) }, focus() { focused++ }}
const form = {querySelector(selector) { return selector === '[name="guests"]' ? input : {replaceChildren() { cleared++ }} }}
const runtime = vm.createContext({window: {htmx: {trigger(node, type) { aborts.push([node, type]) }}}, Event: class { constructor(type) { this.type = type } }})
vm.runInContext(fn('updateCalendarGuestDropdown') + '\n' + fn('selectCalendarGuest'), runtime)
runtime.selectCalendarGuest({closest() { return form }, dataset: {calendarGuestValue: 'Second <second@example.com>'}})
assert.equal(input.value, '"Last, First" <first@example.com>, Second <second@example.com>, ')
assert.deepEqual(events, ['change'])
assert.equal(aborts[0][1], 'htmx:abort')
assert.equal(cleared, 1)
assert.equal(focused, 1)
input.disabled = true
runtime.selectCalendarGuest({closest() { return form }, dataset: {calendarGuestValue: 'Third <third@example.com>'}})
assert.equal(cleared, 1, 'busy forms must not modify invitees')

const dropdownCalls = [], attributes = {}
const option = {id: 'calendar-guest-first', dataset: {calendarGuestActive: 'false'}, setAttribute() {}, scrollIntoView() {}}
const content = {style: {}, contains() { return false }}
const list = {querySelector() { return option }, querySelectorAll() { return [option] }}
const root = {id: 'calendar-guests-dropdown', querySelector() { return content }}
const guestInput = {disabled: false, setAttribute(name, value) { attributes[name] = value }, removeAttribute(name) { delete attributes[name] }, getBoundingClientRect() { return {width: 384} }, closest() { return dropdownForm }}
const dropdownForm = {querySelector(selector) { return selector === '[name="guests"]' ? guestInput : selector === '#calendar-guests-dropdown' ? root : list }}
const dropdownDocument = {activeElement: guestInput}
const dropdownRuntime = vm.createContext({document: dropdownDocument, window: {tui: {popover: {open(id) { dropdownCalls.push(['open', id]) }, close(id) { dropdownCalls.push(['close', id]) }, isOpen() { return true }}}}})
vm.runInContext(fn('updateCalendarGuestDropdown') + '\n' + fn('handleCalendarGuestKeydown'), dropdownRuntime)
dropdownRuntime.updateCalendarGuestDropdown(dropdownForm)
assert.equal(content.style.width, '384px', 'dropdown must match the guest field width')
assert.equal(attributes['aria-expanded'], 'true')
let prevented = false
dropdownRuntime.handleCalendarGuestKeydown({key: 'ArrowDown', preventDefault() { prevented = true }}, guestInput)
assert.equal(prevented, true)
assert.equal(attributes['aria-activedescendant'], option.id)
dropdownRuntime.handleCalendarGuestKeydown({key: 'Escape', preventDefault() {}}, guestInput)
assert.equal(attributes['aria-expanded'], 'false')
assert.equal(attributes['aria-activedescendant'], undefined)
dropdownDocument.activeElement = null
dropdownRuntime.updateCalendarGuestDropdown(dropdownForm)
assert.equal(dropdownCalls.at(-1)[0], 'close', 'late results must not reopen an unfocused guest field')

let error = ''
const fields = {all_day: {checked: false}, repeat_frequency: {value: 'weekly'}, guests: {value: 'guest@example.com', focus() { focused++ }}}
const recurrenceRuntime = vm.createContext({adjustCalendarCreateAllDayRange() {}, setCalendarCreateError(form, message) { error = message }})
vm.runInContext(fn('validateCalendarCreatePickers'), recurrenceRuntime)
const repeatForm = {querySelector(selector) { const match = selector.match(/name="([^"]+)"/); return match ? fields[match[1]] : null }}
assert.equal(recurrenceRuntime.validateCalendarCreatePickers(repeatForm), false)
assert.match(error, /do not repeat/)
console.log('Calendar guests: quoted names, contact selection, stale suggestion cancellation, busy forms, and recurrence validation passed.')
