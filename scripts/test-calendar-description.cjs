const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const source = fs.readFileSync(require('node:path').join(__dirname, '../assets/js/app.js'), 'utf8')
const css = fs.readFileSync(require('node:path').join(__dirname, '../assets/css/input.css'), 'utf8')
assert(css.includes('[data-calendar-description-editor]::before {'), 'the surface must exist independently of animation state')
assert(css.includes('[data-calendar-description-editor][data-resizing="true"]::before {\n  box-shadow: none;\n  transition: none;\n}'), 'hide the ring during resize without hiding the surface')
assert(!css.includes('shadow-xs transition-[color,box-shadow]'), 'restoring the ring must not start a delayed shadow transition')
assert(css.includes('[data-calendar-description-editor] textarea:focus-visible {'), 'native focus styling must not introduce a second fading surface')
function fn(name) {
  const start = source.indexOf('function ' + name + '(')
  const end = source.indexOf('\n}', start)
  assert(start >= 0 && end > start)
  return source.slice(start, end + 2)
}
const animations = []
function animate(frames, timing) {
  let resolve, reject
  const animation = {frames, timing, finished: new Promise((ok, fail) => { resolve = ok; reject = fail }), cancel() { reject(new Error('canceled')) }, finish() { resolve() }}
  // Fade promises are not awaited by the editor, as in the browser's WAAPI.
  animation.finished.catch(() => {})
  animations.push(animation)
  return animation
}
const attrs = {}, icons = {expand: {hidden: false}, collapse: {hidden: true}}
const button = {setAttribute(name, value) { attrs[name] = value }}
const textarea = {value: 'Original notes', focusCount: 0, focus() { this.focusCount++ }}
const content = {style: {opacity: ''}, animate}
const slot = {style: {minHeight: ''}, getBoundingClientRect() { return {left: 145, top: 430, width: 335, height: 100} }, appendChild(node) { node.parent = this }}
const body = {style: {opacity: '', pointerEvents: ''}, inert: false, animate}
const viewport = {clientWidth: 424, clientHeight: 500, getBoundingClientRect() { return {left: 100, top: 100} }, appendChild(node) { node.parent = this }}
const editor = {
  style: {cssText: '', setProperty(name, value) { this[name] = value }}, dataset: {}, parent: slot, animate,
  getBoundingClientRect() {
    if (this.parent === slot) return slot.getBoundingClientRect()
    return {left: 124, top: 104, width: 376, height: 476}
  },
  querySelector(selector) { return selector === 'textarea' ? textarea : selector.endsWith('-content]') ? content : selector.endsWith('-toggle]') ? button : selector.endsWith('-expand]') ? icons.expand : icons.collapse },
}
const elements = {'[data-calendar-description-editor]': editor, '[data-calendar-description-slot]': slot, '[data-calendar-create-viewport]': viewport, '[data-calendar-create-body]': body}
const form = {isConnected: true, querySelector(selector) { return elements[selector] }, closest() { return {open: true} }}
let reduced = false, saves = 0
const runtime = vm.createContext({window: {matchMedia() { return {matches: reduced} }}, getComputedStyle() { return {opacity: body.style.opacity || '1', paddingLeft: '24', paddingRight: '24', paddingTop: '4', paddingBottom: '20'} }, submitCalendarCreate() { saves++ }})
vm.runInContext(['toggleCalendarDescription', 'setCalendarDescriptionExpanded', 'prepareCalendarDescriptionSubmit'].map(fn).join('\n'), runtime)

async function completeMotion(done) {
  animations.at(-2).finish(); animations.at(-1).finish()
  await Promise.resolve()
  assert.equal(editor.dataset.resizing, undefined)
  assert.equal(animations.at(-1).frames[0].opacity, 0, 'only content fades in; permanent CSS surface stays present')
  animations.at(-1).finish() // Content reveal after the empty frame settles.
  await done
}

async function run() {
  let done = runtime.toggleCalendarDescription(form)
  assert.equal(editor.parent, viewport, 'promote the original editor inside the existing dialog')
  assert.equal(slot.style.minHeight, '100px', 'preserve the dialog height and scroll layout')
  assert.equal(body.inert, true)
  assert.equal(attrs['aria-expanded'], 'true')
  assert.equal(icons.collapse.hidden, false)
  assert.equal(animations[0].timing.duration, 220)
  assert.equal(animations[0].frames[1].scale, '1 1')
  assert.equal(animations[0].frames[1].translate, '0px 0px')
  assert.equal(animations[0].timing.fill, 'forwards', 'hold endpoint through layout restoration')
  assert(!('width' in animations[0].frames[0]) && !('height' in animations[0].frames[0]), 'do not reflow native textarea on each animation frame')
  assert.equal(content.style.opacity, '0', 'text and button must stay out of the scaling layer')
  assert.equal(editor.dataset.resizing, 'true')
  textarea.value += ' — typed while growing'
  await completeMotion(done)
  assert.equal(editor.dataset.resizing, undefined)
  assert.equal(content.style.opacity, '')
  assert.equal(animations.at(-1).timing.duration, 160)
  assert.equal(form._calendarDescriptionExpanded, true)
  assert.equal(editor.style.width, 'calc(100% - 48px)', 'expanded width follows viewport resizing')
  done = runtime.toggleCalendarDescription(form)
  await completeMotion(done)
  assert.equal(editor.parent, slot)
  assert.equal(textarea.value, 'Original notes — typed while growing')
  assert.equal(body.inert, false)
  assert.equal(body.style.opacity, '')
  assert.equal(slot.style.minHeight, '')
  assert.equal(form._calendarDescriptionState, null)

  // Reversing mid-animation must cancel the old finish, not reparent early.
  const expanding = runtime.toggleCalendarDescription(form)
  done = runtime.toggleCalendarDescription(form)
  await expanding
  assert.equal(editor.parent, viewport)
  await completeMotion(done)
  assert.equal(editor.parent, slot)

  reduced = true
  await runtime.toggleCalendarDescription(form)
  assert.equal(editor.parent, viewport)
  let prevented = false
  runtime.prepareCalendarDescriptionSubmit({preventDefault() { prevented = true }}, {closest() { return form }})
  await Promise.resolve(); await Promise.resolve()
  assert.equal(prevented, true)
  assert.equal(editor.parent, slot, 'restore hidden required fields before validation and saving')
  assert.equal(saves, 1)
  console.log('Calendar description: original editor, stable frame, live typing, expand/collapse, interruption, reduced motion, and save restoration passed.')
}
run().catch(error => { console.error(error); process.exitCode = 1 })
