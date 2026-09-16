import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const elements=new Map(),listeners=new Map();
const element=selector=>({value:'',classList:{toggle(){},remove(){}},addEventListener(type,handler){listeners.set(`${selector}:${type}`,handler);},showModal(){this.open=true;},close(){this.open=false;}});
const sandbox={console,URLSearchParams,URL,Date,JSON,String,Array,Set,Number,Object,Error,Map,encodeURIComponent,decodeURIComponent,
 document:{querySelector(selector){if(!elements.has(selector))elements.set(selector,element(selector));return elements.get(selector);},addEventListener(){}},
 location:{hash:'',href:'http://localhost/',search:''},localStorage:{getItem(){return null;},setItem(){}},sessionStorage:{getItem(){return null;},setItem(){}},window:{addEventListener(){}},fetch:()=>new Promise(()=>{})};
sandbox.history={replaceState(_state,_title,url){const u=new URL(url);Object.assign(sandbox.location,{href:u.href,search:u.search,hash:u.hash});},pushState(_state,_title,url){this.replaceState(_state,_title,url);}};
vm.createContext(sandbox);
for(const file of ['app','external','flow','library','workspace'])vm.runInContext(fs.readFileSync(new URL(`./static/${file}.js`,import.meta.url),'utf8'),sandbox);
const run=source=>vm.runInContext(source,sandbox);
run(`state={products:[{id:'p',name:'Product'}]};productID='p'`);
assert.ok(run('overview()').includes('Local workspace &amp; project context')||run('overview()').includes('Local workspace & project context'));
assert.ok(!run(`workspaceRemote('javascript:alert(1)')`).includes('<a'));
assert.ok(run(`workspaceRemote('https://example.test/a?x=<script>')`).includes('rel="noopener noreferrer"'));
const context={product:{name:'<img>'},filesystem_enabled:false,repositories:[{name:'Repo',role:'APPLICATION',url:'javascript:alert(1)'}],knowledge:{overview:'<script>',instructions:'Keep state',areas:[{id:'core',name:'Core',repository_ids:['r']}],relationships:[{from:'core',to:'api',type:'calls',contract:'<img>'}]},local_checkouts:[{relative_path:'repo',branch:'feature/x',commit:'abc',agents:{path:'AGENTS.md',content:'<script>alert(1)</script>',sha256:'digest',truncated:true},errors:['Skipped link']}],workspace_agents:[]};
sandbox.context=context;
const html=run('workspaceContextHTML(context)');assert.ok(html.includes('Filesystem scanning unavailable'));assert.ok(html.includes('&lt;script&gt;'));assert.ok(!html.includes('<script>'));assert.ok(html.includes('truncated'));assert.ok(html.includes('Skipped link'));
assert.throws(()=>run(`parseWorkspaceArray('{}','Areas')`),/JSON array/);
let requests=[];sandbox.fakeRequest=async(path,body)=>{requests.push({path,body});return context;};run('request=fakeRequest');
await run(`workspaceAction('scan')`);assert.equal(requests.length,1);assert.equal(requests[0].path,'/api/products/p/context');assert.equal(elements.get('#dialog-title').textContent,'Filesystem scan unavailable');assert.equal(elements.get('#save').hidden,true);
context.filesystem_enabled=true;await run(`workspaceAction('scan')`);assert.equal(elements.get('#save').hidden,false);assert.ok(!elements.get('#fields').innerHTML.includes('name="path"'));
await run(`workspaceAction('knowledge')`);assert.ok(elements.get('#fields').innerHTML.includes('What this product does'));assert.ok(elements.get('#fields').innerHTML.includes('name="areas"'));
await run(`workspaceAction('import')`);assert.ok(elements.get('#form-help').textContent.includes('not a history/backup restore'));assert.ok(elements.get('#fields').innerHTML.includes('name="configuration"'));
assert.ok(listeners.has('#command-form:submit'));
assert.ok(fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8').includes('/workspace.js'));
console.log('Local workspace context escapes scanned text, keeps scan paths server-owned and exposes explicit unavailable/import semantics.');

context.components=[{name:'<component>',kind:'LIBRARY',parameters:{language:'<unsafe>'}}];context.environments=[{name:'dev',parameters:{region:'<region>'}}];const contextual=run('workspaceContextHTML(context)');assert.ok(contextual.includes('&lt;component&gt;'));assert.ok(contextual.includes('&lt;unsafe&gt;'));assert.ok(contextual.includes('&lt;region&gt;'));
