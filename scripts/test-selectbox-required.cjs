const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/selectbox.js'), 'utf8')
function helper(name) {
 const start=source.indexOf('  function '+name+'('),end=source.indexOf('\n  }',start)
 assert(start>=0&&end>start,'Missing '+name)
 return source.slice(start,end+4)
}
function fixture(required,multiple=false,keepOpen=false) {
 const input={value:'one',defaultValue:'one',events:[],dispatchEvent(event){this.events.push(event.type)}}
 const value={textContent:'',getAttribute(){return null},classList:{add(){},remove(){}}}
 const trigger={disabled:false,attrs:{'data-tui-selectbox-required':String(required),'data-tui-selectbox-multiple':String(multiple),'data-tui-selectbox-keep-open':keepOpen?'true':undefined},getAttribute(name){return this.attrs[name]},querySelector(selector){if(selector==='input[type="hidden"]')return input;if(selector==='.select-value')return value},closest(){return container},focus(){}}
 const items=['one','two'].map((id,index)=>({attrs:{'data-tui-selectbox-value':id,'data-tui-selectbox-selected':String(index===0),'data-tui-selectbox-disabled':'false'},getAttribute(name){return this.attrs[name]},setAttribute(name,val){this.attrs[name]=val},querySelector(){return {textContent:id}},closest(selector){return selector==='.select-container'?container:content}}))
 const closed=[]
 const content={open:false,matches(selector){return selector===':popover-open'&&this.open},querySelectorAll(selector){return selector.includes('selected="true"')?items.filter(item=>item.attrs['data-tui-selectbox-selected']==='true'):items}}
 const container={querySelector(selector){return selector==='button.select-trigger'?trigger:content}}
 const context=vm.createContext({window:{tui:{popover:{closeElement(element){closed.push(element);element.open=false}}}},Event:class{constructor(type){this.type=type}},setTimeout(){}})
 vm.runInContext(['getContainer','getTriggerFromContainer','getContentFromContainer','getContentFromTrigger','updateTriggerClearState','closePopover','clearFromTrigger','updateDisplayValue','toggleItem'].map(helper).join('\n'),context)
 return {context,trigger,input,items,content,closed,selected(){return items.filter(item=>item.attrs['data-tui-selectbox-selected']==='true')}}
}
let f=fixture(true)
f.context.toggleItem(f.items[0]);assert.equal(f.input.value,'one');assert.equal(f.selected().length,1)
f.context.toggleItem(f.items[1]);assert.equal(f.input.value,'two');assert.equal(f.selected().length,1)
f.context.toggleItem(f.items[1]);assert.equal(f.input.value,'two');assert.equal(f.selected().length,1)
f.context.clearFromTrigger(f.trigger);assert.equal(f.input.value,'two','Required selection cannot be cleared')
for(const item of f.items)item.setAttribute('data-tui-selectbox-selected','false')
f.input.value='';f.context.updateDisplayValue(f.trigger);assert.equal(f.input.value,'one');assert.equal(f.selected().length,1,'Initialization/reset restores one option')
f=fixture(true)
for(const item of f.items)item.setAttribute('data-tui-selectbox-selected','false')
f.items[0].setAttribute('data-tui-selectbox-disabled','true')
f.context.updateDisplayValue(f.trigger);assert.equal(f.input.value,'two','Disabled default is skipped')
f.items[1].setAttribute('data-tui-selectbox-selected','false')
f.items[1].setAttribute('data-tui-selectbox-disabled','true')
f.context.updateDisplayValue(f.trigger);assert.equal(f.input.value,'');assert.equal(f.selected().length,0,'All disabled choices leave the required selector empty')
f.context.toggleItem(f.items[0]);assert.equal(f.input.value,'','Disabled choice cannot be picked')
f=fixture(false);f.context.toggleItem(f.items[0]);assert.equal(f.input.value,'');assert.equal(f.selected().length,0,'Optional single-select behavior remains unchanged')
f.context.toggleItem(f.items[1]);f.context.clearFromTrigger(f.trigger);assert.equal(f.input.value,'')
f=fixture(false,true);f.context.toggleItem(f.items[1]);assert.equal(f.input.value,'one,two');assert.equal(f.selected().length,2)
f.context.toggleItem(f.items[0]);assert.equal(f.input.value,'two');assert.equal(f.selected().length,1,'Multi-select can still deselect individual items')
// A single-select's open list closes on a pick, unless it keeps open as a
// multiple-select's does.
f=fixture(true);f.content.open=true;f.context.toggleItem(f.items[1]);assert.equal(f.closed.length,1,'A single-select closes on a pick');assert.equal(f.content.open,false)
f=fixture(true,false,true);f.content.open=true;f.context.toggleItem(f.items[1]);assert.equal(f.input.value,'two');assert.equal(f.closed.length,0,'A single-select kept open stays open on a pick');assert.equal(f.content.open,true)
f.context.toggleItem(f.items[1]);assert.equal(f.input.value,'two','and, required, keeps its pick when it is picked again')
console.log('Selectbox required selection, switching, initialization/reset, optional/multiple compatibility, and keeping a list open passed')
