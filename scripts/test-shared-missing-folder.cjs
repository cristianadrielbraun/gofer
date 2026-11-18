const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const loadI18n = require('./i18n-test-helper.cjs')

// A mail action held back because a shared mailbox lacks the folder says so,
// offers no folder of the account's own to choose, and keeps the way around
// it, such as deleting for good.
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('function missingFolderFromResponse(response) {')
const end = source.indexOf('// openFolderRolesSettings goes to', start)
assert(start >= 0 && end > start)

const toasts = []
const context = vm.createContext(loadI18n({ showGoferToast(toast) { toasts.push(toast) } }))
vm.runInContext(source.slice(start, end), context)
const response = value => ({ headers: { get: name => (name === 'X-Gofer-Missing-Folder' ? value : null) } })

assert.deepEqual({ ...context.missingFolderFromResponse(response('acc trash')) }, { account: 'acc', role: 'trash' })
const shared = context.missingFolderFromResponse(response('acc trash shared'))
assert.deepEqual({ ...shared }, { account: 'acc', role: 'trash', shared: true })
assert.equal(context.missingFolderFromResponse(response('acc trash other')), null)

context.showMissingFolderToast(shared, 'Not deleted', { secondaryActionLabel: 'Delete permanently', onSecondaryAction() {}, description: 'replaced' })
const toast = toasts.pop()
assert.equal(toast.description, 'This shared mailbox has no Trash folder. Its mail stays in the shared mailbox.')
assert.equal(toast.actionLabel, undefined, 'no folder of the account is offered')
assert.equal(toast.secondaryActionLabel, 'Delete permanently')

context.showMissingFolderToast({ account: 'acc', role: 'trash' }, 'Not deleted')
assert.equal(toasts.pop().actionLabel, 'Choose a Trash folder')
console.log('shared missing folder: ok')
