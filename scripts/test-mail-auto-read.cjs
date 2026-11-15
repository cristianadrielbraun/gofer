const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
function section(start, end) {
  const from = source.indexOf(start)
  const to = source.indexOf(end, from)
  assert(from >= 0 && to > from, 'Read handling was not found')
  return source.slice(from, to)
}

function setup({unread = false, delay = '0', search = '', controller = true, thread = false} = {}) {
  const requests = [], timers = new Map(), invalidated = []
  let timerID = 0
  const list = {filters: {unread}}
  const window = {location: {search}, GoferSettings: {get() { return delay }}}
  const context = vm.createContext({
    window, GoferSettings: window.GoferSettings, URLSearchParams,
    autoMarkReadTimer: null, autoMarkReadEmailId: null,
    document: {
      getElementById() { return controller ? {_virtualMailList: list} : null },
      querySelector(selector) { return thread && selector.includes('data-read-thread=') ? {} : null },
      querySelectorAll() { return [] },
    },
    setTimeout(callback, ms) { timers.set(++timerID, {callback, ms}); return timerID },
    clearTimeout(id) { timers.delete(id) },
    fetch(url, options) {
      requests.push({url, method: options.method})
      return Promise.resolve({json() { return Promise.resolve({is_read: true}) }})
    },
    setReadButtonIcon() {}, syncThreadReadButtons() {}, refreshSidebarUnread() {},
    invalidateMailListItem(id) { invalidated.push(id) },
  })
  vm.runInContext(section('  function mailListIsUnreadOnly()', '  function textFrom('), context)
  vm.runInContext(section('function toggleRead(', 'function setStarButtonIcon('), context)
  return {context, list, requests, timers, invalidated, window,
    open(id = '42') { context.scheduleAutoMarkRead(id, null) },
    tick() {
      for (const [id, timer] of timers) { timers.delete(id); timer.callback() }
    },
  }
}

async function main() {
  for (const delay of ['0', '2', '5', '10', 'never']) {
    for (const thread of [false, true]) {
      const app = setup({unread: true, delay, thread})
      app.open()
      app.tick()
      assert.equal(app.requests.length, 0, 'Selecting unread-only results must preserve unread state')
      assert.equal(app.timers.size, 0)
      if (thread) app.context.toggleThreadRead('42')
      else app.context.toggleRead('42')
      await new Promise(resolve => setImmediate(resolve))
      assert.deepEqual(app.requests, [{url: '/api/messages/42' + (thread ? '/thread/read' : '/read'), method: 'POST'}],
        'Manual marking remains available regardless of the automatic marking preference')
      assert.deepEqual(app.invalidated, ['42'], 'Manual marking refreshes the list item')
    }
  }

  for (const thread of [false, true]) {
    const app = setup({thread})
    app.open()
    assert.equal(app.requests[0].url, '/api/messages/42' + (thread ? '/thread/read' : '/read') + '?state=read',
      'Ordinary views still automatically mark messages and threads read')
  }
  const delayed = setup({delay: '5'})
  delayed.open()
  assert.equal(delayed.requests.length, 0)
  assert.equal([...delayed.timers.values()][0].ms, 5000)
  delayed.tick()
  assert.equal(delayed.requests.length, 1, 'Ordinary views honor the configured delay')

  const never = setup({delay: 'never'})
  never.open()
  never.tick()
  assert.equal(never.requests.length, 0)

  const switched = setup({delay: '2'})
  switched.open()
  switched.list.filters.unread = true
  switched.tick()
  assert.equal(switched.requests.length, 0, 'A pending timer must respect a newly applied unread filter')

  const selected = setup({delay: '2'})
  selected.open('first')
  selected.list.filters.unread = true
  selected.open('second')
  assert.equal(selected.timers.size, 0, 'Selecting in unread-only mode cancels an earlier timer')
  selected.tick()
  assert.equal(selected.requests.length, 0)
  selected.list.filters.unread = false
  selected.open('third')
  selected.tick()
  assert.equal(selected.requests[0].url, '/api/messages/third/read?state=read', 'Automatic marking resumes in ordinary views')

  const initial = setup({controller: false, search: '?unread=1'})
  initial.open()
  assert.equal(initial.requests.length, 0, 'Unread URLs work before the list controller initializes')
  const applied = setup({search: '?unread=1'})
  applied.open()
  assert.equal(applied.requests.length, 1, 'Applied filters take precedence over an old URL')
  const draft = setup()
  draft.window.readMailFilters = () => ({unread: true})
  draft.open()
  assert.equal(draft.requests.length, 1, 'Unapplied filter edits do not affect automatic marking')
  console.log('Mail automatic read tests passed')
}

main().catch(error => { console.error(error); process.exitCode = 1 })
