const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// The Users page's row menus fetch each action's dialog with htmx; admin.js
// opens it once it's in the page.
const source = fs.readFileSync(path.join(__dirname, '../assets/js/admin.js'), 'utf8')
const start = source.indexOf("  // A person's row actions fetch their dialog with htmx.")
const end = source.indexOf('\n  })', start) + 5
assert(start > 0 && end > start, 'Missing the fetched-dialog hook')

const listeners = {}
const opened = []
const context = {
  document: { addEventListener(name, callback) { (listeners[name] = listeners[name] || []).push(callback) } },
  window: { tui: { dialog: { open(root) { opened.push(root.id) } } } },
}
vm.runInNewContext(source.slice(start, end), context)
assert.equal(listeners['htmx:afterSwap']?.length, 1, 'The hook listens for htmx swaps')
const swap = (target, detail = { target }) => listeners['htmx:afterSwap'][0]({ detail, target })

function dialogRoot(id, open) {
  return {
    id,
    querySelector(selector) {
      assert.equal(selector, '[data-tui-dialog-content]')
      return { getAttribute(name) { return name === 'data-tui-dialog-initial-open' ? String(open) : null } }
    },
  }
}
function slot(...roots) {
  return { querySelectorAll(selector) { assert.equal(selector, '[data-tui-dialog]'); return roots } }
}

swap(slot(dialogRoot('admin-user-status-sample', true)))
assert.deepEqual(opened, ['admin-user-status-sample'], 'A dialog rendered open opens when it arrives')

swap(slot(dialogRoot('admin-user-closed-sample', false)))
assert.deepEqual(opened, ['admin-user-status-sample'], 'A dialog rendered closed stays closed')

swap(slot(dialogRoot('admin-user-delete-sample', true)), {})
assert.deepEqual(opened, ['admin-user-status-sample', 'admin-user-delete-sample'], 'The event target stands in when htmx names none')

swap(null, null)
swap({})
context.window.tui = undefined
swap(slot(dialogRoot('admin-user-early-sample', true)))
assert.deepEqual(opened, ['admin-user-status-sample', 'admin-user-delete-sample'], 'Swaps without dialogs, or before templui loads, change nothing')

console.log('Admin fetched dialogs: open on arrival, closed ones stay closed, missing targets and templui are harmless.')
