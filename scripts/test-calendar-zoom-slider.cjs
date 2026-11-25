const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// The slider component: another control moves it with 'tui-slider:set', which
// redraws it without firing 'input'.
function sliderRoot() {
  const listeners = new Map()
  const thumb = {style: {}, attributes: {}, setAttribute(name, value) { this.attributes[name] = String(value) }, getAttribute(name) { return this.attributes[name] ?? null }, addEventListener() {}}
  const input = {value: '0', events: [], dispatchEvent(event) { this.events.push(event.type) }}
  const fill = {style: {}}
  const track = {addEventListener() {}}
  const root = {
    id: 'zoom', dataset: {min: '0', max: '3', step: '1'},
    querySelector(selector) { return {'[data-track]': track, '[data-fill]': fill}[selector] || null },
    querySelectorAll(selector) { return selector === '[data-thumb]' ? [thumb] : selector === '[data-input]' ? [input] : [] },
    addEventListener(type, listener) { listeners.set(type, listener) },
    dispatch(type, detail) { listeners.get(type)({type, detail}) },
  }
  return {root, thumb, input, fill}
}
const slider = sliderRoot()
vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../assets/js/slider.js'), 'utf8'), {
  document: {querySelectorAll(selector) { return selector.startsWith('[data-tui-slider]') ? [slider.root] : [] }, body: {}},
  MutationObserver: class { observe() {} },
})
assert.equal(slider.thumb.style.left, '0%')
slider.root.dispatch('tui-slider:set', {value: 2})
assert.equal(slider.input.value, 2)
assert.equal(slider.thumb.style.left, (2 / 3 * 100) + '%', 'Setting the value must move the thumb')
assert.equal(slider.thumb.attributes['aria-valuenow'], '2')
assert.deepEqual(slider.input.events, [], 'Setting the value from outside must not fire input')
slider.root.dispatch('tui-slider:set', {value: 9})
assert.equal(slider.input.value, 3, 'Values past the end must stop at the end')

// The calendar: moving the zoom slider zooms the hour grid to that level.
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('// Dragging the zoom slider')
const end = source.indexOf('\n})\n', start)
assert(start >= 0 && end > start, 'Missing the zoom slider listener')
let inputListener = null
const zooms = []
let finished = 0
vm.runInNewContext('var CALENDAR_WEEK_ZOOM_LEVELS = 6\n' + source.slice(start, end + 3), {
  document: {addEventListener(type, listener) { if (type === 'input') inputListener = listener }},
  finishCalendarNavigationTransition() { finished++ },
  setCalendarWeekZoom(zoom) { zooms.push(zoom) },
})
const zoomSlider = {}
function moved(value, inSlider = true) {
  inputListener({target: {value: String(value), closest(selector) { return inSlider && selector === '[data-calendar-week-zoom-slider]' ? zoomSlider : null }}})
}
moved(2)
moved(0)
moved(7)
moved(1, false)
assert.deepEqual(zooms, [2, 1, 6], 'Only the zoom slider may zoom, only to one of its levels, and never to Fit')
assert.equal(finished, 3, 'Zooming must finish a running navigation slide first')

// The buttons: Fit is its own layout, and - and + step from the slider's level.
const clickStart = source.indexOf('document.addEventListener("click", function (event) {\n  var button = event.target.closest && event.target.closest("[data-calendar-week-zoom]")')
const clickEnd = source.indexOf('\n})\n', clickStart)
assert(clickStart >= 0 && clickEnd > clickStart, 'Missing the zoom button listener')
let clickListener = null
const buttons = vm.createContext({
  document: {addEventListener(type, listener) { if (type === 'click') clickListener = listener }},
  finishCalendarNavigationTransition() {},
  setCalendarWeekZoom(zoom) { buttons._calendarWeekZoom = zoom; if (zoom > 0) buttons._calendarWeekZoomLevel = zoom },
})
vm.runInContext('var CALENDAR_WEEK_ZOOM_LEVELS = 6; var _calendarWeekZoom = 0; var _calendarWeekZoomLevel = 2\n' + source.slice(clickStart, clickEnd + 3), buttons)
function press(action) {
  clickListener({target: {closest() { return {disabled: false, dataset: {calendarWeekZoom: action}} }}})
  return buttons._calendarWeekZoom
}
assert.equal(press('in'), 3, '+ from Fit must step up from the level the slider shows')
assert.equal(press('fit'), 0)
assert.equal(buttons._calendarWeekZoomLevel, 3, 'Fit must leave the slider where it was')
assert.equal(press('out'), 2, '- from Fit must step down from the level the slider shows')
assert.equal(press('out'), 1)
assert.equal(press('out'), 1, 'The lowest level is a zoom level, not Fit')

// Level 1 is the whole day in even hours; the levels after it only grow.
vm.runInContext(source.slice(source.indexOf('function _calendarWeekAxis('), source.indexOf('\n}', source.indexOf('function _calendarWeekAxis(')) + 2), buttons)
const heights = [1, 2, 3, 4, 5, 6].map(level => buttons._calendarWeekAxis(600, [{start: 600, end: 660}], level).height)
assert.equal(heights[0], 600, 'The lowest level must fit the whole day')
heights.slice(1).forEach((height, index) => assert.ok(height > heights[index], 'Each level must be larger than the one before'))
console.log('Calendar zoom slider: outside set, clamping, slider-driven zoom, Fit apart from the levels, and even level sizes passed.')
