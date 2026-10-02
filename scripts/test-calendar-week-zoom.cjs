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
function element(event = false) {
  return {dataset: {}, style: {height: '24px', top: '100px', left: '10px', width: '90px'}, hasAttribute() { return event }}
}
const row = element(), event = element(true), timeline = element(), grid = element()
grid.querySelectorAll = () => [timeline, row, event]
const scroller = {clientHeight: 500, scrollTop: 0, isConnected: true, querySelector() { return grid }}
const calendar = {querySelector() { return scroller }}
const callbacks = new Map()
let frameID = 0, reducedMotion = false
const axis = height => ({height, heights: Array(24).fill(height / 24), offsets: Array.from({length: 25}, (_, index) => index * height / 24), expanded: Array(24).fill(true), zoom: 0})
grid._calendarWeekAxis = axis(480)
const context = vm.createContext({
  document: {getElementById() { return calendar }},
  window: {matchMedia() { return {matches: reducedMotion} }, getComputedStyle(node) { return node.style }},
  requestAnimationFrame(callback) { callbacks.set(++frameID, callback); return frameID },
  cancelAnimationFrame(id) { callbacks.delete(id) },
  initializeCalendarViewport() {
    const zoom = context._calendarWeekZoom
    const height = [480, 960, 1536, 2304][zoom]
    grid._calendarWeekAxis = {...axis(height), zoom}
    grid.style.height = height + 'px'
    timeline.style.height = height + 'px'
    row.style.height = height / 24 + 'px'
    event.style.top = height / 4 + 'px'
    event.style.height = height / 12 + 'px'
    event.style.left = zoom ? '20px' : '10px'
    event.style.width = zoom ? '80px' : '90px'
    scroller.scrollTop = zoom ? 200 : 0
  },
})
vm.runInContext('var _calendarWeekZoom = 0; var _calendarVisibility = new Map();\n' + ['_calendarSourceIsVisible', 'cancelCalendarWeekZoom', 'setCalendarWeekZoom'].map(helper).join('\n'), context)
context.initializeCalendarViewport()
function tick(now) {
  const queued = Array.from(callbacks.values())
  callbacks.clear()
  queued.forEach(callback => callback(now))
}

context.setCalendarWeekZoom(1)
assert.equal(row.style.height, '20px', 'Zoom must begin at the visible spacing')
assert.equal(scroller.scrollTop, 0, 'Scroll must not jump before the first frame')
tick(0)
tick(110)
assert.ok(parseFloat(row.style.height) > 20 && parseFloat(row.style.height) < 40)
assert.ok(scroller.scrollTop > 0 && scroller.scrollTop < 200)
assert.ok(grid._calendarWeekAxis.height > 480 && grid._calendarWeekAxis.height < 960)
const visibleHeight = row.style.height, visibleTop = scroller.scrollTop
context.setCalendarWeekZoom(2)
assert.equal(row.style.height, visibleHeight, 'Rapid zoom must continue from the visible spacing')
assert.equal(scroller.scrollTop, visibleTop)
assert.equal(callbacks.size, 1, 'Only the latest zoom may keep animating')
tick(120)
tick(340)
assert.equal(row.style.height, '64px')
assert.equal(event.style.top, '384px')
assert.equal(scroller.scrollTop, 200)
assert.equal(scroller._calendarZoomAnimation, null)
assert.equal(callbacks.size, 0)

context.setCalendarWeekZoom(0)
tick(400)
tick(510)
assert.ok(parseFloat(row.style.height) > 20 && parseFloat(row.style.height) < 64)
tick(620)
assert.equal(row.style.height, '20px')
assert.equal(scroller.scrollTop, 0, 'Fit must finish with no vertical scroll')
context.setCalendarWeekZoom(0)
assert.equal(callbacks.size, 0, 'Selecting the active zoom must do nothing')

reducedMotion = true
context.setCalendarWeekZoom(1)
assert.equal(row.style.height, '40px')
assert.equal(callbacks.size, 0, 'Reduced motion must apply the layout immediately')
reducedMotion = false
context.setCalendarWeekZoom(2)
context.cancelCalendarWeekZoom(scroller)
assert.equal(callbacks.size, 0, 'Resizing may cancel a zoom without a stale frame overriding the new layout')
context.setCalendarWeekZoom(3)
scroller.isConnected = false
tick(700)
assert.equal(scroller._calendarZoomAnimation, null, 'A replaced Calendar must stop animating')
assert.equal(callbacks.size, 0)
console.log('Calendar Week zoom: synchronized interpolation, rapid clicks, Fit, reduced motion, resize cancellation, and detached panes passed.')
