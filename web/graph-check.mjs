import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {JSDOM} from 'jsdom';

const html=fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8');
const tick=()=>new Promise(resolve=>setTimeout(resolve,0));
const sha='1234567890abcdef1234567890abcdef12345678';
const feature={id:'feature-one',product_id:'p',title:'Feature one',status:'active'};
const prs=[
 {id:'12',url:'https://github.com/org/api/pull/12',repository_id:'api',integration_ids:['i'],title:'Merged API',status:'MERGED',source_branch:'feature/api',target_branch:'dev',head_sha:sha,merge_sha:'abcdef1234567890',merged_at:'2026-09-10T12:00:00Z',observed_at:'2026-09-16T12:00:00Z',evidence:'recorded'},
 {id:'13',url:'https://github.com/org/api/pull/13',repository_id:'api',integration_ids:['i'],title:'Release API',status:'OPEN',source_branch:'dev',target_branch:'main',head_sha:sha,evidence:'recorded'},
 {id:'14',url:'javascript:alert(1)',repository_id:'api',integration_ids:['i'],title:'<img src=x onerror=alert(1)>',status:'CLOSED',source_branch:'<script>bad()</script>',target_branch:'main',evidence:'recorded'},
];
const revision={id:'rev',repository_id:'api',integration_id:'i',branch:'feature/api',base_commit:'aaaa0000',head_commit:sha};
const fixture={feature,repositories:[
 {repository:{id:'api',name:'org/api',url:'https://github.com/org/api',role:'APPLICATION'},integrations:['i'],branches:[],pull_requests:prs,commits:[sha]},
 {repository:{id:'ui',name:'org/ui',url:'https://github.com/org/ui'},integrations:['j'],branches:[],pull_requests:[],commits:[]},
],integrations:[{id:'i',title:'API work'},{id:'j',title:'UI work'}],environments:[{id:'dev',name:'DEV'}],revisions:[revision],compositions:[{id:'slice',name:'API test slice',status:'planned',environment_id:'dev',created_at:'2026-09-16T12:00:00Z',components:[{base_ref:'main',base_commit:'aaaa0000',revision_ids:['rev'],target_branch:'generated/dev'}],revision_snapshots:[revision]}],builds:[],artifacts:[],deployments:[
 {operation_id:'op-pending',repository_id:'api',integration_ids:['i'],environment_id:'dev',deployment_state:'GITOPS_APPLIED',source_commit:sha,digest:'sha256:expected',gitops_commit:'bbbb0000',runtime_observations:[]},
 {operation_id:'op-observed',repository_id:'ui',integration_ids:['j'],environment_id:'dev',deployment_state:'DEPLOYED',source_commit:sha,digest:'sha256:actual',gitops_commit:'cccc0000',runtime_observations:[{healthy:true,created_at:'2026-09-16T12:00:00Z',details:'Pod checked',artifact_digest:'sha256:actual'}]},
],reported_snapshots:[{memory_id:'report',reported_at:'2026-09-12',cluster:'Old production',namespace:'prod',reported_health:'Healthy per historical report',limits:'Not re-observed',components:[{repository_id:'api',name:'org/api',commit:sha,url:'https://github.com/org/api/commit/'+sha}]}]};
function browser(){
 const dom=new JSDOM(html,{url:'http://localhost/?product=p#feature-one',runScripts:'outside-only'}),w=dom.window;
 const pending=[];
 w.fetch=path=>path.endsWith('/graph')?new Promise((resolve,reject)=>pending.push({path,resolve,reject})):new Promise(()=>{});
 const run=s=>vm.runInContext(s,dom.getInternalVMContext());
 for(const name of ['app','external','live','graph'])run(fs.readFileSync(new URL(`./static/${name}.js`,import.meta.url),'utf8'));
 w.fixtureState={products:[{id:'p',name:'Product'}],features:[feature,{id:'feature-two',product_id:'p',title:'Feature two',status:'active'}]};
 run('state=fixtureState;render()');
 const click=s=>{const e=w.document.querySelector(s);assert.ok(e,`Missing ${s}`);e.click();};
 const change=(s,value)=>{const e=w.document.querySelector(s);e.value=value;e.dispatchEvent(new w.Event('change',{bubbles:true}));};
 const text=()=>w.document.querySelector('#main').textContent;
 return {dom,w,run,pending,click,change,text};
}
const b=browser();
try{
 await tick();assert.equal(b.pending.length,1);assert.equal(b.pending[0].path,'/api/features/feature-one/graph');assert.match(b.text(),/Loading repositories/);
 b.pending.shift().resolve({ok:true,json:async()=>fixture});await tick();
 assert.equal(b.w.document.querySelectorAll('.graph-lane:not(.graph-lane-labels)').length,2);
 assert.match(b.text(),/Historical report/);assert.match(b.text(),/Merge ≠ deployment/);
 b.click('[data-graph-repo="api"]');
 assert.equal(b.w.document.querySelector('#graph-repository').value,'api','drilldown keeps visible repository selector aligned');
 assert.equal(b.w.document.querySelectorAll('.graph-pr-row').length,3);
 assert.match(b.text(),/feature\/api/);assert.match(b.text(),/Merged into/);assert.match(b.text(),/Proposed target/);
 assert.equal(b.w.document.querySelector('#main img'),null);assert.equal(b.w.document.querySelector('#main script'),null);
 b.change('#graph-status','open');assert.equal(b.w.document.querySelectorAll('.graph-pr-row').length,1);assert.match(b.text(),/Release API/);assert.doesNotMatch(b.text(),/Merged API/);
 b.change('#graph-status','merged');assert.equal(b.w.document.querySelectorAll('.graph-pr-row').length,1);
 b.click('[data-graph-pr]');assert.match(b.w.document.querySelector('.graph-inspector').textContent,new RegExp(sha));assert.match(b.w.document.querySelector('.graph-inspector').textContent,/API work/);
 assert.equal(b.w.document.querySelector('.graph-inspector a').rel,'noopener noreferrer');
 b.w.document.dispatchEvent(new b.w.KeyboardEvent('keydown',{key:'Escape',bubbles:true}));assert.equal(b.w.document.querySelector('.graph-inspector'),null);
 b.change('#graph-status','closed');b.click('[data-graph-pr]');assert.equal(b.w.document.querySelector('.graph-inspector a'),null,'unsafe Git URL stays plain text');assert.equal(b.w.document.querySelector('#main [onerror]'),null);
 b.click('[data-graph-tab="slices"]');assert.match(b.text(),/API test slice/);assert.match(b.text(),/generated\/dev/);assert.match(b.text(),/source plan, not proof of deployment/);assert.match(b.text(),/Reported · not live/);
 b.click('[data-graph-tab="deployments"]');assert.match(b.text(),/op-pending/);assert.doesNotMatch(b.text(),/op-observed/);assert.match(b.text(),/Runtime unknown/);assert.match(b.text(),/Reported · not live/);
 b.change('#graph-repository','');assert.match(b.text(),/Observed healthy/);assert.match(b.text(),/Pod checked/);assert.match(b.text(),/Runtime unknown/);
 b.click('[data-graph-tab="work"]');assert.match(b.text(),/Implementation plan/);assert.match(b.text(),/Development memory/);
 // A failed refresh retains its prior snapshot and exposes an explicit retry.
 b.click('[data-graph-tab="map"]');b.click('[data-graph-refresh]');assert.equal(b.pending.length,1);b.pending.shift().reject(new Error('Graph offline'));await tick();
 assert.match(b.text(),/Graph offline/);assert.match(b.text(),/last successful snapshot/);
 b.click('[data-graph-refresh]');b.pending.shift().resolve({ok:true,json:async()=>fixture});await tick();assert.doesNotMatch(b.text(),/Graph offline/);
 // A slow response from the previous feature must not repaint the new route.
 b.click('[data-graph-refresh]');const stale=b.pending.shift();
 b.w.history.replaceState(null,'','/?product=p#feature-two');b.run('render()');await tick();
 assert.equal(b.pending.length,1);stale.resolve({ok:true,json:async()=>fixture});await tick();assert.match(b.text(),/Feature two/);assert.doesNotMatch(b.text(),/Feature one/);
 b.pending.shift().resolve({ok:true,json:async()=>({...fixture,feature:{...feature,id:'feature-two',title:'Feature two'},repositories:[],deployments:[],reported_snapshots:[]})});await tick();assert.match(b.text(),/Feature two/);assert.doesNotMatch(b.text(),/org\/api/);
}finally{b.dom.window.close();}
const offline=browser();
try{await tick();offline.pending.shift().resolve({ok:false,json:async()=>({error:'No graph'})});await tick();assert.match(offline.text(),/Graph unavailable/);offline.click('[data-graph-refresh]');offline.pending.shift().resolve({ok:true,json:async()=>fixture});await tick();assert.match(offline.text(),/Repositories/);assert.doesNotMatch(offline.text(),/Graph unavailable/);}finally{offline.dom.window.close();}
console.log('Feature graph: API loading/retry, navigation races, repository/PR filters, evidence separation, slices and safe links passed.');
