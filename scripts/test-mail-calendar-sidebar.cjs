const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/mail-calendar-sidebar.js'), 'utf8')

// Loads the calendar sidebar with a fake page, as wide as screen says.
// slide stands in for resize.js's slideAppPanelWidth and decides what it
// hands back.
function load(slide, screen = 'wide', reducedMotion = false) {
  const fannedIn = []
  let content = null
  const listeners = {}
  const windowListeners = {}
  const mediaListeners = {}
  const rootAttributes = new Set()
  const observers = []
  const root = {
    hasAttribute: name => rootAttributes.has(name),
    toggleAttribute(name, on) { if (on) rootAttributes.add(name); else rootAttributes.delete(name) },
  }
  const topbarButton = element()
  const slides = []
  const toggle = element()
  let shellOnPage = true
  const grip = element()
  const aside = element()
  const shell = {
    dataset: {open: 'true'},
    style: {setProperty() {}, removeProperty() {}},
    querySelector(selector) {
      if (selector === '[data-mail-calendar-toggle]') return toggle
      if (selector === '[data-mail-calendar-grip]') return grip
      if (selector === '[data-mail-calendar-sidebar]') return aside
      if (selector === '#mail-calendar-sidebar-content') return content
      return null
    },
    getBoundingClientRect() { return {width: this.dataset.open === 'true' ? 300 : 0} },
  }
  toggle.closest = selector => selector === '[data-mail-calendar-shell]' ? shell : null
  grip.closest = toggle.closest
  grip.removeAttribute = name => { delete grip.attributes[name] }
  const storage = {}
  const settings = {}
  const context = vm.createContext({
    window: {addEventListener(type, listener) { windowListeners[type] = listener }, removeEventListener() {}},
    localStorage: {getItem: key => storage[key] ?? null, setItem: (key, value) => { storage[key] = value }},
    matchMedia: query => ({
      matches: query.includes('min-width: 1536px') ? screen === 'wide' : query.includes('max-width: 1023.98px') ? screen === 'small' : query.includes('reduced-motion') ? reducedMotion : false,
      addEventListener(type, listener) { mediaListeners[query] = listener },
    }),
    MutationObserver: class { constructor(callback) { observers.push(callback) } observe() {} },
    document: {
      body: {style: {}},
      documentElement: root,
      addEventListener(type, listener) { (listeners[type] ||= []).push(listener) },
      removeEventListener(type, listener) { listeners[type] = (listeners[type] || []).filter(other => other !== listener) },
      querySelector: selector => selector === '#main-content [data-mail-calendar-shell]' && shellOnPage ? shell : null,
      querySelectorAll: selector => selector === '[data-mobile-mail-calendar]' ? [topbarButton] : [],
    },
    fanInElements(nodes, delay) { fannedIn.push({nodes, delay}) },
    GoferSettings: {get: key => settings[key] ?? null, set(key, value) { settings[key] = value }},
    slideAppPanelWidth(target, from, to) {
      slides.push({target, from, to})
      return slide()
    },
  })
  vm.runInContext(source, context)
  const dispatch = (type, event) => (listeners[type] || []).forEach(listener => listener(event))
  const clickOn = attribute => dispatch('click', {target: {closest: selector => selector === '[' + attribute + ']' ? (attribute === 'data-mail-calendar-toggle' ? toggle : {}) : null}})
  return {
    shell, aside, slides, storage, settings, root, topbarButton,
    oobSwap(target, incoming) { dispatch('htmx:oobBeforeSwap', {detail: {target, fragment: {querySelectorAll: () => [incoming]}}}) },
    click: () => clickOn('data-mail-calendar-toggle'),
    tapTopbarButton: () => clickOn('data-mobile-mail-calendar'),
    tapBackdrop: () => clickOn('data-mobile-nav-close'),
    pressEscape: () => dispatch('keydown', {key: 'Escape', target: {}}),
    grip,
    pressGrip: () => dispatch('pointerdown', {button: 0, clientX: 900, preventDefault() {}, target: {closest: selector => selector === '[data-mail-calendar-grip]' ? grip : null}}),
    moveTo: clientX => dispatch('pointermove', {clientX}),
    releaseGrip: () => dispatch('pointerup', {}),
    goBack: () => windowListeners.popstate(),
    leaveMail() { shellOnPage = false; observers.forEach(callback => callback()) },
    fannedIn,
    observe: () => observers.forEach(callback => callback()),
    // Puts calendar content in the sidebar, as htmx does, with these events.
    swapIn(events, busy = false) {
      content = {dataset: {}, getAttribute: name => name === 'aria-busy' ? String(busy) : null, querySelectorAll: selector => selector === '[data-calendar-event-trigger]' ? events : []}
      observers.forEach(callback => callback())
    },
  }
}

