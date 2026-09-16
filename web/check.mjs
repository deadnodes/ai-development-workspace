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
vm.runInContext(fs.readFileSync(new URL('./static/external.js',import.meta.url),'utf8'),sandbox);
vm.runInContext(fs.readFileSync(new URL('./static/flow.js',import.meta.url),'utf8'),sandbox);
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

// External delivery controls preserve typed command payloads and never infer runtime success.
evaluate(`state.github_connections=[{id:'conn',product_id:'p',name:'App install',config:{owner:'example'}}];state.repository_bindings=[{id:'binding',product_id:'p',repository_id:'repo',role:'SOURCE',full_name:'example/source',default_branch:'main'}];state.component_builds=[];state.environment_bindings=[];state.git_observations=[];state.operations=[{id:'op',product_id:'p',integration_id:'i',kind:'DEPLOY',status:'GITOPS_APPLIED',phase:'APPLY',detail:'PENDING_RECONCILIATION',gitops_result:{commit_sha:fixtureSHA},artifact:{available:true,digest:'sha256:exact'}}];state.operation_steps=[{id:'step',product_id:'p',operation_id:'op',phase:'BUILD',status:'SUCCEEDED',detail:'Exact artifact evidence',evidence:{digest:'sha256:exact'}}]`);
const opsHTML=evaluate('operationsView()');
for(const text of ['PENDING_RECONCILIATION','Exact artifact evidence','sha256:exact',shaA])assert.ok(opsHTML.includes(text),text);
assert.ok(evaluate('externalIntegrationPanel(state.integrations[0])').includes('Git state unknown'));
assert.ok(evaluate('externalProductPanels()').includes('Accessible repositories'));
assert.equal(evaluate("externalFormValues('configure_environment',{environment:'env'}).allow_deploy"),false);
formValues={_actor:'test/operator',application_id:'app-a',connection_id:'conn',workflow:'build.yml',image_repository:'ghcr.io/example/app',workflow_ref:'',inputs_text:'platform=linux/amd64\nmode=dev'};
evaluate("currentForm={action:'configure_component',attrs:{},fields:forms.configure_component.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{}});
assert.equal(capturedCommand.product_id,'p');
assert.equal(Object.hasOwn(capturedCommand.data,'inputs_text'),false);
assert.deepEqual(capturedCommand.data.inputs,{platform:'linux/amd64',mode:'dev'});
formValues={_actor:'test/operator',environment_id:'env',application_id:'app-a',revision_id:'rev-a',expected_digest:''};
evaluate("currentForm={action:'deploy_integration',attrs:{integration:'i'},fields:forms.deploy_integration.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{}});
assert.equal(capturedCommand.action,'deploy_integration');
assert.equal(capturedCommand.integration_id,'i');
assert.equal(capturedCommand.data.revision_id,'rev-a');
assert.equal(Object.hasOwn(capturedCommand.data,'integration_id'),false);
assert.ok(!evaluate('forms.create_github_connection.fields').some(f=>f.name==='registry_credential_ref'));
console.log('Provider UI commands, operation evidence, and unknown-runtime rendering checks passed.');

// External systems are shared context; only relationships and affected scopes are product-owned.
evaluate(`state.external_systems=[{id:'partner',name:'Partner API',team:'Partner team',contracts:['Keep v1 compatible']}];state.system_relationships=[{id:'rel',product_id:'p',external_system_id:'partner',type:'CONSUMES'}];state.external_scopes=[{id:'scope',feature_id:'f',integration_id:'i',external_system_ids:['partner'],relationship_ids:['rel']}]`);
assert.ok(evaluate('externalSystemsPanel()').includes('Keep v1 compatible'));
assert.ok(evaluate("externalScopePanel({integration:'i'})").includes('CONSUMES'));
formValues={_actor:'test/agent',external_system_ids:['partner'],relationship_ids:['rel']};
evaluate("currentForm={action:'set_external_scope',attrs:{integration:'i'},fields:forms.set_external_scope.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{}});
assert.equal(capturedCommand.integration_id,'i');
assert.deepEqual(capturedCommand.data.external_system_ids,['partner']);
assert.deepEqual(capturedCommand.data.relationship_ids,['rel']);
formValues={_actor:'test/agent',name:'Partner',description:'Outside our product',team:'Partner team',contact:'partner@example.test',interfaces:'HTTP',contracts:'Keep v1',notes:''};
evaluate("currentForm={action:'create_external_system',attrs:{},fields:forms.create_external_system.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{}});
assert.equal(Object.hasOwn(capturedCommand,'product_id'),false);
assert.equal(Object.hasOwn(capturedCommand,'feature_id'),false);
assert.deepEqual(capturedCommand.data.contracts,['Keep v1']);
console.log('External systems instance ownership and affected-scope form checks passed.');

// Polling is restricted to active operations, never interrupts forms/focus, and preserves scroll.
sandbox.location.hash='#operations';
sandbox.document.hidden=false;
sandbox.document.activeElement={tagName:'MAIN'};
sandbox.document.querySelector('#dialog').open=false;
evaluate("currentForm=null;state.operations=[{id:'poll-op',product_id:'p',status:'PENDING'}]");
let pollRequests=0,restoredScroll;
sandbox.window.scrollX=0;sandbox.window.scrollY=360;sandbox.window.scrollTo=(x,y)=>{restoredScroll=[x,y];};
const nextState=JSON.parse(evaluate('JSON.stringify(state)'));
nextState.operations[0].status='SUCCEEDED';
sandbox.fetch=async()=>{pollRequests++;return {ok:true,json:async()=>nextState};};
await evaluate('pollOperations()');
assert.equal(pollRequests,1);
assert.equal(sandbox.location.hash,'#operations');
assert.deepEqual(restoredScroll,[0,360]);
await evaluate('pollOperations()');assert.equal(pollRequests,1,'completed work must stop polling');
evaluate("state.operations[0].status='RUNNING'");
sandbox.document.querySelector('#dialog').open=true;
await evaluate('pollOperations()');assert.equal(pollRequests,1,'open dialog must pause polling');
sandbox.document.querySelector('#dialog').open=false;
sandbox.document.activeElement={tagName:'BUTTON'};
await evaluate('pollOperations()');assert.equal(pollRequests,1,'keyboard interaction must pause polling');
sandbox.document.activeElement={tagName:'MAIN'};
sandbox.location.hash='#f';
await evaluate('pollOperations()');assert.equal(pollRequests,1,'feature route must not poll');
console.log('Bounded operation polling preserves route/scroll and pauses for dialogs, focus, and completed work.');
assert.ok(evaluate("forms.import_repository.fields.some(f=>f.name==='base_branch')"));
assert.equal(evaluate("externalFormValues('configure_component',{application:'unconfigured'}).rebuild_missing"),false);
assert.ok(evaluate("forms.configure_component.fields.some(f=>f.name==='rebuild_missing'&&f.type==='boolean')"));
console.log('Comparison base and opt-in artifact rebuild configuration checks passed.');
const attentionHTML=evaluate("attentionView([{id:'op',reason:'GIT_SYNC_FAILED',detail:'Installation access must be checked',integration_id:'i',environment_id:'env'}])");
assert.ok(attentionHTML.includes('GIT SYNC FAILED'));
assert.ok(attentionHTML.includes('Installation access must be checked'));
assert.ok(attentionHTML.includes('Open feature & integration'));
assert.ok(attentionHTML.includes('Raw attention record'));
assert.ok(evaluate('attentionView([])').includes('No attention items reported'));
console.log('Actionable attention cards include reason, evidence, entity navigation and optional raw details.');
evaluate(`state.findings=[{id:'review',feature_id:'f',title:'Codex P2',body:'<script>untrusted review</script>',severity:'P2',status:'open',integration_ids:['i'],review_source:{pull_request:5,comment:{url:'https://github.com/example/source/pull/5#discussion_r1',path:'src/api.go',commit:'reviewed-sha'}}}];`);
const reviewHTML=evaluate('featureView(selectedFeature())');
for(const text of ['Codex PR #5','src/api.go','reviewed-sha','&lt;script&gt;untrusted review&lt;/script&gt;'])assert.ok(reviewHTML.includes(text),text);
assert.ok(!reviewHTML.includes('<script>untrusted'));
console.log('Review findings retain source/commit attribution and escape untrusted comment bodies.');

// Execution flow controls keep approvals explicit and bind scenario evidence to exact outputs.
for(const action of ['reconcile_composition','prepare_release_candidate','promote_release_candidate','create_hotfix','record_runtime_observation','create_test_scenario','revise_test_scenario','record_scenario_run'])assert.ok(evaluate(`forms[${JSON.stringify(action)}]`),action);
assert.equal(evaluate("flowFormValues('prepare_release_candidate',{}).approve_main_update"),false);
assert.equal(evaluate("flowFormValues('promote_release_candidate',{}).approve"),false);
assert.equal(evaluate("flowFormValues('record_runtime_observation',{}).healthy"),false);
assert.deepEqual(Array.from(evaluate("forms.configure_environment.fields.find(f=>f.name==='purpose').options")),['DEV','TEST','PROD']);
sandbox.fetch=(_path,options)=>{capturedCommand=JSON.parse(options.body);return new Promise(()=>{});};
sandbox.location.hash='#f';
formValues={_actor:'test/operator',name:'Candidate',environment_id:'env',approve_main_update:''};
evaluate("currentForm={action:'prepare_release_candidate',attrs:{},fields:forms.prepare_release_candidate.fields}");
capturedCommand=null;
await elements.get('#command-form').onsubmit({preventDefault(){},target:{querySelectorAll(){return rows;}}});
assert.equal(capturedCommand,null,'candidate must not submit without explicit main approval');
formValues.approve_main_update='on';
elements.get('#command-form').onsubmit({preventDefault(){},target:{querySelectorAll(){return rows;}}});
assert.equal(capturedCommand.action,'prepare_release_candidate');
assert.equal(capturedCommand.product_id,'p');
assert.equal(Object.hasOwn(capturedCommand,'feature_id'),false);
assert.equal(capturedCommand.data.approve_main_update,true);
assert.deepEqual(capturedCommand.data.components[0].revision_ids,['rev-a']);
evaluate(`state.operations=[{id:'candidate',product_id:'p',kind:'RELEASE_CANDIDATE',status:'SUCCEEDED',deployment_state:'READY_FOR_VERIFICATION',child_ids:['child'],composition_snapshot:{name:'<script>candidate</script>'}},{id:'child',product_id:'p',kind:'DEPLOY',parent_id:'candidate',application_id:'app-a',environment_id:'env',status:'SUCCEEDED',build_only:true,snapshot:{revision:{head_commit:'${shaB}'}},artifact:{digest:'sha256:${'d'.repeat(64)}'}}];state.scenario_versions=[{id:'sv1',scenario_id:'scenario',version:1,product_id:'p',title:'User flow',steps:['Pay'],expected_outcomes:['One charge'],blocking:true}];state.scenario_runs=[]`);
const flowHTML=evaluate('flowProductPanel()');assert.ok(flowHTML.includes('&lt;script&gt;candidate&lt;/script&gt;'));assert.ok(flowHTML.includes('Review exact candidate promotion'));assert.ok(!flowHTML.includes('<script>candidate'));
assert.ok(evaluate('flowFeaturePanel(selectedFeature())').includes('Create hotfix integration'));
assert.ok(evaluate("flowCompositionActions({id:'composition'})").includes('reconcile_composition'));
formValues={_actor:'test/operator',approve:'on'};
evaluate("currentForm={action:'promote_release_candidate',attrs:{id:'candidate'},fields:forms.promote_release_candidate.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{}});
assert.equal(capturedCommand.id,'candidate');assert.equal(capturedCommand.data.approve,true);assert.equal(Object.hasOwn(capturedCommand,'feature_id'),false);
formValues={_actor:'test/tester',scenario_version_id:'sv1',target:'candidate:candidate',environment_id:'',result:'passed',observations:'One charge recorded',artifacts:'trace | https://example.test/trace | Test run',finding_ids:[]};
evaluate("currentForm={action:'record_scenario_run',attrs:{},fields:forms.record_scenario_run.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{querySelectorAll(){return [{dataset:{scenarioComponent:'child'},querySelector(){return {value:'Executed exact candidate in test container'};}}];}}});
assert.equal(capturedCommand.data.candidate_operation_id,'candidate');assert.equal(Object.hasOwn(capturedCommand.data,'target'),false);assert.equal(Object.hasOwn(capturedCommand.data,'environment_id'),false);
assert.equal(capturedCommand.data.components[0].source_sha,shaB);assert.equal(capturedCommand.data.components[0].operation_id,'child');assert.equal(capturedCommand.data.components[0].artifact_digest,'sha256:'+'d'.repeat(64));assert.deepEqual(capturedCommand.data.observations,['One charge recorded']);
formValues={_actor:'test/agent',title:'Fix duplicate charge',objective:'Preserve idempotency',finding_id:'review',repositories:['repo'],remaining:'Add regression check'};
evaluate("currentForm={action:'create_hotfix',attrs:{},fields:forms.create_hotfix.fields}");
elements.get('#command-form').onsubmit({preventDefault(){},target:{}});
assert.equal(capturedCommand.feature_id,'f');assert.equal(capturedCommand.data.finding_id,'review');assert.deepEqual(capturedCommand.data.remaining,['Add regression check']);
console.log('Execution flow approvals, ready-work selection, exact scenario provenance and hotfix form checks passed.');
