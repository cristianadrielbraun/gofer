const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('var _labelsDialog = null')
const end = source.indexOf('function removeLabelMessage(', start)
assert.ok(start >= 0 && end > start)

function option(name, initial, managed = true) {
  const row = {
    textContent: name,
    getAttribute() { return name },
  }
  const attrs = new Map([['data-initial', initial], ['data-state', initial]])
  return {
    checked: initial === 'all',
    indeterminate: initial === 'some',
    getAttribute(name) { return attrs.get(name) },
    setAttribute(name, value) { attrs.set(name, value) },
    closest(selector) {
      return selector === (managed ? '[data-mail-flag-color-option]' : '[data-mail-label-option]') ? row : null
    },
  }
}

function fixture(colorStates, ordinaryState = 'none') {
  const colors = Object.entries(colorStates).map(([name, state]) => option(name, state))
  const ordinary = option('Blue', ordinaryState, false)
  const apply = {disabled: true}
  const sent = []
  const root = {
    querySelectorAll(selector) { return selector.startsWith('[data-mail-flag-color-option]') ? colors : [...colors, ordinary] },
    querySelector(selector) { return selector === '[data-mail-labels-apply]' ? apply : null },
  }
  const context = vm.createContext({
    document: {getElementById() { return root }},
    window: {},
    mailActionPayload(body) { return body },
    fetch(url, req) {
      sent.push({url, body: JSON.parse(req.body)})
      return Promise.resolve({ok: true})
    },
  })
  vm.runInContext(source.slice(start, end), context)
  const scope = {kind: 'account', account_id: 'work'}
  const targets = [{id: '12', folder_id: 'work-inbox'}]
  function submit() {
    return new Promise(resolve => {
      context._labelsDialog = {targets, searchScope: scope, done: resolve}
      context.applyLabelsDialog()
    })
  }
  return {context, colors, ordinary, apply, sent, scope, targets, submit}
}

async function main() {
  const edit = fixture({blue: 'some', orange: 'some', purple: 'none'})
  const [blue, orange, purple] = edit.colors
  edit.context.cycleLabelOption(blue)
  assert.equal(blue.checked, true)
  assert.equal(orange.indeterminate, false)
  edit.context.cycleLabelOption(orange)
  assert.equal(orange.checked, true, 'another mixed color replaces the pending choice in one click')
  assert.equal(blue.checked, false)
  edit.context.cycleLabelOption(purple)
  assert.equal(purple.checked, true)
  assert.equal(orange.checked, false)
  edit.context.cycleLabelOption(edit.ordinary)
  assert.equal(edit.ordinary.checked, true)
  assert.equal(edit.apply.disabled, false)
  await edit.submit()
  assert.deepEqual(edit.sent, [
    {url: '/api/messages/label', body: {targets: edit.targets, label: '', flag_color: 'purple', search_scope: edit.scope}},
    {url: '/api/messages/label', body: {targets: edit.targets, label: 'Blue', search_scope: edit.scope}},
  ])

  const clear = fixture({blue: 'all', orange: 'none'}, 'all')
  clear.context.cycleLabelOption(clear.colors[1])
  clear.context.cycleLabelOption(clear.colors[0])
  assert.equal(clear.apply.disabled, true, 'selecting the original color cancels its replacement')
  clear.context.cycleLabelOption(clear.colors[0])
  assert.equal(clear.ordinary.checked, true)
  await clear.submit()
  assert.deepEqual(clear.sent, [
    {url: '/api/messages/unlabel', body: {targets: clear.targets, label: '', flag_color: 'blue', search_scope: clear.scope}},
  ])

  const removeFromSome = fixture({blue: 'some', orange: 'some'})
  removeFromSome.context.cycleLabelOption(removeFromSome.colors[0])
  removeFromSome.context.cycleLabelOption(removeFromSome.colors[0])
  assert.equal(removeFromSome.colors[1].indeterminate, true)
  await removeFromSome.submit()
  assert.equal(removeFromSome.sent[0].url, '/api/messages/unlabel')
  assert.equal(removeFromSome.sent[0].body.flag_color, 'blue')
}

main().catch(error => {
  process.exitCode = 1
  console.error(error)
})
