// A message's frame reports where its body's content ends, and the page sets
// the frame to that height. Some emails size themselves to the frame, with a
// height:100% table or min-height:100vh, so a taller frame made them taller and
// the frame grew without end. These tests run the body's script, from
// internal/handler/handler.go, against a pretend layout.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../internal/handler/handler.go'), 'utf8')
const match = source.slice(source.indexOf('func emailResizeScript(')).match(/fmt\.Sprintf\(`<script>([\s\S]*?)<\/script>`, emailID\)/)
assert.ok(match, 'emailResizeScript is found in handler.go')
const script = match[1].replace('%q', '"7"')

// frame loads the script into a frame of the given size. layout(width, height)
// says where the body's content ends at that frame size. Like the reader, the
// page sets the frame to each reported height, with a 100px minimum, and the
// frame's resize observer hears every change of size.
function frame(layout, {width = 600, height = 150} = {}) {
  const tasks = []
  const reports = []
  const observers = []
  const win = {
    innerWidth: width,
    innerHeight: height,
    scrollY: 0,
    addEventListener(type, callback) { if (type === 'load') tasks.push(callback) },
  }
  let content = layout
  const observe = () => observers.forEach(callback => tasks.push(() => callback([])))
  vm.runInNewContext(script, {
    window: win,
    document: {
      body: {},
      documentElement: {},
      createRange: () => ({selectNodeContents() {}, getBoundingClientRect: () => ({bottom: content(win.innerWidth, win.innerHeight)})}),
      querySelectorAll: () => [],
    },
    getComputedStyle: () => ({paddingBottom: '0px', borderBottomWidth: '0px', marginBottom: '0px'}),
    requestAnimationFrame: callback => tasks.push(callback),
    setTimeout: callback => tasks.push(callback),
    ResizeObserver: class { constructor(callback) { observers.push(callback) } observe() {} },
    parent: {
      postMessage(message) {
        reports.push(message)
        tasks.push(() => {
          const next = Math.max(100, Math.ceil(message.height))
          if (next === win.innerHeight) return
          win.innerHeight = next
          observe()
        })
      },
    },
  })
  const result = {
    reports,
    get height() { return win.innerHeight },
    // settle runs the frame until it stops reporting.
    settle(label) {
      const before = reports.length
      while (tasks.length) {
        assert.ok(reports.length - before < 50, label + ': the frame settles instead of reporting without end (now ' + win.innerHeight + 'px)')
        tasks.shift()()
      }
      return result
    },
    // resize changes the frame's width, as dragging the reader's edge does.
    resize(newWidth) {
      win.innerWidth = newWidth
      observe()
      return result
    },
    // change lays the content out differently, as an image that loads does.
    change(newLayout) {
      content = newLayout
      observe()
      return result
    },
  }
  return result
}

// A centred table with height="100%" around 400px of content.
const fillingTable = size => (width, height) => Math.max(size, height)

const table = frame(fillingTable(400)).settle('table that fills the frame')
assert.equal(table.height, 416, 'a table that fills the frame ends 16px below its content')

const wrapper = frame((width, height) => height + 16).settle('min-height:100vh wrapper')
assert.ok(wrapper.height <= 182, 'a min-height:100vh wrapper stops within one step, got ' + wrapper.height)

const ordinary = frame(() => 900).settle('ordinary body')
assert.equal(ordinary.height, 916, 'an ordinary body gets its height plus 16px')
assert.equal(ordinary.reports.length, 1, 'an ordinary body reports once')

const growing = frame(() => 100).settle('short body')
assert.equal(growing.height, 116)
growing.change(() => 750).settle('body that gets longer')
assert.equal(growing.height, 766, 'a body that gets longer still grows the frame')

const longerTable = frame(fillingTable(300)).settle('short table')
assert.equal(longerTable.height, 316)
longerTable.change(fillingTable(1500)).settle('table that gets longer')
assert.equal(longerTable.height, 1516, 'a table that fills the frame still grows when its content gets longer')

// Dragging the reader narrower and back sends a report for every width, which
// the page waits for, without adding a step each time.
const dragged = frame(fillingTable(400)).settle('table before the drag')
for (let width = 597; width >= 420; width -= 3) dragged.resize(width).settle('table at ' + width + 'px')
for (let width = 423; width <= 600; width += 3) dragged.resize(width).settle('table at ' + width + 'px')
assert.equal(dragged.height, 416, 'dragging the width does not grow a table that fills the frame')
assert.ok(dragged.reports.some(report => report.width === 420), 'the narrowest width gets a report')
assert.equal(dragged.reports.at(-1).width, 600, 'the last report is for the final width')

// Text gets longer when it wraps at a narrower width and shorter again after.
const text = frame(width => Math.round(360000 / width)).settle('text')
assert.equal(text.height, 616)
text.resize(400).settle('narrow text')
assert.equal(text.height, 916, 'narrower text grows the frame')
text.resize(600).settle('wide text')
assert.equal(text.height, 616, 'wider text shrinks it again')

// Switching the message's colors loads the body again into a frame that keeps
// its height.
let reloaded = table
for (let i = 0; i < 3; i++) reloaded = frame(fillingTable(400), {height: reloaded.height}).settle('reload ' + (i + 1))
assert.equal(reloaded.height, 416, 'loading the body again does not grow a table that fills the frame')

console.log('Email body resize tests passed')
