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
const context = vm.createContext({})
vm.runInContext(['calendarLoadingWeekCount', 'calendarLoadingPeriodLabel'].map(helper).join('\n'), context)
for (const [month, rows] of [['2027-02', 4], ['2026-09', 5], ['2026-03', 6], ['2024-02', 5], ['2028-02', 5]]) {
  assert.equal(context.calendarLoadingWeekCount(month), rows, 'Skeleton must match the actual Monday-first row count')
}
for (const bad of ['', '2026-13', '2026-00', 'September', '2026-9']) assert.equal(context.calendarLoadingWeekCount(bad), null)
for (const [view, date, label] of [
  ['month', '2026-10', 'October 2026'], ['month', '2026-10-02', 'October 2026'],
  ['week', '2026-10-02', 'Sep 28 – Oct 4, 2026'],
  ['week', '2026-10-08', 'Oct 5–11, 2026'],
  ['week', '2026-12-31', 'Dec 28, 2026 – Jan 3, 2027'],
  ['week', '2026-03-29', 'Mar 23–29, 2026'], // DST cannot shift the weekday axis.
]) assert.equal(context.calendarLoadingPeriodLabel(view, date), label)
assert.equal(context.calendarLoadingPeriodLabel('month', 'invalid'), '')

function pane(id, width, text) {
  return {
    style: {width},
    attributes: [{name: 'id', value: id}, {name: 'class', value: 'original'}],
    childNodes: [{value: text, cloneNode() { return {value: this.value} }}],
    removeAttribute(name) { this.attributes = this.attributes.filter(attribute => attribute.name !== name) },
    setAttribute(name, value) { this.attributes.push({name, value}) },
    replaceChildren(...nodes) { this.childNodes = nodes },
  }
}
vm.runInContext(helper('replaceAppPaneContents'), context)
const root = pane('mail-list', '62%', 'real'), pending = pane('mail-list', '50%', 'pending')
pending.attributes.push({name: 'inert', value: ''}, {name: 'aria-busy', value: 'true'})
const saved = pane('mail-list', '62%', 'real')
context.replaceAppPaneContents(root, pending, true)
assert.equal(root.style.width, '62%', 'Loading must preserve the current resized width')
assert.equal(root.childNodes[0].value, 'pending')
assert.ok(root.attributes.some(attribute => attribute.name === 'inert'))
context.replaceAppPaneContents(root, saved, false)
assert.equal(root.childNodes[0].value, 'real')
assert.ok(!root.attributes.some(attribute => attribute.name === 'inert'), 'Failure recovery must restore interactive content')
assert.equal(saved.childNodes[0].value, 'real', 'Rollback must not consume its saved fragment')

const appNav = source.slice(source.indexOf('function setupSidebarAppNavToggle()'), source.indexOf('function mailMainContentClass()'))
assert.ok(source.indexOf('setupSidebarAppNavToggle()') < source.indexOf('initVirtualScroll()'), 'Request ownership must be registered before Mail starts fetching')
assert.ok(appNav.includes('}, true)'), 'App selection must run in capture, before HTMX starts loading')
assert.ok(appNav.indexOf('setSidebarAppNavMode(mode)') < appNav.indexOf('showAppSwitchPending(mode)'), 'Indicator must move before placeholders are rendered')
assert.ok(appNav.includes('xhr !== request'), 'Old app responses must not overwrite the new tab')
assert.ok(appNav.includes('goferAppPaneMode !== sidebar.dataset.sidebarAppBody'), 'Background replies from the previous app must not overwrite a skeleton or a new app')
assert.ok(appNav.includes('htmx:sendAbort'), 'Interrupted loading must have a recovery path')
console.log('Calendar loading: exact month row counts, week labels/DST, target identity, saved widths, rollback, immediate tab selection, and stale request guards passed.')
