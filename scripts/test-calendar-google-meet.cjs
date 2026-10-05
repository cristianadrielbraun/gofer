const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const source = fs.readFileSync(require('node:path').join(__dirname, '../assets/js/app.js'), 'utf8')
function helper(name) {
 const start = source.indexOf('function ' + name + '('), end = source.indexOf('\n}', start)
 assert(start > 0 && end > start)
 return source.slice(start, end + 2)
}
function node() { return {value:'', hidden:false, disabled:false, textContent:'', removeAttribute(name) { delete this[name] }} }
const toggle = {checked:true,disabled:false}, input = node(), status=node(), link=node(), anchor=node(), retry=node(), copy=node(), sourceInput={value:'google'}
const elements = {'[name="google_meet_draft_id"]':input,'[data-calendar-meet-status]':status,'[data-calendar-meet-link]':link,'[data-calendar-meet-url]':anchor,'[data-calendar-meet-prepare-retry]':retry,'[data-calendar-meet-copy]':copy}
const panel={hidden:true,querySelector(selector){return elements[selector]}}
const form={isConnected:true,dataset:{},querySelector(selector){return {'[name="google_meet_meeting"]':toggle,'[data-calendar-meet-preview]':panel,'[name="source_id"]':sourceInput,'[name="all_day"]':{checked:true},'[name="repeat_frequency"]':{value:'none'},'[name="start_date"]':{value:'2026-10-02'},'[name="end_date"]':{value:'2026-10-02'},'[name="timezone"]':{value:'UTC'},...elements}[selector]}}
const requests=[],timers=[],copied=[]
let ids=0,error=''
const context=vm.createContext({URLSearchParams,AbortController,window:{crypto:{randomUUID(){return 'draft-'+(++ids)}}},navigator:{clipboard:{writeText(value){copied.push(value);return Promise.resolve()}}},setTimeout(fn,ms){const timer={fn,ms};timers.push(timer);return timer},clearTimeout(timer){const i=timers.indexOf(timer);if(i!==-1)timers.splice(i,1)},fetch(url,options){return new Promise((resolve,reject)=>requests.push({url,options,resolve,reject}))},updateCalendarCreateForm(){context.updateCalendarMeetPreview(form,false)},adjustCalendarCreateAllDayRange(){},setCalendarCreateError(_form,message){error=message}})
vm.runInContext(['updateCalendarMeetPreview','prepareCalendarMeetPreview','retryCalendarMeetPreview','copyCalendarMeetLink','validateCalendarCreatePickers'].map(helper).join('\n'),context)
function respond(request,data,ok=true){request.resolve({ok,json(){return Promise.resolve(data)}})}
async function settle(){await new Promise(resolve=>setImmediate(resolve))}
;(async()=>{
 context.updateCalendarMeetPreview(form,false)
 assert.equal(panel.hidden,false)
 assert.match(status.textContent,/Generating/)
 assert.equal(input.value,'')
 assert.equal(context.validateCalendarCreatePickers(form),false)
 assert.match(error,/Wait for the Google Meet link/)
 assert.equal(requests.length,1)
 context.updateCalendarMeetPreview(form,false)
 assert.equal(requests.length,1,'Editing event details cannot allocate a second conference')
 respond(requests[0],{source_id:'google',draft_id:'draft-1',pending:true})
 await settle(); assert.equal(input.value,'');timers.shift().fn();assert.equal(requests.length,2)
 assert.equal(requests[0].options.body,requests[1].options.body,'Pending checks reuse the same draft')
 respond(requests[1],{source_id:'google',draft_id:'draft-1',join_url:'https://meet.google.com/abc-defg-hij'})
 await settle()
 assert.equal(input.value,'draft-1');assert.equal(link.hidden,false);assert.equal(anchor.href,'https://meet.google.com/abc-defg-hij');assert.equal(context.validateCalendarCreatePickers(form),true)
 context.updateCalendarMeetPreview(form,false);assert.equal(requests.length,2)
 const button={textContent:'Copy link',isConnected:true,closest(){return panel}}
 context.copyCalendarMeetLink(button);await settle();assert.deepEqual(copied,[anchor.href]);assert.equal(button.textContent,'Copied');timers.shift().fn()
 sourceInput.value='second';context.updateCalendarMeetPreview(form,false);assert.equal(input.value,'')
 toggle.checked=false;context.updateCalendarMeetPreview(form,false);assert.equal(panel.hidden,true)
 respond(requests[2],{source_id:'second',draft_id:'draft-2',join_url:'https://meet.google.com/abc-defg-hij'})
 await settle();assert.equal(input.value,'','Stale response cannot restore a switched-off meeting')
 toggle.checked=true;context.updateCalendarMeetPreview(form,false)
 requests[3].reject(new Error('Offline'));await settle();assert.equal(retry.hidden,false);assert.match(status.textContent,/Offline/)
 context.retryCalendarMeetPreview(form);assert.equal(requests[3].options.body,requests[4].options.body,'Retry must retain the resource identity')
 respond(requests[4],{source_id:'second',draft_id:'draft-3',join_url:'https://meet.google.com.evil/abc-defg-hij'})
 await settle();assert.equal(input.value,'');assert.equal(retry.hidden,false)
 form._calendarCreateUncertain=true;context.retryCalendarMeetPreview(form);assert.equal(requests.length,5,'Frozen saves cannot allocate or refresh a conference')
 form._calendarCreateUncertain=false;context.retryCalendarMeetPreview(form)
 form.isConnected=false
 respond(requests[5],{source_id:'second',draft_id:'draft-3',join_url:'https://meet.google.com/abc-defg-hij'})
 await settle();assert.equal(input.value,'','Closing the dialog ignores late responses')
 form.isConnected=true;form._calendarMeetPreview=null
 context.updateCalendarMeetPreview(form,false)
 const hanging=requests.at(-1), hangingBody=hanging.options.body
 assert.equal(timers[0].ms,35000,'Each request must have a bounded timeout')
 timers.shift().fn();await settle()
 assert.equal(hanging.options.signal.aborted,true)
 assert.equal(retry.hidden,false);assert.match(status.textContent,/timed out/)
 context.retryCalendarMeetPreview(form)
 assert.equal(requests.at(-1).options.body,hangingBody,'Timeout retry checks the same conference')
 respond(hanging,{source_id:'second',draft_id:form._calendarMeetPreview.id,join_url:'https://meet.google.com/xxx-yyyy-zzz'})
 await settle();assert.equal(input.value,'','A timed-out response cannot replace the retry')
 respond(requests.at(-1),{source_id:'second',draft_id:form._calendarMeetPreview.id,join_url:'https://meet.google.com/abc-defg-hij'})
 await settle();assert.equal(input.value,form._calendarMeetPreview.id);assert.equal(timers.length,0)
 form._calendarMeetPreview=null;context.updateCalendarMeetPreview(form,false)
 const stalledJSON=requests.at(-1)
 stalledJSON.resolve({ok:true,json(){return new Promise(()=>{})}})
 await settle();timers.shift().fn();await settle()
 assert.equal(retry.hidden,false);assert.match(status.textContent,/timed out/,'Response bodies are also bounded')
 console.log('Google Meet preview readiness, stable links, retry, copying, and stale response checks passed')
})().catch(error=>{console.error(error);process.exitCode=1})
