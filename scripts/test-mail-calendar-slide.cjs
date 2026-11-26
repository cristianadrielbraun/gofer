const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/mail-calendar-sidebar.js'), 'utf8')

// Loads the calendar beside Mail with a fake page. slide stands in for
// resize.js's slideAppPanelWidth and decides what it hands back.
function load(slide) {
  const listeners = {}
  const slides = []
  const element = () => ({attributes: {}, setAttribute(name, value) { this.attributes[name] = value }})
  const toggle = element()
  const grip = element()
  const aside = element()
  const shell = {
    dataset: {open: 'true'},
    style: {setProperty() {}, removeProperty() {}},
    querySelector(selector) {
      if (selector === '[data-mail-calendar-toggle]') return toggle
      if (selector === '[data-mail-calendar-grip]') return grip
      if (selector === '[data-mail-calendar-sidebar]') return aside
      return null
    },
    getBoundingClientRect() { return {width: this.dataset.open === 'true' ? 300 : 0} },
  }
  toggle.closest = selector => selector === '[data-mail-calendar-shell]' ? shell : null
  const storage = {}
  const context = vm.createContext({
    window: {},
    localStorage: {getItem: key => storage[key] ?? null, setItem: (key, value) => { storage[key] = value }},
    matchMedia: () => ({matches: true, addEventListener() {}}),
    MutationObserver: class { observe() {} },
    document: {
      body: {},
      addEventListener(type, listener) { listeners[type] = listener },
      querySelector: () => null,
      querySelectorAll: () => [],
    },
    slideAppPanelWidth(target, from, to) {
      slides.push({target, from, to})
      return slide()
    },
  })
  vm.runInContext(source, context)
  const click = () => listeners.click({target: {closest: selector => selector === '[data-mail-calendar-toggle]' ? toggle : null}})
  return {shell, aside, slides, storage, click}
}

function fakeAnimation() {
  return {
    listeners: {},
    cancelled: false,
    addEventListener(type, listener) { this.listeners[type] = listener },
    cancel() { this.cancelled = true; this.listeners.cancel?.() },
    finish() { this.listeners.finish?.() },
  }
}

// Hiding slides the shell from its open width to nothing, and the panel
// stays drawn until the slide ends.
let animations = []
let page = load(() => { const animation = fakeAnimation(); animations.push(animation); return animation })
page.click()
assert.equal(page.shell.dataset.open, 'false')
assert.equal(page.slides.length, 1)
assert.equal(page.slides[0].target, page.shell)
assert.equal(page.slides[0].from, 300, 'The slide starts at the width the calendar had')
assert.equal(page.slides[0].to, 0, 'The slide ends at the hidden width')
assert.equal(page.shell.dataset.mailCalendarSliding, 'true', 'The panel stays drawn while it slides away')
assert.equal(page.aside.inert, true, 'A hiding calendar takes no clicks or focus')
animations[0].finish()
assert.equal(page.shell.dataset.mailCalendarSliding, undefined, 'CSS hides the panel once the slide ends')

// Showing it again slides back to the open width.
page.click()
assert.equal(page.shell.dataset.open, 'true')
assert.deepEqual([page.slides[1].from, page.slides[1].to], [0, 300])

// Turning back halfway stops the first slide, and its end doesn't clear the
// mark for the slide that replaced it.
page.click()
const first = animations[2]
page.click()
assert.equal(first.cancelled, true, 'A new click replaces the running slide')
assert.equal(page.shell.dataset.mailCalendarSliding, 'true', 'The replaced slide must not end the new one early')
animations[3].finish()
assert.equal(page.shell.dataset.mailCalendarSliding, undefined)

// Without motion (phones, reduced motion) the calendar just switches.
page = load(() => null)
page.click()
assert.equal(page.shell.dataset.open, 'false')
assert.equal(page.shell.dataset.mailCalendarSliding, undefined, 'Nothing waits for a slide that never ran')
assert.equal(page.storage['gofer.mailCalendar.open.wide.v2'], 'false')

console.log('Mail calendar slide: hide and show slide the shell, the panel stays drawn while sliding, a second click replaces the slide, and no motion just switches.')
