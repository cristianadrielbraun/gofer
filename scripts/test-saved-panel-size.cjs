const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/saved-panel-size.js'), 'utf8')

// Runs the early panel size script for a page at pathname with these saved
// settings, and returns the style it adds, if any.
function run(settings, pathname = '/', innerWidth = 1800) {
  let added = null
  const attributes = new Set()
  const context = vm.createContext({
    localStorage: {getItem: key => key === 'gofer:ui_settings' ? JSON.stringify(settings) : null},
    location: {pathname},
    window: {innerWidth},
    document: {
      documentElement: {classList: {add() {}}, setAttribute(name) { attributes.add(name) }},
      createElement: () => ({setAttribute() {}, textContent: ''}),
      head: {appendChild(style) { added = style.textContent }},
    },
  })
  vm.runInContext(source, context)
  return {style: added, calendarOver: attributes.has('data-mail-calendar-over'), fits: context.goferMailCalendarFits}
}

function savedStyle(settings, pathname) {
  return run(settings, pathname).style
}

// The saved sidebar width goes to the app's sidebar only. Mail's calendar
// sidebar is an aside too, and took that width while the page loaded.
const style = savedStyle({sidebar_width: '300'})
assert.match(style, /#app-shell>aside\{width:300px!important\}/, 'The app sidebar is drawn at its saved width')
assert.doesNotMatch(style, /(^|[{}])aside\{/, 'Other asides, like the calendar sidebar, keep their own width')

// Nothing saved, nothing added.
assert.equal(savedStyle({}), null)

// The calendar sidebar fits beside the panes while the reading pane keeps at
// least 600px, the width most emails are laid out for.
const fits = run({}).fits
assert.equal(fits(1275, 'side', '50%', 450), false, 'A 1536px window with half the Mail area for the list leaves the reading pane 183px')
assert.equal(fits(1700, 'side', '50%', 450), false, 'A reading pane of about 400px is too narrow for an email')
assert.equal(fits(2200, 'side', '50%', 450), true, 'A wide Mail area leaves it 645px')
assert.equal(fits(1400, 'side', '400', 300), true, 'A list saved in pixels counts as that many, leaving 695px')
assert.equal(fits(1400, 'side', '600', 300), false, 'A wider saved list leaves 495px')
assert.equal(fits(1100, 'stacked', '50%', 450), true, 'Stacked, the reading pane is the Mail area less the calendar: 650px')
assert.equal(fits(1000, 'stacked', '50%', 450), false)
assert.equal(fits(800, 'list', '50%', 450), true, 'With no reading pane, the list keeps its usual 300px')
assert.equal(fits(700, 'list', '50%', 450), false)
assert.equal(fits(2000, undefined, undefined, 300), true, 'Nothing saved is side by side with half for the list')

// The page is drawn with the calendar over the panes when it doesn't fit,
// and always on small screens.
assert.equal(run({sidebar_width: '256', mail_calendar_width: '450'}, '/', 1536).calendarOver, true)
assert.equal(run({sidebar_width: '256', mail_calendar_width: '300'}, '/', 2200).calendarOver, false, 'A 2200px window leaves the reading pane about 665px')
assert.equal(run({}, '/', 900).calendarOver, true)

console.log('Saved panel size: the saved sidebar width goes to the app sidebar only, and the calendar sidebar goes over the panes when the reading pane would be narrower than 600px.')
