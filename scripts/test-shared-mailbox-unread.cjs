const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// The shared mailbox counts in the sidebar follow their folders' badges as
// these change, leaving out Sent, Drafts, Spam and Trash; folded, the shared
// mailboxes' row shows them all.
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const countStart = source.indexOf('function sidebarUnreadCount(badge) {')
const countEnd = source.indexOf('\n}\n', countStart) + 3
const start = source.indexOf('var SHARED_UNCOUNTED_ROLES')
const end = source.indexOf('function syncSidebarUnreadRollups(root) {', start)
assert(countStart >= 0 && start >= 0 && end > start)

// el is a bare element: attributes, children, and the lookups the code uses.
function el(attrs, children = [], text = '') {
  const node = { attrs, children, parent: null, hidden: false, textContent: text, style: { display: '' } }
  node.getAttribute = name => (name in node.attrs ? node.attrs[name] : null)
  const attrOf = selector => selector.slice(1, -1)
  const all = (from, name, out) => {
    for (const child of from.children) {
      if (name in child.attrs) out.push(child)
      all(child, name, out)
    }
    return out
  }
  node.querySelectorAll = selector => all(node, attrOf(selector), [])
  node.querySelector = selector => node.querySelectorAll(selector)[0] || null
  node.closest = selector => {
    for (let at = node; at; at = at.parent) if (attrOf(selector) in at.attrs) return at
    return null
  }
  for (const child of children) child.parent = node
  return node
}
const badge = n => el({ 'data-sidebar-shared-unread': '' }, [], String(n))
const folderBadge = (id, n) => el({ 'data-folder-unread': id }, [], String(n))

const inbox = folderBadge('in', 3)
const trash = folderBadge('tr', 5)
const mailboxHolder = el({ 'data-sidebar-shared-unread-mailbox': 'support@example.com' }, [badge(0)])
const totalHolder = el({ 'data-sidebar-shared-unread-total': 'acc' }, [badge(0)])
const mailbox = el({ 'data-sidebar-shared-mailbox': 'support@example.com' }, [
  el({}, [mailboxHolder]),
  el({ 'data-sidebar-folder-children': 'g2' }, [
    el({ 'data-folder-shared-role': 'inbox' }, [inbox]),
    el({ 'data-folder-shared-role': 'trash' }, [trash]),
  ]),
])
const group = el({ 'data-sidebar-shared-mailboxes': 'acc', 'data-sidebar-folder-collapsed': 'true' }, [
  el({}, [totalHolder]),
  el({ 'data-sidebar-folder-children': 'g1' }, [mailbox]),
])
const root = el({}, [group])

const context = vm.createContext({})
vm.runInContext(source.slice(countStart, countEnd) + source.slice(start, end), context)
context.syncSharedMailboxUnread(root)
assert.equal(mailboxHolder.children[0].textContent, '3', 'Trash is not counted')
assert.equal(mailboxHolder.hidden, false)
assert.equal(totalHolder.children[0].textContent, '3')
assert.equal(totalHolder.hidden, false, 'folded, the shared mailboxes show their unread mail')

// Mail read elsewhere: the badges follow.
inbox.textContent = '0'
inbox.style.display = 'none'
context.syncSharedMailboxUnread(root)
assert.equal(mailboxHolder.hidden, true)
assert.equal(totalHolder.hidden, true)

// Unfolded, the total is not shown; each mailbox keeps its own.
inbox.textContent = '2'
inbox.style.display = ''
group.attrs['data-sidebar-folder-collapsed'] = 'false'
context.syncSharedMailboxUnread(root)
assert.equal(mailboxHolder.hidden, false)
assert.equal(totalHolder.hidden, true)
console.log('shared mailbox unread: ok')
