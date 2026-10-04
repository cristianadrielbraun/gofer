const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const read = name => fs.readFileSync(path.join(__dirname, '../assets/js', name), 'utf8')
const app = read('app.js'), settings = read('settings.js')
const entryStart = app.indexOf('function animateSectionContent(')
const entryEnd = app.indexOf('\ndocument.addEventListener(', entryStart)
const filterStart = app.indexOf('    function switchAdvancedFilterPanel(')
const filterEnd = app.indexOf('    function scheduleActivePillOverflow(', filterStart)
const settingsStart = settings.indexOf('document.body.addEventListener("htmx:afterSwap"')
const settingsEnd = settings.indexOf('document.body.addEventListener("htmx:afterSettle"', settingsStart)
assert(entryStart >= 0 && entryEnd > entryStart && filterStart >= 0 && filterEnd > filterStart)
assert(settingsStart >= 0 && settingsEnd > settingsStart)

function node(attributes = {}, hidden = false) {
  const classes = new Set(hidden ? ['hidden'] : [])
  return {
    style: {}, animations: [],
    classList: {
      contains(name) { return classes.has(name) },
      toggle(name, active) { if (active) classes.add(name); else classes.delete(name) },
      add(...names) { names.forEach(name => classes.add(name)) },
      remove(...names) { names.forEach(name => classes.delete(name)) },
    },
    getAttribute(name) { return attributes[name] ?? null },
    setAttribute(name, value) { attributes[name] = value },
    hasAttribute(name) { return Object.hasOwn(attributes, name) },
    animate(frames, options) { this.animations.push(JSON.parse(JSON.stringify({frames, options}))) },
  }
}

for (const bundle of ['tabs.js', 'tabs.min.js']) {
  let reduced = false
  const handlers = {}, timers = []
  const box = node()
  box.getBoundingClientRect = () => ({height: 300})
  const filterPanels = ['calendar', 'status'].map((name, index) => {
    const panel = node({'data-mail-filter-panel': name}, index !== 0)
    panel.closest = () => box
    return panel
  })
  const filterButtons = ['calendar', 'status'].map(name => node({'data-mail-filter-panel-button': name}))
  const tabPanels = ['folders', 'sync'].map((name, index) => node({'data-tui-tabs-value': name}, index !== 0))
  const tabs = node({'data-tui-tabs-animate-content': '', 'data-tui-tabs-local': ''})
  tabs.querySelector = () => null
  tabs.querySelectorAll = selector => selector.includes('data-tui-tabs-content') ? tabPanels : []
  const document = {
    querySelector: () => tabs,
    querySelectorAll(selector) {
      if (selector === '[data-mail-filter-panel]') return filterPanels
      if (selector === '[data-mail-filter-panel-button]') return filterButtons
      return []
    },
    addEventListener() {},
    body: {addEventListener(type, callback) { (handlers[type] ||= []).push(callback) }},
    fonts: {ready: {then() {}}},
  }
  const window = {
    matchMedia() { return {matches: reduced} },
    setTimeout(callback) { timers.push(callback); return timers.length },
    clearTimeout() {}, addEventListener() {}, location: {pathname: '/settings/sync'},
  }
  const context = vm.createContext({document, window, MutationObserver: class { observe() {} }})
  vm.runInContext(app.slice(entryStart, entryEnd) + app.slice(filterStart, filterEnd), context)
  vm.runInContext(settings.slice(settingsStart, settingsEnd), context)
  vm.runInContext(read(bundle), context)
  function swap(section, verb = 'get') {
    for (const handler of handlers['htmx:afterSwap']) handler({target: section, detail: {requestConfig: {verb}}})
  }

  context.switchAdvancedFilterPanel(filterButtons[1])
  assert.equal(filterPanels[0].classList.contains('hidden'), true, 'Filter panels switch immediately')
  assert.equal(filterPanels[1].classList.contains('hidden'), false)
  const reference = filterPanels[1].animations[0]
  assert.deepEqual(reference, {
    frames: [{opacity: 0, transform: 'translateY(5px)'}, {opacity: 1, transform: 'translateY(0)'}],
    options: {duration: 180, easing: 'ease-out'},
  })

  const section = node()
  section.id = 'settings-content'
  const before = timers.length
  swap(section)
  assert.deepEqual(section.animations[0], reference, 'Settings navigation uses the exact filter entry effect')
  assert.equal(timers.length, before, 'Settings navigation adds no animation timers or swap delays')
  swap(section, 'post')
  assert.equal(section.animations.length, 1, 'Saving settings does not replay the section transition')
  const nested = node()
  nested.id = 'mail-operations-content'
  swap(nested)
  assert.equal(nested.animations.length, 0, 'Background content refreshes remain untouched')

  window.tui.tabs.setActive('email-sync-folder-tabs', 'sync')
  assert.equal(tabPanels[0].classList.contains('hidden'), true, 'Settings tabs hide the old section immediately')
  assert.equal(tabPanels[1].classList.contains('hidden'), false)
  assert.deepEqual(tabPanels[1].animations[0], reference)
  assert.equal(timers.length, before, 'Local settings tabs do not schedule exit-fade timers')
  window.tui.tabs.setActive('email-sync-folder-tabs', 'sync')
  assert.equal(tabPanels[1].animations.length, 1, 'Selecting the current tab does not restart the animation')
  window.tui.tabs.setActive('email-sync-folder-tabs', 'folders')
  window.tui.tabs.setActive('email-sync-folder-tabs', 'sync')
  assert.equal(tabPanels[0].classList.contains('hidden'), true, 'Rapid switching leaves only the latest tab visible')

  reduced = true
  swap(section)
  assert.equal(section.animations.length, 1, 'Settings respects reduced motion')
  window.tui.tabs.setActive('email-sync-folder-tabs', 'folders')
  assert.equal(tabPanels[0].animations.length, 1, 'Reduced motion switches tabs without animation')
  context.switchAdvancedFilterPanel(filterButtons[0])
  assert.equal(filterPanels[0].animations.length, 0, 'Filters keep their reduced-motion behavior')
  context.animateSectionContent(null)
  context.animateSectionContent({})
}
console.log('Section transitions: filters, settings navigation, local settings tabs, rapid switching, reduced motion, and both tab bundles passed.')
