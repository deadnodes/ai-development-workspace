import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {TextEncoder,TextDecoder} from 'node:util';
import {JSDOM} from 'jsdom';

const dom=new JSDOM(fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8'),{url:'http://localhost/?product=p#operations',runScripts:'outside-only',pretendToBeVisual:true});
const w=dom.window,run=script=>vm.runInContext(script,dom.getInternalVMContext()),tick=()=>new Promise(resolve=>setTimeout(resolve,0));
w.fetch=()=>new Promise(()=>{});w.TextEncoder=TextEncoder;w.TextDecoder=TextDecoder;
for(const file of ['app','external','flow'])run(fs.readFileSync(new URL(`./static/${file}.js`,import.meta.url),'utf8'));
const requests=[];w.fetch=(path,options)=>new Promise((resolve,reject)=>requests.push({path,options,resolve,reject}));
const pending=()=>requests.at(-1);
const op=(id,extra={})=>({id,product_id:'p',kind:'INTELLIGENCE',status:'SUCCEEDED',updated_at:'2026-11-04T12:00:00Z',intelligence:{kind:'release_notes',input_fingerprint:'input-1',result_fingerprint:'result-1',model:'configured-model',prompt_version:'v1',input_ciphertext:'do-not-render-input',result_ciphertext:'do-not-render-result'},...extra});
const id='notes<&"/opaque';
const draft=(task=op(id))=>({task,review_required:true,published:false,usage_cost:'unknown',stale:false,result:{decision:'draft',summary:'Клиентский релиз <img src=x onerror=alert(1)>',changes:[{integration_ids:['internal-integration'],text:'Исправлена доставка <script>bad()</script>',evidence_refs:['internal-evidence']}],warnings:['Проверить формулировки <svg onload=bad()>']},provenance:{environment_id:'env',previous_release_id:'previous-release',candidate_operation_id:'candidate',candidate_fingerprint:'candidate-fingerprint',previous_sources:{repo:'old-sha'},sources:{repo:'new-sha'},previous_artifacts:{app:'old-digest'},artifacts:{app:'new-digest'}}});
function reply(request,data,status=200){request.resolve({ok:status===200,status,text:async()=>typeof data==='string'?data:JSON.stringify(data)});}
function button(){return w.document.querySelector('[data-intelligence-preview]');}
function preview(){return w.document.querySelector('[data-intelligence-result]');}
function open(){assert.ok(button());button().click();return pending();}
function paint(operations=[op(id)]){run(`state={products:[{id:'p',name:'Product'},{id:'q',name:'Other'}],operations:${JSON.stringify(operations)}};attentionResults.set('p',[]);attentionResults.set('q',[]);productID='p';persistProductRoute(false,'#operations');render()`);}
try{
 paint([op(id),op('other-product',{product_id:'q'}),op('annotation',{intelligence:{kind:'annotation'}}),op('delivery',{kind:'DEPLOY'})]);
 assert.equal(w.document.querySelectorAll('[data-intelligence-preview]').length,1,'preview belongs only to release notes in this product');
 assert.equal(requests.length,0,'render does not read or generate drafts');
 assert.doesNotMatch(w.document.querySelector('#main').textContent,/do-not-render/,'encrypted envelopes are not displayed');
 let request=open();assert.match(preview().textContent,/Loading customer release notes/);
 assert.equal(request.path,`/api/intelligence-tasks/${encodeURIComponent(id)}`);assert.equal(request.options.method,'GET');assert.equal(request.options.cache,'no-store');assert.equal(request.options.redirect,'error');
 reply(request,draft());await tick();
 assert.match(preview().textContent,/Review required/);assert.match(preview().textContent,/not been published/);assert.match(preview().textContent,/Клиентский релиз/);
 assert.equal(preview().querySelector('img,script,svg'),null,'provider content is text, never executable markup');
 const customer=preview().querySelector('[aria-label="Customer-facing draft"]');assert.ok(customer);assert.doesNotMatch(customer.textContent,/internal-integration|internal-evidence|new-sha|configured-model|Проверить/,'internal evidence and warnings remain outside customer copy');
 assert.match(preview().querySelector('details').textContent,/previous-release/);assert.match(preview().querySelector('details').textContent,/internal-evidence/);
 preview().querySelector('button').click();assert.equal(preview().hidden,true);assert.equal(preview().textContent,'');assert.equal(button().getAttribute('aria-expanded'),'false');
 request=open();reply(request,{...draft(),stale:true});await tick();assert.match(preview().textContent,/draft is stale/);assert.doesNotMatch(preview().textContent,/Клиентский релиз/,'even an incorrectly included stale result is hidden');
 for(const [status,expected] of [['PENDING',/queued/],['RUNNING',/being prepared/],['CANCELLED',/cancelled/],['FAILED',/failed/],['BLOCKED',/need attention/]]){
  request=open();const data=draft(op(id,{status}));delete data.result;delete data.provenance;reply(request,data);await tick();assert.match(preview().textContent,expected);assert.doesNotMatch(preview().textContent,/Клиентский релиз/);
 }
 for(const [status,expected] of [[401,/access has expired/],[403,/do not have access/],[404,/unavailable/],[429,/Too many/],[500,/Could not load/]]){
  request=open();reply(request,{},status);await tick();assert.match(preview().textContent,expected);assert.ok(preview().querySelector('[role="alert"]'));
 }
 for(const data of [{...draft(),task:op('wrong-task')},{...draft(),task:op(id,{product_id:'q'})},{...draft(),published:true},{...draft(),review_required:false},{...draft(),result:{...draft().result,changes:[{text:'unscoped'}]}},{...draft(),provenance:null}]){
  request=open();reply(request,data);await tick();assert.match(preview().textContent,/invalid|missing/);assert.doesNotMatch(preview().textContent,/Клиентский релиз/);
 }
 request=open();reply(request,'<html>bad response</html>');await tick();assert.match(preview().textContent,/invalid response/);
 request=open();reply(request,'x'.repeat(512*1024+1));await tick();assert.match(preview().textContent,/preview limit/);
 request=open();request.reject(new Error('Network disconnected'));await tick();assert.match(preview().textContent,/Network disconnected/);
 // An older read cannot overwrite a newer preview for the same task.
 const old=open(),newer=open();assert.equal(old.options.signal.aborted,true);reply(newer,{...draft(),result:{...draft().result,summary:'Current draft'}});await tick();reply(old,draft());await tick();assert.match(preview().textContent,/Current draft/);assert.doesNotMatch(preview().textContent,/Клиентский релиз/);
 // Product navigation clears visible text and aborts pending reads.
 request=open();const selector=w.document.querySelector('#product');selector.value='q';selector.dispatchEvent(new w.Event('change',{bubbles:true}));assert.equal(request.options.signal.aborted,true);reply(request,draft());await tick();assert.equal(button()?.dataset.intelligencePreview,'other-product');assert.doesNotMatch(w.document.querySelector('#main').textContent,/Current draft|Клиентский релиз/);
 paint();request=open();run(`state.operations[0].updated_at='2026-11-04T13:00:00Z'`);reply(request,draft());await tick();assert.match(preview().textContent,/task changed while loading/);
 paint();request=open();w.sessionStorage.setItem('rc-token','replacement');reply(request,draft());await tick();assert.equal(preview().hidden,true);assert.equal(preview().textContent,'');
 request=open();assert.equal(request.options.headers.Authorization,'Bearer replacement');reply(request,draft());await tick();assert.match(preview().textContent,/Клиентский релиз/);
 // An obsolete 401 must not sign out a different, newer credential/session.
 request=open();run(`authMode='oidc';authExpired=false`);reply(request,{},401);await tick();assert.equal(run('authExpired'),false);assert.equal(preview().hidden,true);
 request=open();reply(request,{},401);await tick();assert.equal(run('authExpired'),true);assert.ok(w.document.querySelector('#main a[href="/auth/login"]'));
 assert.ok(requests.every(r=>r.path.startsWith('/api/intelligence-tasks/')&&r.options.method==='GET'&&!r.options.body),'preview never generates tasks, mutates state or falls back to broad context');
}finally{run('clearIntelligencePreviews()');dom.window.close();}
console.log('Release notes UI: explicit bounded GET, scoped draft/provenance separation, safe text, stale/pending/error states, latest-read/product/credential races and no generation/publication passed.');
