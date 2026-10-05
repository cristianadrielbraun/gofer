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
function fixture(required,multiple=false) {
 const input={value:'one',defaultValue:'one',events:[],dispatchEvent(event){this.events.push(event.type)}}
 const value={textContent:'',getAttribute(){return null},classList:{add(){},remove(){}}}
 const trigger={disabled:false,attrs:{'data-tui-selectbox-required':String(required),'data-tui-selectbox-multiple':String(multiple)},getAttribute(name){return this.attrs[name]},querySelector(selector){if(selector==='input[type="hidden"]')return input;if(selector==='.select-value')return value},closest(){return container},focus(){}}
 const items=['one','two'].map((id,index)=>({attrs:{'data-tui-selectbox-value':id,'data-tui-selectbox-selected':String(index===0),'data-tui-selectbox-disabled':'false'},getAttribute(name){return this.attrs[name]},setAttribute(name,val){this.attrs[name]=val},querySelector(){return {textContent:id}},closest(selector){return selector==='.select-container'?container:content}}))
 const content={matches(){return false},querySelectorAll(selector){return selector.includes('selected="true"')?items.filter(item=>item.attrs['data-tui-selectbox-selected']==='true'):items}}
 const container={querySelector(selector){return selector==='button.select-trigger'?trigger:content}}
 const context=vm.createContext({window:{},Event:class{constructor(type){this.type=type}},setTimeout(){}})
 vm.runInContext(['getContainer','getTriggerFromContainer','getContentFromContainer','getContentFromTrigger','updateTriggerClearState','closePopover','clearFromTrigger','updateDisplayValue','toggleItem'].map(helper).join('\n'),context)
 return {context,trigger,input,items,selected(){return items.filter(item=>item.attrs['data-tui-selectbox-selected']==='true')}}
}
let f=fixture(true)
f.context.toggleItem(f.items[0]);assert.equal(f.input.value,'one');assert.equal(f.selected().length,1)
f.context.toggleItem(f.items[1]);assert.equal(f.input.value,'two');assert.equal(f.selected().length,1)
f.context.toggleItem(f.items[1]);assert.equal(f.input.value,'two');assert.equal(f.selected().length,1)
f.context.clearFromTrigger(f.trigger);assert.equal(f.input.value,'two','Required selection cannot be cleared')
for(const item of f.items)item.setAttribute('data-tui-selectbox-selected','false')
f.input.value='';f.context.updateDisplayValue(f.trigger);assert.equal(f.input.value,'one');assert.equal(f.selected().length,1,'Initialization/reset restores one option')
f=fixture(false);f.context.toggleItem(f.items[0]);assert.equal(f.input.value,'');assert.equal(f.selected().length,0,'Optional single-select behavior remains unchanged')
f.context.toggleItem(f.items[1]);f.context.clearFromTrigger(f.trigger);assert.equal(f.input.value,'')
f=fixture(false,true);f.context.toggleItem(f.items[1]);assert.equal(f.input.value,'one,two');assert.equal(f.selected().length,2)
f.context.toggleItem(f.items[0]);assert.equal(f.input.value,'two');assert.equal(f.selected().length,1,'Multi-select can still deselect individual items')
console.log('Selectbox required selection, switching, initialization/reset, and optional/multiple compatibility passed')
