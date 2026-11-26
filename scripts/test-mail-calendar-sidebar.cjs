const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/mail-calendar-sidebar.js'), 'utf8')

;(async () => {

// Loads the calendar sidebar with a fake page, as wide as screen says.
// slide stands in for resize.js's slideAppPanelWidth and decides what it
// hands back.
function load(slide, screen = 'wide', reducedMotion = false, saved = {}) {
  const fannedIn = []
  const fannedOut = []
  const fadedDays = []
  const swapped = []
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
  const settings = {...saved}
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
      getElementById: id => id === 'mail-calendar-sidebar-content' ? content : null,
      documentElement: root,
      addEventListener(type, listener) { (listeners[type] ||= []).push(listener) },
      removeEventListener(type, listener) { listeners[type] = (listeners[type] || []).filter(other => other !== listener) },
      querySelector: selector => selector === '#main-content [data-mail-calendar-shell]' && shellOnPage ? shell : null,
      querySelectorAll: selector => selector === '[data-mobile-mail-calendar]' ? [topbarButton] : [],
    },
    fanInElements(nodes, delay) { fannedIn.push({nodes, delay}) },
    fanOutElement(node) {
      const animation = {node, cancelled: false, cancel() { this.cancelled = true }}
      animation.finished = new Promise(resolve => { animation.finish = resolve })
      fannedOut.push(animation)
      return animation
    },
    fadeInCalendarDays(nodes, direction) { fadedDays.push({nodes: [...nodes], direction}) },
    htmx: {swap(target, response) {
      swapped.push({target, response})
      content = response
      observers.forEach(callback => callback())
    }},
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
    fannedIn, fannedOut, fadedDays, swapped,
    content: () => content,
    request(xhr) { dispatch('htmx:beforeRequest', {detail: {xhr, target: content}}) },
    respond(xhr, response, ok = true) {
      const detail = {xhr, target: content, shouldSwap: ok, serverResponse: response}
      dispatch('htmx:beforeSwap', {detail})
      return detail
    },
    fail(type, xhr) { dispatch(type, {detail: {xhr}}) },
    observe: () => observers.forEach(callback => callback()),
    // Puts calendar content in the sidebar, as htmx does, with these events.
    swapIn(events, busy = false) {
      content = calendarContent(events, busy)
      observers.forEach(callback => callback())
    },
  }
}

// A calendar sidebar's content as htmx puts it in: month is its month, and
// days are what a day of it looks like to the month fade.
function calendarContent(events, busy = false, month = '2026-03', days = ['day']) {
  return {
    id: 'mail-calendar-sidebar-content',
    dataset: {mailCalendarMonth: month},
    getAttribute: name => name === 'aria-busy' ? String(busy) : null,
    querySelectorAll: selector => selector.includes('data-calendar-event-trigger') ? events : selector === '[data-mail-calendar-day]' ? days : [],
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
assert.equal(page.settings.mail_calendar_open, 'false', 'Closing it beside the panes is saved with the other pane settings')

// Over the panes (smaller screens) it has its own choice, closed unless
// opened, and opening it there leaves the wide screens' choice alone.
page = load(() => null, 'medium')
assert.equal(page.shell.dataset.open, 'false', 'Over the panes it starts closed')
page.click()
assert.equal(page.shell.dataset.open, 'true')
assert.equal(page.settings.mail_calendar_overlay_open, 'true', 'Opening it over the panes is saved for smaller screens')
assert.equal(page.settings.mail_calendar_open, undefined)
page = load(() => null, 'medium', false, {mail_calendar_overlay_open: 'true'})
assert.equal(page.shell.dataset.open, 'true', 'It opens over the panes when it was left open there')
page = load(() => null, 'wide', false, {mail_calendar_open: 'false'})
assert.equal(page.shell.dataset.open, 'false', 'It stays closed beside the panes when it was left closed')

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

// Picking another day works like switching mail folders: the day's events
// fan out while the next day loads, and it only goes in once they're gone.
const card = name => ({name, animate() {}})
page = load(() => null)
page.swapIn([card('lunch'), card('standup')])
const nextDay = calendarContent([card('dentist')], false, '2026-03')
page.request('xhr-1')
assert.deepEqual(page.fannedOut.map(animation => animation.node.name), ['lunch', 'standup'], 'The day\'s events fan out as the next day loads')
let detail = page.respond('xhr-1', nextDay)
assert.equal(detail.shouldSwap, false, 'htmx leaves the swap to the calendar')
await Promise.resolve()
assert.equal(page.swapped.length, 0, 'The next day waits for the events to go')
page.fannedOut.forEach(animation => animation.finish())
await new Promise(resolve => setTimeout(resolve))
assert.equal(page.content(), nextDay, 'The next day goes in once the events are gone')
assert.equal(page.fadedDays.length, 0, 'Another day of the same month leaves the days alone')
assert.deepEqual([...page.fannedIn.at(-1).nodes].map(item => item.name), ['dentist'], 'Its events fan in')

// Another month: its days fade in from the side it came from.
for (const [month, direction] of [['2026-04', 1], ['2026-02', -1]]) {
  page.request('xhr-' + month)
  page.respond('xhr-' + month, calendarContent([], false, month, ['first', 'second']))
  page.fannedOut.forEach(animation => animation.finish())
  await new Promise(resolve => setTimeout(resolve))
  const fade = page.fadedDays.at(-1)
  assert.equal(fade.direction, direction, 'A month ' + (direction > 0 ? 'ahead' : 'back') + ' comes in from that side')
  assert.deepEqual(fade.nodes, ['first', 'second'])
}

// A pick that doesn't load puts the events back, whether the server says
// no or the request never gets an answer.
for (const failure of [p => p.respond('xhr-bad', null, false), p => p.fail('htmx:sendError', 'xhr-bad')]) {
  page = load(() => null)
  page.swapIn([card('lunch')])
  page.request('xhr-bad')
  failure(page)
  assert.equal(page.fannedOut[0].cancelled, true, 'The events come back')
  assert.equal(page.swapped.length, 0)
}

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

console.log('Mail calendar sidebar: hide and show slide the shell, the panel stays drawn while sliding, a second click replaces the slide, no motion just switches, small screens open and close it as a drawer, and moving to another day or month fans the events out and in and fades the days of a new month in.')
})().catch(error => {
  console.error(error)
  process.exit(1)
})
