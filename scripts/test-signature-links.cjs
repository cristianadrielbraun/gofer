const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const read = name => fs.readFileSync(path.join(__dirname, '../assets/js', name), 'utf8')
const app = read('app.js')
const settings = read('settings.js')
const slice = (source, start, end) => {
  const from = source.indexOf(start)
  assert.ok(from >= 0, `${start} is missing`)
  return source.slice(from, source.indexOf(end, from))
}

// A link element with just what the two cleaners touch.
const link = href => {
  const attrs = []
  const el = {
    tagName: 'A',
    attributes: attrs,
    getAttribute: name => (attrs.find(a => a.name === name) || {}).value ?? null,
    hasAttribute: name => attrs.some(a => a.name === name),
    setAttribute(name, value) {
      const found = attrs.find(a => a.name === name)
      if (found) found.value = String(value)
      else attrs.push({name, value: String(value)})
    },
    removeAttribute(name) {
      const i = attrs.findIndex(a => a.name === name)
      if (i >= 0) attrs.splice(i, 1)
    },
  }
  if (href !== null) el.setAttribute('href', href)
  return el
}
const root = el => ({
  querySelectorAll: () => [],
  get innerHTML() { return el.getAttribute('href') },
})
const walkerOver = el => () => {
  let done = false
  return {currentNode: el, nextNode: () => (done ? false : (done = true))}
}

let current = null
const context = vm.createContext({
  NodeFilter: {SHOW_ELEMENT: 1},
  DOMParser: class {
    parseFromString() { return {body: root(current), createTreeWalker: walkerOver(current)} }
  },
  document: {
    createElement: () => {
      const template = root(current)
      template.content = root(current)
      return template
    },
    createTreeWalker: (...args) => walkerOver(current)(...args),
  },
  _inlineComposeStyleRules() {},
})
const helper = app.includes('// _safeLinkHref answers') ? slice(app, '// _safeLinkHref answers', '// _normalizeLinkAddress turns') : ''
vm.runInContext(
  helper +
    slice(app, 'function _sanitizeComposeHTML(', 'function _composeEditorText(') +
    slice(settings, 'function sanitizeSignatureStyle(', 'function setSignatureModeButtonState('),
  context,
)

const cleaners = {
  'signature editor': href => { current = link(href); return context.sanitizeSignatureHTML('') },
  compose: href => { current = link(href); return context._sanitizeComposeHTML('') },
}

for (const [name, clean] of Object.entries(cleaners)) {
  // Mail, phone and web links stay, as the server keeps them.
  assert.equal(clean('mailto:alex@example.com'), 'mailto:alex@example.com', name)
  assert.equal(clean('MAILTO:alex@example.com?subject=Lunch%20on%20Friday%3F'), 'MAILTO:alex@example.com?subject=Lunch%20on%20Friday%3F', name)
  assert.equal(clean('tel:+1-555-0100'), 'tel:+1-555-0100', name)
  assert.equal(clean('https://example.com/about'), 'https://example.com/about', name)
  assert.equal(clean('//example.com'), 'https://example.com', name)
  assert.equal(clean('#top'), '#top', name)

  // Spaces around the address and line breaks inside it are left out, as a
  // browser or mail app reads the link, rather than the link dropped.
  assert.equal(clean(' mailto:alex@example.com '), 'mailto:alex@example.com', name)
  assert.equal(clean('\n  mailto:alex@\nexample.com'), 'mailto:alex@example.com', name)
  assert.equal(clean('\ttel:+1-555-0100'), 'tel:+1-555-0100', name)

  // Anything else loses its link.
  assert.equal(clean('javascript:alert(1)'), null, name)
  assert.equal(clean(' java\nscript:alert(1)'), null, name)
  assert.equal(clean('data:text/html,hi'), null, name)
  assert.equal(clean('/settings'), null, name)
  assert.equal(clean('www.example.com'), null, name)
}

console.log('signature links ok')
