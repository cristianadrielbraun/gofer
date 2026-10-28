const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('document.addEventListener("gofer-request-blocked"')
const end = source.indexOf('\n})', start) + 3
assert(start > 0 && end > start)
let notify
const toasts = []
vm.runInNewContext(source.slice(start, end), {
  document: {addEventListener(name, callback) {
    assert.equal(name, 'gofer-request-blocked')
    notify = callback
  }},
  showGoferToast(options) { toasts.push(options) },
})

const explanation = 'Cross-origin request blocked. Check GOFER_BASE_URL, including https:// and the port.'
// HTMX wraps a string from HX-Trigger JSON in detail.value, even on a 403.
notify({detail: {value: explanation}})
assert.equal(toasts.length, 1)
assert.equal(toasts[0].description, explanation)
assert.equal(toasts[0].title, 'Request blocked')
assert.equal(toasts[0].variant, 'error')
assert.equal(toasts[0].duration, 0, 'Configuration guidance stays visible until dismissed')
assert.equal(toasts[0].dismissible, true, 'Persistent guidance needs a close button')
notify({detail: {value: explanation}})
assert.equal(toasts[1].id, toasts[0].id, 'Repeated attempts replace the same toast')
console.log('Guard rejection notification: explanation, error style, persistent display, and stable toast ID passed.')
