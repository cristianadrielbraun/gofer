// The sidebar's menu offers Hide from sidebar on folders without a role and
// on any tag, and hiding or showing posts to the folder's or tag's
// visibility and says where to show it again. A tag's switch in Settings →
// Appearance turns back when saving fails.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
// These are inside app.js's startup function, four spaces in.
function extract(name) {
  const start = source.indexOf('    function ' + name + '(')
  assert(start >= 0, name)
  return source.slice(start, source.indexOf('\n    }\n', start) + 7)
}

// extractTop is a function at the top of app.js.
function extractTop(name) {
  const start = source.indexOf('\nfunction ' + name + '(')
  assert(start >= 0, name)
  return source.slice(start, source.indexOf('\n}\n', start) + 3)
}

const posted = []
const toasts = []
let sidebarReloads = 0
const context = vm.createContext({
  document: { documentElement: { getAttribute: () => 'classic' } },
  window: { goferRefreshMailSidebarBody: () => { sidebarReloads++ } },
  goferT: (key, args) => key + (args && args.name ? ':' + args.name : ''),
  runFolderChange: (id, failure, work) => work(),
  postFolderChange: (url, fields) => { posted.push({ url, fields }); return Promise.resolve({}) },
  folderChangeDone: (id, title, accountID, refreshList, description) => toasts.push({ title, accountID, description }),
})
vm.runInContext(extract('fillSidebarContextMenu') + extract('setSidebarVisibility'), context)

// shown is which of the menu's items fillSidebarContextMenu leaves showing.
function shown(target) {
  const items = ['hide-folder', 'hide-tag', 'rename-folder', 'rename-label'].map(action => ({
    action,
    hidden: true,
    getAttribute: () => action,
  }))
  const root = { querySelectorAll: () => items, querySelector: () => null }
  context.fillSidebarContextMenu(root, target)
  return items.filter(item => !item.hidden).map(item => item.action)
}

const folder = { kind: 'folder', accountID: 'acc', folderID: 'trips', name: 'Trips', role: 'custom' }
for (const provider of ['imap', 'outlook', 'jmap', 'local']) {
  assert.deepEqual(shown({ ...folder, provider }), ['hide-folder', 'rename-folder'], provider)
}
assert.deepEqual(shown({ ...folder, provider: 'imap', role: 'inbox', folderID: 'inbox' }), [], 'Inbox')
const tag = { kind: 'tag', tag: true, accountID: 'mail', labelID: 'receipts', folderID: 'receipts', name: 'Receipts' }
assert.deepEqual(shown({ ...tag, provider: 'gmail' }), ['hide-tag', 'rename-label'], 'Gmail label')
assert.deepEqual(shown({ ...tag, provider: 'imap' }), ['hide-tag'], 'IMAP keyword')
assert.deepEqual(shown({ ...tag, accountID: '', provider: '', unifiedTag: true, labelID: 'unified_tag_1' }), ['hide-tag'], 'a tag of all accounts')

async function main() {
  await context.setSidebarVisibility({ accountID: 'acc', folderID: 'Trips/Coast', name: 'Coast' }, false)
  await context.setSidebarVisibility({ accountID: 'mail', tag: true, labelID: 'receipts', name: 'Receipts' }, false)
  await context.setSidebarVisibility({ accountID: '', tag: true, unifiedTag: true, labelID: 'unified_tag_1', name: 'Travel' }, false)
  // From Settings → Appearance, which is where it'd be shown again.
  await context.setSidebarVisibility({ accountID: 'work', tag: true, labelID: 'clients', name: 'Clients' }, false, true)
  await context.setSidebarVisibility({ accountID: 'acc', folderID: 'Trips/Coast', name: 'Coast' }, true)
  assert.deepEqual(posted.map(p => [p.url, p.fields.visible]), [
    ['/api/folders/Trips%2FCoast/visibility', 'false'],
    ['/api/tags/receipts/visibility', 'false'],
    ['/api/unified-tags/unified_tag_1/visibility', 'false'],
    ['/api/tags/clients/visibility', 'false'],
    ['/api/folders/Trips%2FCoast/visibility', 'true'],
  ])
  assert.deepEqual(toasts.map(t => [t.title, t.accountID, t.description]), [
    ['js.folders.hidden:Coast', 'acc', 'js.folders.hidden_where'],
    ['js.folders.hidden:Receipts', 'mail', 'js.folders.hidden_tag_where'],
    ['js.folders.hidden:Travel', '', 'js.folders.hidden_tag_where'],
    ['js.folders.hidden:Clients', 'work', ''],
    ['js.folders.shown:Coast', 'acc', ''],
  ])
  // A tag of all accounts is in no account's part of the sidebar, so it
  // reloads the whole sidebar.
  assert.equal(sidebarReloads, 1)

  // The switch turns back when saving fails, and stays when it works.
  const toggle = extractTop('toggleTagSidebarVisibility')
  for (const saved of [undefined, true]) {
    const input = {
      checked: false,
      isConnected: true,
      hasAttribute: name => name === 'data-tag-visibility-unified',
      getAttribute: name => ({ 'data-tag-visibility': 'unified_tag_1', 'data-tag-visibility-name': 'Travel' })[name] || null,
    }
    let called
    const switchContext = vm.createContext({ Promise, window: { goferSetSidebarVisibility: (target, visible, quiet) => { called = { target, visible, quiet }; return Promise.resolve(saved) } } })
    vm.runInContext(toggle, switchContext)
    switchContext.toggleTagSidebarVisibility(input)
    await new Promise(resolve => setImmediate(resolve))
    assert.equal(called.visible, false)
    assert.equal(called.quiet, true)
    assert.equal(called.target.unifiedTag, true)
    assert.equal(called.target.labelID, 'unified_tag_1')
    assert.equal(input.checked, !saved, 'saved=' + saved)
  }
  console.log('folder visibility ok')
}

main().catch(err => { console.error(err); process.exit(1) })
