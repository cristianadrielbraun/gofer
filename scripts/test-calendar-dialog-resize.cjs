const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm')
const source=fs.readFileSync(require('node:path').join(__dirname,'../assets/js/app.js'),'utf8')
function helper(name){const a=source.indexOf('function '+name+'('),b=source.indexOf('\n}',a);assert(a>0&&b>a);return source.slice(a,b+2)}
let natural=500,visual=null,now=0,reduced=false,frameID=0
const frames=new Map(),animations=[],observers=[],listeners=new Map()
const dialog={open:true,isConnected:true,style:{height:'',transitionProperty:''},hasAttribute(){return false},addEventListener(name,fn){listeners.set(name,fn)},removeEventListener(name,fn){if(listeners.get(name)===fn)listeners.delete(name)},animate(keyframes,options){const a={keyframes,options,currentTime:0,cancelled:false,cancel(){this.cancelled=true;visual=null}};animations.push(a);visual=parseFloat(keyframes[0].height);return a}}
const body={style:{scrollbarGutter:''},children:[{},{}]}
const form={isConnected:true,closest(){return dialog},querySelector(){return body},addEventListener(name,fn){listeners.set('form:'+name,fn)},removeEventListener(name,fn){if(listeners.get('form:'+name)===fn)listeners.delete('form:'+name)}}
class Observer{constructor(fn){this.callback=fn;this.disconnected=false;this.targets=[];observers.push(this)}observe(target){this.targets.push(target)}disconnect(){this.disconnected=true}}
const context=vm.createContext({MutationObserver:Observer,ResizeObserver:Observer,performance:{now(){return now}},getComputedStyle(){return {height:String(visual!==null?visual:dialog.style.height?parseFloat(dialog.style.height):natural)+'px'}},requestAnimationFrame(fn){frames.set(++frameID,fn);return frameID},cancelAnimationFrame(id){frames.delete(id)},window:{matchMedia(){return {matches:reduced}},addEventListener(name,fn){listeners.set('window:'+name,fn)},removeEventListener(name,fn){if(listeners.get('window:'+name)===fn)listeners.delete('window:'+name)}}})
vm.runInContext(['initializeCalendarDialogResize','stopCalendarDialogResize'].map(helper).join('\n'),context)
function mutate(){form._calendarDialogResize.mutations.callback()}
function flush(){const callbacks=Array.from(frames.values());frames.clear();callbacks.forEach(fn=>fn())}
function finish(a){visual=null;a.onfinish()}
context.initializeCalendarDialogResize(form)
assert.equal(body.style.scrollbarGutter,'stable')
mutate();mutate();assert.equal(frames.size,1,'Batch form changes before the next paint');flush();assert.equal(animations.length,0,'Initialization cannot replay the dialog entrance')
natural=620;mutate();flush();let first=animations.at(-1)
assert.equal(first.keyframes[0].height,'500px');assert.equal(first.keyframes[1].height,'620px');assert.equal(first.options.duration,220)
assert.equal(first.keyframes[0].scale,undefined,'Text and controls must not be scaled')
assert.equal(dialog.style.height,'620px')
assert.equal(dialog.style.transitionProperty,'opacity, transform, scale, translate','Preserve native opacity/scale transitions')
observers[1].callback();assert.equal(frames.size,0,'Animated flex layout must not recursively retarget itself')
visual=550;now=80;natural=430;mutate();flush();let second=animations.at(-1)
assert.equal(first.cancelled,true);assert.equal(second.keyframes[0].height,'550px','Rapid controls retarget from current visible height');assert.equal(second.keyframes[1].height,'430px');assert.equal(second.options.duration,220)
visual=510;now=140;mutate();flush();let third=animations.at(-1)
assert.equal(third.options.duration,160,'Unchanged targets keep the original completion time')
finish(third);assert.equal(dialog.style.height,'');assert.equal(dialog.style.transitionProperty,'');assert.equal(form._calendarDialogResize.height,430)
reduced=true;natural=700;const before=animations.length;mutate();flush();assert.equal(animations.length,before,'Respect reduced motion');assert.equal(form._calendarDialogResize.height,700)
reduced=false;natural=480;mutate();flush();const closing=animations.at(-1);mutate();listeners.get('close')()
assert.equal(closing.cancelled,true);assert.equal(frames.size,0);assert.equal(form._calendarDialogResize,null);assert.equal(dialog.style.height,'');assert.equal(body.style.scrollbarGutter,'');assert(observers.every(o=>o.disconnected));assert.equal(listeners.size,0)
context.initializeCalendarDialogResize(form);dialog.open=false;natural=0;mutate();flush();assert.equal(form._calendarDialogResize.height,0)
context.stopCalendarDialogResize(form)
console.log('Calendar dialog resize batching, smooth retargeting, completion, intrinsic sizing, reduced motion, and cleanup passed')