function element() {
  return {attributes: {}, setAttribute(name, value) { this.attributes[name] = value }}
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

// Dragging the grip keeps it marked, so its line stays highlighted over the
// tab even when the pointer drifts off it, and letting go clears the mark.
page = load(() => null)
page.pressGrip()
assert('data-dragging' in page.grip.attributes, 'The grip is marked while it is dragged')
page.moveTo(860)
page.releaseGrip()
assert(!('data-dragging' in page.grip.attributes), 'Letting go clears the mark')
assert.equal(page.settings.mail_calendar_width, '340', 'The dragged width is saved with the other pane sizes')

// The calendar keeps itself when Mail is drawn again, but it replaces the
// empty placeholder of a Mail drawn with it turned off.
const attributes = names => ({names: new Set(names), hasAttribute(name) { return this.names.has(name) }, removeAttribute(name) { this.names.delete(name) }})
const incoming = () => attributes(['data-mail-calendar-shell', 'hx-preserve'])
let arriving = incoming()
page.oobSwap(Object.assign(attributes([]), {id: 'mail-calendar-sidebar-slot'}), arriving)
assert.equal(arriving.hasAttribute('hx-preserve'), false, 'The calendar replaces the placeholder')
arriving = incoming()
page.oobSwap(Object.assign(attributes(['data-mail-calendar-shell']), {id: 'mail-calendar-sidebar-slot'}), arriving)
assert.equal(arriving.hasAttribute('hx-preserve'), true, 'The calendar on screen keeps itself')

// Events come in like a new mail list's rows, once for each day or month
// that arrives, but not while the calendar is still loading.
page = load(() => null)
page.swapIn(['skeleton'], true)
assert.equal(page.fannedIn.length, 0, 'The loading calendar has no events to bring in')
page.swapIn(['lunch', 'standup'])
assert.equal(page.fannedIn.length, 1)
assert.deepEqual([...page.fannedIn[0].nodes], ['lunch', 'standup'], 'The day\'s events come in together, like list rows')
page.observe()
assert.equal(page.fannedIn.length, 1, 'Other changes on the page don\'t bring the same events in again')
page.swapIn(['dentist'])
assert.equal(page.fannedIn.length, 2, 'Another day coming in brings its events in again')
page = load(() => null, 'wide', true)
page.swapIn(['lunch'])
assert.equal(page.fannedIn.length, 0, 'Reduced motion shows events without the fan-in')

// Small screens: the top bar's button opens the calendar as a drawer. It
// starts closed, whatever wider screens left the calendar at.
page = load(() => null, 'small')
assert.equal(page.aside.inert, true, 'A closed drawer takes no taps or focus')
page.tapBackdrop()
assert.equal(page.root.hasAttribute('data-mail-calendar-drawer-open'), false, 'The backdrop only closes the drawer')
page.tapTopbarButton()
assert.equal(page.root.hasAttribute('data-mail-calendar-drawer-open'), true)
assert.equal(page.topbarButton.attributes['aria-expanded'], 'true')
assert.equal(page.aside.inert, false, 'The open drawer takes taps')
assert.equal(page.shell.dataset.open, 'false', 'Opening the drawer leaves the wider screens\' choice alone')

for (const [how, close] of [
  ['the button again', p => p.tapTopbarButton()],
  ['the backdrop', p => p.tapBackdrop()],
  ['Escape', p => p.pressEscape()],
  ['going back', p => p.goBack()],
]) {
  if (!page.root.hasAttribute('data-mail-calendar-drawer-open')) page.tapTopbarButton()
  close(page)
  assert.equal(page.root.hasAttribute('data-mail-calendar-drawer-open'), false, how + ' closes the drawer')
  assert.equal(page.topbarButton.attributes['aria-expanded'], 'false')
  assert.equal(page.aside.inert, true)
}

assert.equal(page.root.hasAttribute('data-mail-calendar-in-page'), true, 'Mail with the calendar shows the top bar button')
page.tapTopbarButton()
page.leaveMail()
assert.equal(page.root.hasAttribute('data-mail-calendar-in-page'), false, 'Other sections hide the top bar button')
assert.equal(page.root.hasAttribute('data-mail-calendar-drawer-open'), false, 'Leaving Mail closes the drawer with the calendar')

console.log('Mail calendar sidebar: hide and show slide the shell, the panel stays drawn while sliding, a second click replaces the slide, no motion just switches, and small screens open and close it as a drawer.')
