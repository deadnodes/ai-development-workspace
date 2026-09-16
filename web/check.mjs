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

// Composition planning serializes multiple independent application rows and keeps immutable snapshots visible.
const shaA='a'.repeat(40),shaB='b'.repeat(40);
sandbox.fixtureSHA=shaA;
evaluate(`state.applications=[{id:'app-a',product_id:'p',name:'API',repository_id:'repo',path:'api'},{id:'app-b',product_id:'p',name:'Worker',repository_id:'repo',path:'worker'}];state.integration_revisions=[{id:'rev-a',product_id:'p',feature_id:'f',integration_id:'i',repository_id:'repo',head_commit:fixtureSHA,base_commit:fixtureSHA,commits:[fixtureSHA]}];state.environments=[{id:'env',product_id:'p',name:'QA West',cluster:'west',namespace:'qa',desired_composition_id:'composition'}];state.compositions=[{id:'composition',product_id:'p',name:'QA selection',environment_id:'env',environment_snapshot:{id:'env',name:'Original QA',cluster:'old-cluster'},application_snapshots:[{id:'app-a',name:'Captured API',repository_id:'repo',path:'api'}],revision_snapshots:[{id:'rev-a',integration_id:'i',branch:'feature/example',base_commit:fixtureSHA,head_commit:fixtureSHA,commits:[fixtureSHA]}],components:[{application_id:'app-a',base_ref:'main',base_commit:fixtureSHA,target_branch:'generated/qa',revision_ids:['rev-a']}]}]`);
const compositionHTML=evaluate("compositionView(productItems('compositions'))");
for(const text of ['Original QA','old-cluster','Captured API',shaA,'Planned / not deployed','Selected as desired'])assert.ok(compositionHTML.includes(text),text);
assert.ok(!compositionHTML.includes('Current API'));
assert.ok(evaluate("environmentView(productItems('environments'))").includes('QA West'));
assert.ok(evaluate("revisionLabel(state.integration_revisions[0])").includes(shaA));
assert.ok(evaluate("revisionView('i')").includes(shaA));
const rowFixture=(appID,base,revisionIDs)=>({querySelector(selector){const key=selector.match(/="(.*?)"/)[1];return key==='revision_ids'?{selectedOptions:revisionIDs.map(value=>({value}))}:{value:{application_id:appID,base_ref:'main',base_commit:base,target_branch:'generated/qa'}[key]};}});
const rows=[rowFixture('app-a',shaA,['rev-a']),rowFixture('app-b',shaA,['rev-a'])];
formValues={_actor:'test/agent',name:'Multi-app QA',environment_id:'env'};
evaluate("currentForm={action:'plan_composition',attrs:{},fields:forms.plan_composition.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{querySelectorAll(){return rows;}}});
assert.equal(capturedCommand.action,'plan_composition');
assert.equal(capturedCommand.product_id,'p');
assert.deepEqual(capturedCommand.data.components.map(c=>c.application_id),['app-a','app-b']);
assert.deepEqual(capturedCommand.data.components[0].revision_ids,['rev-a']);
assert.equal(capturedCommand.data.components[1].base_commit,shaA);
formValues={_actor:'test/agent'};
evaluate("currentForm={action:'select_composition',attrs:{id:'composition'},fields:forms.select_composition.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{}});
assert.equal(capturedCommand.id,'composition');
assert.deepEqual(capturedCommand.data,{});
formValues={_actor:'test/agent',repository_id:'repo',branch:'feature/a',base_commit:shaA,head_commit:shaB,commits:shaB};
evaluate("currentForm={action:'record_integration_revision',attrs:{integration:'i'},fields:forms.record_integration_revision.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{}});
assert.equal(capturedCommand.integration_id,'i');
assert.equal(capturedCommand.data.head_commit,shaB);
assert.deepEqual(capturedCommand.data.commits,[shaB]);
for(const action of ['create_application','update_environment','record_integration_revision','plan_composition','select_composition'])assert.ok(evaluate(`forms[${JSON.stringify(action)}]`),action);
console.log('Multi-application composition, immutable snapshots, source revision and desired selection checks passed.');
