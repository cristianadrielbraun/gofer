// A message shown as sent ("Original") sits on a white page, like in a
// browser, so a message without its own background stays readable in dark
// Gofer themes. Dark and Light still use the theme's message colors.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const slice = (start, end) => source.slice(source.indexOf(start), source.indexOf(end))

const code = slice('function applyEmailBodyTheme(', 'function setEmailBodyFrameSrc(')

function frame(mode) {
  return {
    dataset: {emailId: '7', bodyMode: mode},
    style: {backgroundColor: '#2a2520'},
    classList: {add() {}},
  }
}

const loaded = []
const context = vm.createContext({
  URLSearchParams,
  document: {querySelector: () => null, querySelectorAll: () => []},
  clearEmailBodyError() {},
  getEmailBodyBaseTheme: () => 'dark',
  oppositeEmailBodyTheme: theme => theme === 'dark' ? 'light' : 'dark',
  defaultEmailBodyMode: () => 'dark',
  readEmailBodyPalette: theme => theme === 'dark' ? {bg: '#2a2520', fg: '#d8ccb4', link: '#d49040'} : {bg: '#f8f2e6', fg: '#2c2418', link: '#1a0dab'},
  emailBodyWantsLargeText: () => false,
  translatedEmailBodyURL: () => '/translated',
  setEmailBodyFrameSrc: (iframe, url) => loaded.push(url),
  updateEmailBodySchemeButton() {},
})
vm.runInContext(code, context)

const original = frame('original')
context.applyEmailBodyTheme(original)
assert.equal(original.style.backgroundColor, '#ffffff', 'Original sits on a white page in a dark theme')
assert.match(loaded.pop(), /mode=original/)

const dark = frame('dark')
context.applyEmailBodyTheme(dark)
assert.equal(dark.style.backgroundColor, '#2a2520', 'Dark uses the theme color')

const light = frame('light')
context.applyEmailBodyTheme(light)
assert.equal(light.style.backgroundColor, '#f8f2e6', 'Light uses the theme color')

console.log('Email body original tests passed')
