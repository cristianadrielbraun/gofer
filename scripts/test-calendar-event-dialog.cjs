const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const path = require('node:path')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
function fixture() {
 const handlers = {}, timers = new Map(), opens = [], toasts = [], aborts = []
 let timerID = 0, root = null, visible = true
 const calendar = {dataset: {calendarPeriod: 'month:2026-10'}, hasAttribute() {return false}}
 function makeRoot() {
  const listeners = {}, title = {textContent: ''}
  const dialog = {isConnected: true, open: false, attrs: new Map([['aria-busy','true']]),
   hasAttribute(name) {return this.attrs.has(name)}, removeAttribute(name) {this.attrs.delete(name)},
   querySelector() {return title}, addEventListener(name, fn) {(listeners[name] ||= []).push(fn)},
   emit(name, target = this) {for (const fn of listeners[name] || []) fn({target})},
   close() {this.open = false;this.emit('close')},
  }
  return {dialog, title, querySelector() {return dialog}, remove() {dialog.isConnected = false;if (root === this) root = null}}
 }
 const target = {replaceChildren() {if (root) root.remove();root = makeRoot()}, querySelector() {return root}}
 const template = {content: {cloneNode() {return {}}}}
 const context = vm.createContext({
  document: {body: {addEventListener(type,fn) {(handlers[type] ||= []).push(fn)}}, getElementById(id) {return {'calendar-main':calendar,'calendar-event-loading':template,'app-pane-dialogs':target,'calendar-event-details-dialog':root}[id]}},
  window: {tui: {dialog: {open(id) {opens.push(id);if (root) root.dialog.open = true}}}, htmx: {trigger(trigger,name) {aborts.push(name);fire('htmx:sendAbort',{elt:trigger,xhr:context._calendarEventRequestForTest})}}},
  DOMParser: class {parseFromString(html) {return {querySelector() {return html === 'details' ? {} : null}}}},
  _calendarSourceIsVisible() {return visible}, showGoferToast(toast) {toasts.push(toast)},
  setTimeout(fn,delay) {assert.equal(delay,250);timers.set(++timerID,fn);return timerID}, clearTimeout(id) {timers.delete(id)},
 })
 vm.runInContext(source.slice(source.indexOf('var _calendarEventRequest = null'), source.indexOf('var _calendarCreateDialogRequest = null')), context)
 function fire(type,detail) {for (const fn of handlers[type] || []) fn({detail})}
 function begin(title = 'Planning <b>literal title</b>') {
  const trigger = {dataset:{calendarSourceId:'source'},attrs:new Map(),hasAttribute(name){return name==='data-calendar-event-trigger'},setAttribute(name,value){this.attrs.set(name,value)},removeAttribute(name){this.attrs.delete(name)},querySelector(){return {textContent:title}}}
  const detail = {xhr:{status:200},elt:trigger,target}
  fire('htmx:beforeRequest',detail)
  context._calendarEventRequestForTest=detail.xhr
  return detail
 }
 function delayed() {const callbacks=[...timers.values()];timers.clear();for(const fn of callbacks) fn()}
 function response(detail,successful=true,html='details') {
  const swap={...detail,shouldSwap:successful,isError:!successful,serverResponse:html}
  fire('htmx:beforeSwap',swap)
  if(swap.shouldSwap) fire('htmx:afterSwap',swap)
  fire('htmx:afterRequest',{...detail,successful})
  return swap
 }
 return {context,calendar,opens,toasts,aborts,timers,begin,delayed,response,fire,get root(){return root},setVisible(value){visible=value}}
}
let f=fixture(), req=f.begin()
assert.equal(f.opens.length,0)
assert.equal(f.root,null,'Normal loads must not show placeholders')
f.response(req)
assert.equal(f.opens.length,1)
assert.equal(f.timers.size,0)
f.delayed();assert.equal(f.opens.length,1,'Completed fast request cannot flash a loader')

f=fixture();req=f.begin();f.delayed()
assert.equal(f.opens.length,1)
assert.equal(f.root.title.textContent,'Planning <b>literal title</b>','The clicked title must be plain text')
const nativeDialog=f.root.dialog
const swap=f.response(req)
assert.equal(swap.target,nativeDialog)
assert.equal(swap.swapOverride,'innerHTML')
assert.match(swap.selectOverride,/data-tui-dialog-panel/)
assert.equal(f.root.dialog,nativeDialog,'Loading handoff keeps the native modal open')
assert.equal(nativeDialog.hasAttribute('aria-busy'),false)
assert.equal(f.opens.length,1,'Details must not reopen or animate the backdrop a second time')
assert.equal(f.toasts.length,0)

for(const close of ['cancel','close','button','backdrop','closing']) {
 f=fixture();req=f.begin();f.delayed()
 const dialog=f.root.dialog
 if(close==='cancel') dialog.emit('cancel')
 if(close==='close') dialog.close()
 if(close==='button') dialog.emit('click',{closest(){return {}}})
 if(close==='backdrop') dialog.emit('click',dialog)
 if(close==='closing') dialog.attrs.set('data-tui-dialog-closing','true')
 assert.equal(f.response(req).shouldSwap,false,'A dismissed dialog rejects late details: '+close)
 assert.equal(f.opens.length,1,'Dismissed dialog must stay closed')
 assert.equal(f.toasts.length,0,'Cancellation must not be reported as a loading error')
}

for(const invalid of ['period','hidden','removed','replaced','abort']) {
 f=fixture();req=f.begin();f.delayed()
 const oldDialog=f.root.dialog
 if(invalid==='period') f.calendar.dataset.calendarPeriod='month:2026-11'
 if(invalid==='hidden') f.setVisible(false)
 if(invalid==='removed') oldDialog.isConnected=false
 if(invalid==='replaced') f.begin('Different event')
 if(invalid==='abort') f.fire('htmx:sendAbort',req)
 assert.equal(f.response(req).shouldSwap,false,'Reject stale loading responses: '+invalid)
 assert.ok(!oldDialog.isConnected || !oldDialog.open, "Stale modal must be closed or detached: " + invalid)
 assert.equal(f.toasts.length,0)
}
f=fixture();req=f.begin();f.calendar.dataset.calendarPeriod='month:2026-11';f.delayed()
assert.equal(f.opens.length,0,'Navigation before the delay must not open a loader')
for(const slow of [false,true]) {
 f=fixture();req=f.begin();if(slow)f.delayed();req.xhr.status=404
 f.response(req,false)
 assert.equal(f.timers.size,0)
 assert.equal(f.root,null,'Failed detail requests must not strand a modal')
 assert.equal(f.toasts.length,1);assert.match(f.toasts[0].description,/no longer available/)
}
f=fixture();req=f.begin();f.delayed();f.response(req,true,'malformed response')
assert.equal(f.root,null);assert.equal(f.toasts.length,1,'A response without the details panel cannot strand a loader')
console.log('Calendar event dialog: no fast-load flash, delayed loading, same-modal handoff, dismissal, stale responses, aborts, and failures passed')
