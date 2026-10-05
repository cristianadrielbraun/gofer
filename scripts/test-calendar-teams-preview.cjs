const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm')
const source=fs.readFileSync(require('node:path').join(__dirname,'../assets/js/app.js'),'utf8')
function helper(name){const a=source.indexOf('function '+name+'('),b=source.indexOf('\n}',a);assert(a>0&&b>a);return source.slice(a,b+2)}
function node(){return {value:'',hidden:false,disabled:false,textContent:'',removeAttribute(name){delete this[name]}}}
const toggle={checked:true,disabled:false},id=node(),status=node(),link=node(),anchor=node(),retry=node(),copy=node(),calendar={value:'outlook'}
const elements={'[name="teams_draft_id"]':id,'[data-calendar-meet-status]':status,'[data-calendar-meet-link]':link,'[data-calendar-meet-url]':anchor,'[data-calendar-meet-prepare-retry]':retry,'[data-calendar-meet-copy]':copy}
const panel={hidden:true,querySelector(selector){return elements[selector]}}
const form={isConnected:true,dataset:{},querySelector(selector){return {'[data-calendar-meeting-provider="teams_meeting"]':panel,'[name="teams_meeting"]':toggle,'[name="source_id"]':calendar,'[name="all_day"]':{checked:true},'[name="repeat_frequency"]':{value:'none'},'[name="start_date"]':{value:'2026-10-05'},'[name="end_date"]':{value:'2026-10-05'},'[name="timezone"]':{value:'UTC'},...elements}[selector]}}
const requests=[],timers=[];let ids=0,error=''
const context=vm.createContext({URLSearchParams,AbortController,window:{crypto:{randomUUID(){return 'teams-'+(++ids)}}},setTimeout(fn,ms){const timer={fn,ms};timers.push(timer);return timer},clearTimeout(timer){const i=timers.indexOf(timer);if(i!==-1)timers.splice(i,1)},fetch(url,options){if(url.endsWith('/discard')){requests.push({url,options});return Promise.resolve()};return new Promise((resolve,reject)=>requests.push({url,options,resolve,reject}))},updateCalendarCreateForm(){context.updateCalendarTeamsPreview(form,false)},adjustCalendarCreateAllDayRange(){},setCalendarCreateError(_form,message){error=message}})
vm.runInContext(['updateCalendarTeamsPreview','prepareCalendarMeetPreview','retryCalendarTeamsPreview','abandonCalendarTeamsPreview','validateCalendarCreatePickers'].map(helper).join('\n'),context)
const settle=()=>new Promise(resolve=>setImmediate(resolve))
function respond(r,data,ok=true){r.resolve({ok,json(){return Promise.resolve(data)}})}
const url='https://teams.microsoft.com/l/meetup-join/19%3ameeting_TEST/0?context=test'
;(async()=>{
 context.updateCalendarTeamsPreview(form,false)
 assert.equal(requests[0].url,'/api/calendar/teams/drafts');assert.equal(id.value,'');assert.equal(context.validateCalendarCreatePickers(form),false);assert.match(error,/Wait for the Teams link/)
 context.updateCalendarTeamsPreview(form,false);assert.equal(requests.length,1,'Editing details cannot allocate another meeting')
 respond(requests[0],{source_id:'outlook',draft_id:'teams-1',pending:true});await settle();assert.equal(id.value,'');timers.shift().fn()
 respond(requests[1],{source_id:'outlook',draft_id:'teams-1',join_url:url});await settle()
 assert.equal(id.value,'teams-1');assert.equal(anchor.href,url);assert.equal(context.validateCalendarCreatePickers(form),true)
 assert.equal(requests[0].options.body,requests[1].options.body)
 toggle.checked=false;context.updateCalendarTeamsPreview(form,false)
 assert.equal(panel.hidden,true);assert.equal(id.value,'');assert.equal(requests[2].url,'/api/calendar/teams/drafts/discard');assert.equal(requests[2].options.keepalive,true)
 toggle.checked=true;context.updateCalendarTeamsPreview(form,false)
 const hung=requests[3];timers.shift().fn();await settle();assert.equal(hung.options.signal.aborted,true);assert.match(status.textContent,/Teams.*timed out/);assert.equal(retry.hidden,false)
 context.retryCalendarTeamsPreview(form);assert.equal(requests[4].options.body,hung.options.body,'Retry must keep the same draft')
 respond(requests[4],{source_id:'outlook',draft_id:'teams-2',join_url:'https://teams.microsoft.com.evil/meet/test'});await settle();assert.equal(id.value,'');assert.equal(retry.hidden,false)
 context.retryCalendarTeamsPreview(form);respond(requests[5],{source_id:'outlook',draft_id:'teams-2',join_url:'https://teams.live.com/meet/123?foo=bar'});await settle();assert.equal(id.value,'teams-2')
 form._calendarCreateUncertain=true;const before=requests.length;context.abandonCalendarTeamsPreview(form);assert.equal(requests.length,before,'An uncertain save must never discard the provider event')
 form._calendarCreateUncertain=false;form._calendarTeamsPreview.saved=true;context.abandonCalendarTeamsPreview(form);assert.equal(requests.length,before,'A saved event must never be discarded')
 form._calendarTeamsPreview.saved=false;context.abandonCalendarTeamsPreview(form);assert.equal(requests.at(-1).url,'/api/calendar/teams/drafts/discard')
 form.dataset.calendarEventId='existing';const count=requests.length;context.updateCalendarTeamsPreview(form,false);assert.equal(requests.length,count,'Existing event editing must not reserve a replacement event')
 console.log('Teams preview readiness, stable retries, URL validation, discard, saved/uncertain protection, and existing-edit checks passed')
})().catch(error=>{console.error(error);process.exitCode=1})
