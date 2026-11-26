const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/saved-panel-size.js'), 'utf8')

// Runs the early panel size script for a page at pathname with these saved
// settings, and returns the style it adds, if any.
function savedStyle(settings, pathname = '/') {
  let added = null
  const context = vm.createContext({
    localStorage: {getItem: key => key === 'gofer:ui_settings' ? JSON.stringify(settings) : null},
    location: {pathname},
    window: {innerWidth: 1800},
    document: {
      documentElement: {classList: {add() {}}},
      createElement: () => ({setAttribute() {}, textContent: ''}),
      head: {appendChild(style) { added = style.textContent }},
    },
  })
  vm.runInContext(source, context)
  return added
}

// The saved sidebar width goes to the app's sidebar only. Mail's calendar
// sidebar is an aside too, and took that width while the page loaded.
const style = savedStyle({sidebar_width: '300'})
assert.match(style, /#app-shell>aside\{width:300px!important\}/, 'The app sidebar is drawn at its saved width')
assert.doesNotMatch(style, /(^|[{}])aside\{/, 'Other asides, like the calendar sidebar, keep their own width')

// Nothing saved, nothing added.
assert.equal(savedStyle({}), null)

console.log('Saved panel size: the saved sidebar width goes to the app sidebar only.')
