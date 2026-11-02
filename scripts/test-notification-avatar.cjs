const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const calls = []
let fetched = 0
let closed = 0
const drawing = {
  beginPath() {}, arc(...args) { calls.push(['arc', ...args]) }, fill() {}, save() {},
  clip() { calls.push(['clip']) }, drawImage(...args) { calls.push(['draw', ...args.slice(1)]) },
  restore() {}, stroke() { calls.push(['stroke']) },
}
const context = {
  self: { location: { href: 'https://gofer.example/settings/advanced' } },
  URL, AbortSignal, Uint8Array, btoa,
  async fetch() { fetched++; return { ok: true, async blob() { return {} } } },
  async createImageBitmap() { return { width: 250, height: 340, close() { closed++ } } },
  OffscreenCanvas: class {
    constructor(width, height) { assert.equal(width, height); assert.equal(width, 128) }
    getContext() { return drawing }
    async convertToBlob(options) { assert.equal(options.type, 'image/png'); return new Blob(['png']) }
  },
}
vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../assets/js/notification-avatar.js'), 'utf8'), context)

;(async function () {
  const render = context.self.GoferNotificationAvatar.render
  assert.equal(await render('/assets/logo.png'), '/assets/logo.png')
  assert.equal(fetched, 0, 'The Gofer logo should keep its own shape')
  const icon = await render('/assets/notification-avatar.png')
  assert.equal(icon, 'data:image/png;base64,cG5n')
  assert(calls.find(call => call[0] === 'clip'), 'Image must be clipped to the circular path')
  assert.deepEqual(calls.find(call => call[0] === 'arc'), ['arc', 64, 64, 56, 0, Math.PI * 2])
  const sampleDraw = calls.find(call => call[0] === 'draw')
  assert.equal(sampleDraw[2], 8 + 112 * (1 - 0.85) / 2 + 112 * 0.1, 'Sample must match the lowered preview position')
  assert(Math.abs(sampleDraw[3] - 112 * 0.85) < 0.001, 'Sample should be zoomed out by 15 percent')
  assert(calls.find(call => call[0] === 'stroke'), 'Circle must have a border')
  assert.equal(closed, 1, 'Decoded image should be released')
  assert.equal(await render('/assets/notification-avatar.png'), icon)
  assert.equal(fetched, 1, 'Repeated notifications should reuse the rendered icon')
  calls.length = 0
  await render('/api/avatars/sender')
  assert(calls.find(call => call[0] === 'draw')[2] < 8, 'Real avatars should keep a centered crop')
  context.fetch = async function () { throw new Error('offline') }
  assert.equal(await render('/missing'), '/assets/logo.png', 'Failure should preserve the notification with a Gofer icon')
  console.log('Circular notification avatars: crop, sample position, border, PNG output, cache, cleanup, and fallback passed.')
})().catch(function (err) { console.error(err); process.exitCode = 1 })
