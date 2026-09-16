import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {JSDOM} from 'jsdom';
const dom=new JSDOM(fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8'),{url:'http://localhost/#f',runScripts:'outside-only',pretendToBeVisual:true});
const {window}=dom,{document}=window;
window.fetch=()=>new Promise(()=>{});window.setInterval=()=>0;
window.scrollTo=(x,y)=>{window.scrollX=x;window.scrollY=y;};
const ctx=dom.getInternalVMContext(),run=s=>vm.runInContext(s,ctx);
for(const file of ['app','external','flow','library','workspace','live'])run(fs.readFileSync(new URL(`./static/${file}.js`,import.meta.url),'utf8'));
run(`refreshInFlight=0;productID='p';currentForm=null;state={products:[{id:'p',name:'Product'}],features:[{id:'f',product_id:'p',title:'Feature',goal:'Old goal',status:'active'}],integrations:[{id:'i',product_id:'p',feature_id:'f',title:'Integration',status:'working',position:0}],memories:[],events:[],findings:[],gates:[],checks:[],results:[]};render()`);
await new Promise(resolve=>setTimeout(resolve,0));
const main=document.querySelector('#main'),integration=main.querySelector('[data-live-key="integration-i"]');
const details=integration.querySelector('details'),button=integration.querySelector('button');details.open=true;button.focus();window.scrollY=240;
let requests=[];window.sessionStorage.setItem('rc-token','test-token');
const next=JSON.parse(run('JSON.stringify(state)'));next.features[0].goal='New goal from external agent';next.integrations[0].title='Updated integration';
window.fetch=async(path,options)=>{requests.push({path,options});return {ok:true,status:200,headers:{get:()=> '"v1"'},json:async()=>next};};
await run('pollLive()');
assert.equal(requests.length,1);assert.equal(requests[0].options.headers.Authorization,'Bearer test-token');
assert.ok(main.textContent.includes('New goal from external agent'));assert.equal(main.querySelector('[data-live-key="integration-i"]'),integration);assert.equal(integration.querySelector('details'),details);assert.equal(details.open,true);assert.equal(document.activeElement,button);assert.equal(window.scrollY,240);
const observer=new window.MutationObserver(()=>{});observer.observe(main,{subtree:true,childList:true,attributes:true,characterData:true});
window.fetch=async(path,options)=>{requests.push({path,options});return {status:304,ok:false,json(){throw new Error('304 must not be parsed');}};};
await run('pollLive()');assert.equal(requests.at(-1).options.headers['If-None-Match'],'"v1"');assert.equal(observer.takeRecords().length,0);
// No-change 200 also preserves unchanged nodes and does not mutate the main tree.
window.fetch=async()=>({ok:true,status:200,headers:{get:()=> '"v1"'},json:async()=>next});await run('pollLive()');assert.equal(observer.takeRecords().length,0);
// Entity insertion and reordering reuse the keyed original row and open details.
const container=document.createElement('div');document.body.append(container);container.innerHTML='<article data-live-key="a"><details open><summary>History (1)</summary><p>old</p></details></article><article data-live-key="b">B</article>';
const rowA=container.firstChild,rowB=container.lastChild,history=rowA.firstChild;window.testContainer=container;
run(`patchLiveHTML(testContainer,'<article data-live-key="b">B updated</article><article data-live-key="a"><details><summary>History (2)</summary><p>new</p></details></article><article data-live-key="c">C</article>')`);
assert.equal(container.firstChild,rowB);assert.equal(container.children[1],rowA);assert.equal(rowA.firstChild,history);assert.equal(history.open,true);assert.equal(rowB.textContent,'B updated');
// Drafts, editable focus and text selection pause reads; button focus does not.
let count=0;window.fetch=async()=>{count++;throw new Error('unexpected read');};document.querySelector('#dialog').setAttribute('open','');document.querySelector('#fields').innerHTML='<textarea>unsaved draft</textarea>';await run('pollLive()');assert.equal(count,0);assert.equal(document.querySelector('#fields textarea').value,'unsaved draft');document.querySelector('#dialog').removeAttribute('open');
const input=document.createElement('input');document.body.append(input);input.focus();await run('pollLive()');assert.equal(count,0);input.blur();
Object.defineProperty(document,'hidden',{configurable:true,value:true});await run('pollLive()');assert.equal(count,0);Object.defineProperty(document,'hidden',{configurable:true,value:false});
const range=document.createRange();range.selectNodeContents(integration.querySelector('h3'));window.getSelection().addRange(range);await run('pollLive()');assert.equal(count,0);window.getSelection().removeAllRanges();
// A delayed read cannot overwrite state after a mutation or navigation generation.
let release;window.fetch=()=>{count++;return new Promise(resolve=>{release=resolve;});};
const pending=run('pollLive()');await run('pollLive()');assert.equal(count,1,'poll requests must not overlap');run('invalidateLive()');release({status:200,ok:true,headers:{get:()=> '"stale"'},json:async()=>({products:[]})});await pending;assert.equal(run('state.products[0].id'),'p');assert.equal(run('liveInFlight'),false);
// An in-flight response is also discarded if a draft opens while the read runs.
const pendingDraft=run('pollLive()');document.querySelector('#dialog').setAttribute('open','');release({status:200,ok:true,headers:{get:()=> '"draft"'},json:async()=>({products:[]})});await pendingDraft;assert.equal(run('state.products[0].id'),'p');document.querySelector('#dialog').removeAttribute('open');
// Errors retain the view. Timeout abort releases the polling slot.
window.fetch=async()=>{throw new Error('offline');};const before=main.innerHTML;await run('pollLive()');assert.equal(main.innerHTML,before);assert.ok(document.querySelector('#notice').textContent.includes('retained'));
const nativeTimeout=window.setTimeout;window.setTimeout=(fn,ms)=>nativeTimeout(fn,ms===10000?1:ms);window.fetch=(_path,{signal})=>new Promise((_resolve,reject)=>signal.addEventListener('abort',()=>reject(new Error('timeout'))));await run('pollLive()');assert.equal(run('liveInFlight'),false);
observer.disconnect();dom.window.close();
console.log('Live DOM tests passed: conditional/no-overlap polling, stable keyed nodes, focus/scroll/details/drafts, stale reads, errors and timeout recovery.');
