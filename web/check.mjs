// Dependency-free checks of the browser's actual renderers and form contract.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const element = () => ({value:'',dataset:{},classList:{toggle(){}},addEventListener(){},showModal(){},close(){}});
const elements = new Map();
const sandbox = {console, URL, Date, JSON, String, Array, Set, Number, Object, Error,
 document:{querySelector(selector){if(!elements.has(selector))elements.set(selector,element());return elements.get(selector);},addEventListener(){}},
 location:{hash:'#f'},localStorage:{getItem(){return null;},setItem(){}},sessionStorage:{getItem(){return null;},setItem(){}},
 window:{addEventListener(){}},fetch:()=>new Promise(()=>{})};
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8'),sandbox);
const evaluate = s => vm.runInContext(s,sandbox);
assert.equal(evaluate("esc('<script>alert(1)</script>')"),'&lt;script&gt;alert(1)&lt;/script&gt;');
evaluate(`state={products:[{id:'p',name:'Product'}],features:[{id:'f',product_id:'p',title:'<img src=x onerror=alert(1)>',goal:'Ship safely'}],integrations:[{id:'i',feature_id:'f',title:'Integration',status:'working',position:0}],gates:[{id:'g',feature_id:'f',title:'Gate',blocking:true,result:'failed',integration_ids:['i']}],checks:[{id:'c',gate_id:'g',title:'Check'}],results:[{id:'r',check_id:'c',result:'failed',created_at:'2026-09-16T12:00:00Z',commit:'abc123',observations:'Evidence preserved',logs:'<script>bad</script>'}],memories:[{id:'m',feature_id:'f',kind:'handoff',current:'Next task',warnings:['Keep v1']}],events:[],environments:[],repositories:[],findings:[]};productID='p'`);
const html = evaluate('featureView(selectedFeature())');
for(const text of ['&lt;img src=x onerror=alert(1)&gt;','Evidence preserved','abc123','Keep v1','Create finding','Execution history'])assert.ok(html.includes(text),text);
assert.ok(!html.includes('<script>'));
assert.ok(!html.includes('style='));
assert.ok(evaluate('overview()').includes('<progress'));
for(const action of ['create_product','create_feature','create_integration','start_integration','record_progress','record_decision','record_discovery','create_gate','add_check','record_check_result','record_finding','resolve_finding','complete_integration','handoff'])assert.ok(evaluate(`forms[${JSON.stringify(action)}]`),action);
assert.ok(evaluate("inputField(field('body','Body','textarea'),'</textarea><script>bad</script>')").includes('&lt;/textarea&gt;'));
console.log('Browser renderers, escaping, evidence history, and vertical-slice form surface passed.');
// Exercise the real submit handler, including optional envelope metadata.
let capturedCommand;
let formValues = {};
sandbox.FormData = class {
 get(name){return formValues[name] ?? '';}
 getAll(name){const value=this.get(name);return Array.isArray(value)?value:[];}
};
sandbox.fetch = (_path, options) => {capturedCommand=JSON.parse(options.body);return new Promise(()=>{});};
for(const integrationID of ['', 'i']) {
 formValues={_actor:'test/agent',title:'Decision',body:'Keep compatibility',reason:'Old clients',integration_id:integrationID};
 evaluate("currentForm={action:'record_decision',attrs:{},fields:forms.record_decision.fields}");
 elements.get('#command-form').onsubmit({preventDefault(){},target:{}});
 assert.ok(capturedCommand,'submit must send a command');
 assert.equal(Object.hasOwn(capturedCommand.data,'integration_id'),false);
 assert.equal(capturedCommand.integration_id,integrationID||undefined);
 assert.equal(capturedCommand.actor,'test/agent');
}
let prevented=false, focused=false;
sandbox.document.querySelector('#main').focus=()=>{focused=true;};
elements.get('.skip').onclick({preventDefault(){prevented=true;}});
assert.ok(prevented && focused);
assert.equal(sandbox.location.hash,'#f');
console.log('Command serialization and skip-link focus regression checks passed.');
