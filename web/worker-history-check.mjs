import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {TextEncoder} from 'node:util';
import {JSDOM} from 'jsdom';

const dom=new JSDOM(fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8'),{url:'http://localhost/?product=p',runScripts:'outside-only',pretendToBeVisual:true});
const w=dom.window,run=s=>vm.runInContext(s,dom.getInternalVMContext()),tick=()=>new Promise(resolve=>setTimeout(resolve,0));
w.fetch=()=>new Promise(()=>{});w.setInterval=()=>0;w.TextEncoder=TextEncoder;
for(const file of ['app','worker-history','live'])run(fs.readFileSync(new URL(`./static/${file}.js`,import.meta.url),'utf8'));
const requests=[];
w.fetch=(path,options)=>new Promise((resolve,reject)=>requests.push({path,options,resolve,reject}));
const pending=()=>requests.at(-1);
function reply(request,items=[],more=false,status=200,overrides={}){
 const q=new URL(request.path,'http://localhost').searchParams;
 const data={projection:'native_worker_history',product_id:q.get('product_id'),feature_id:q.get('feature_id')||'',job_id:q.get('job_id')||'',collection:q.get('collection'),items,pagination:{has_more:more,next_cursor:more?'opaque+cursor/==':''},...overrides};
 request.resolve({ok:status===200,status,text:async()=>JSON.stringify(data)});
}
const click=selector=>{const element=w.document.querySelector(selector);assert.ok(element,selector);element.click();};
const text=()=>w.document.querySelector('#worker-history')?.textContent||'';
const control=(action,collection='jobs')=>`[data-worker-history="${action}"][data-collection="${collection}"]`;
const job=(id,extra={})=>({id,product_id:'p',recorded_status:'FAILED',capability:'local_oci_build',attempts:3,links:2,provenance:'recorded_snapshot_unverified',...extra});
try{
 run(`state={products:[{id:'p',name:'Synthetic Product'},{id:'q',name:'Other'}],features:[{id:'f',product_id:'p',title:'Feature <script>bad</script>'}]};productID='p';render()`);
 assert.match(text(),/Native worker history/);assert.equal(requests.length,0,'render never reads history');
 click('[data-worker-history="toggle"]');assert.equal(requests.length,1);assert.match(text(),/Loading jobs/);
 assert.equal(new URL(pending().path,'http://x').searchParams.get('limit'),'10');assert.equal(pending().options.method,'GET');assert.equal(pending().options.cache,'no-store');
 const malicious='j<&"';reply(pending(),[job(malicious,{source_sha:'a'.repeat(40),created_at:'bad date',capability:'<img src=x onerror=alert(1)>'})],true);await tick();
 assert.match(text(),/Unknown \(invalid recorded timestamp\)/);assert.match(text(),/a{40}/);assert.equal(w.document.querySelector('#worker-history img'),null);
 assert.ok(w.document.querySelector('[data-job]').dataset.job===malicious,'opaque IDs survive escaping');
 assert.match(text(),/not verified build success/);assert.doesNotMatch(text(),/latest/i);
 const count=requests.length;run('render(true);render(true)');assert.equal(requests.length,count,'live renders do not refetch');
 click('[data-worker-history="select"]');assert.equal(requests.length,count+2);
 const attempts=requests.at(-2),links=requests.at(-1);
 assert.equal(new URL(links.path,'http://x').searchParams.get('job_id'),malicious);
 reply(attempts,[{id:'a1',product_id:'p',job_id:malicious,recorded_status:'EXPIRED'}],true);
 reply(links,[{id:'old-revision',product_id:'p',job_id:malicious,feature_id:'other-feature<&',revision_id:'old-revision',integration_id:'i',repository_id:'r',source_sha:'old-sha',provenance:'recorded_snapshot_unverified'}],true);await tick();
 assert.match(text(),/EXPIRED/);assert.match(text(),/old-sha/);
 const a=w.document.querySelector('#worker-history a');assert.equal(new URL(a.href).hash,'#other-feature%3C%26');
 assert.equal(a.textContent,'other-feature<&','missing Feature title uses exact ID');
 click(control('next','attempts'));assert.equal(new URL(pending().path,'http://x').searchParams.get('cursor'),'opaque+cursor/==');
 reply(pending(),[{id:'a2',product_id:'p',job_id:malicious,recorded_status:'CANCELLED'}]);await tick();
 assert.match(text(),/CANCELLED/);assert.doesNotMatch(text(),/EXPIRED/,'pages replace rather than accumulating');
 assert.match(text(),/old-sha/,'attempt paging preserves links page');
 click(control('next','links'));reply(pending(),[{id:'r2',product_id:'p',job_id:malicious,feature_id:'f',revision_id:'r2',integration_id:'i',repository_id:'r'}]);await tick();
 assert.equal(w.document.querySelector('#worker-history script'),null);assert.match(text(),/Feature <script>bad<\/script>/);
 click(control('start','attempts'));assert.equal(new URL(pending().path,'http://x').searchParams.has('cursor'),false);reply(pending());await tick();assert.match(text(),/No attempts recorded/);
 click(control('next'));const stale=pending();assert.equal(run('workerHistory.job'),'');
 run(`productID='q';persistProductRoute(false,'');render()`);assert.equal(stale.options.signal.aborted,true);
 reply(stale,[job('stale-private')]);await tick();assert.doesNotMatch(text(),/stale-private/);assert.equal(w.document.querySelector('[data-worker-history="toggle"]').getAttribute('aria-expanded'),'false');
 click('[data-worker-history="toggle"]');reply(pending(),[],false,501);await tick();assert.match(text(),/unavailable on this server/);assert.doesNotMatch(text(),/No native worker jobs/);
 click(control('retry'));reply(pending(),[],false,403);await tick();assert.match(text(),/do not have access/);
 click(control('start'));reply(pending(),[],false,400);await tick();assert.match(text(),/scope or cursor changed/);
 click(control('start'));reply(pending(),[],false,200,{product_id:'p'});await tick();assert.match(text(),/mismatched scope/);
 click(control('start'));pending().resolve({ok:true,status:200,text:async()=>'<html>bad</html>'});await tick();assert.match(text(),/invalid response/);
 click(control('start'));pending().resolve({ok:true,status:200,text:async()=>'x'.repeat(32769)});await tick();assert.match(text(),/metadata limit/);
 click(control('start'));pending().reject(new Error('Network disconnected'));await tick();assert.match(text(),/Network disconnected/);
 click(control('start'));reply(pending(),Array.from({length:11},(_,i)=>({id:String(i),product_id:'q'})));await tick();assert.match(text(),/invalid or mismatched scope/);
 click(control('start'));reply(pending());await tick();assert.match(text(),/No native worker jobs/);
 // New feature scope, then a late response for an obsolete request in that scope.
 run(`productID='p';persistProductRoute(false,'#f');render()`);click('[data-worker-history="toggle"]');
 assert.equal(new URL(pending().path,'http://x').searchParams.get('feature_id'),'f');
 const earlier=pending();run(`loadWorkerHistory('jobs')`);const newer=pending();
 reply(newer,[job('new',{capability:'registry_publish',recorded_status:'SUCCEEDED',provenance:'unknown',links:0})]);await tick();
 reply(earlier,[job('obsolete')]);await tick();assert.match(text(),/Registry publication/);assert.match(text(),/Unknown attribution/);assert.doesNotMatch(text(),/obsolete/);
 click('[data-worker-history="select"]');reply(requests.at(-2));reply(pending());await tick();assert.match(text(),/base-only input or unknown \/ malformed/);
 // Credential changes invalidate an in-flight read even without a render.
 click(control('start'));const oldCredential=pending();w.sessionStorage.setItem('rc-token','replacement');reply(oldCredential,[job('wrong-credential')]);await tick();assert.doesNotMatch(text(),/wrong-credential/);
 run('render()');click('[data-worker-history="toggle"]');assert.equal(pending().options.headers.Authorization,'Bearer replacement');reply(pending(),[],false,401);await tick();assert.match(text(),/Access expired/);
 run(`authMode='oidc';authExpired=false;render()`);click('[data-worker-history="toggle"]');reply(pending(),[],false,401);await tick();
 assert.equal(run('authExpired'),true);assert.equal(run('workerHistory'),null);assert.ok(w.document.querySelector('#main a[href="/auth/login"]'));
 assert.ok(requests.every(r=>r.path.startsWith('/api/worker-history?')&&r.options.method==='GET'),'no full state, execution/detail or mutation fallback');
}finally{dom.window.close();}
console.log('Worker history UI: bounded independent pages, frozen shared links, opaque IDs, escaping, unknown/base-only/error states, scope/credential/stale races, auth expiry and GET-only reads passed.');
