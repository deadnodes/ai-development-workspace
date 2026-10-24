import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {JSDOM} from 'jsdom';

const dom=new JSDOM(fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8'),{url:'https://control.example.test/',runScripts:'outside-only',pretendToBeVisual:true});
const {window}=dom,{document}=window,ctx=dom.getInternalVMContext();
const run=source=>vm.runInContext(source,ctx);
window.fetch=()=>new Promise(()=>{});window.setInterval=()=>0;
window.HTMLDialogElement.prototype.showModal=function(){this.open=true;};
window.HTMLDialogElement.prototype.close=function(){this.open=false;};
for(const file of ['app','external','flow','library','workspace','live'])run(fs.readFileSync(new URL(`./static/${file}.js`,import.meta.url),'utf8'));
window.sessionStorage.setItem('rc-token','old-manual-token');
window.fetch=async()=>({status:404});await run('loadIdentity()');
assert.equal(run('authMode'),'local');assert.equal(run('browserToken()'),'old-manual-token');
const principal={id:'human/<name>',kind:'human',role:'operator',products:['p'],permissions:['read','write']};
window.fetch=async()=>({ok:true,status:200,json:async()=>({auth_mode:'oidc',principal,csrf_token:'csrf-secret'})});
await run('loadIdentity()');
assert.equal(run('authMode'),'oidc');assert.equal(run('browserToken()'),null);assert.equal(window.sessionStorage.getItem('rc-token'),null);
assert.equal(document.querySelector('#identity').textContent,'human/<name>');
let sent;
window.fetch=async(path,options)=>{sent={path,options};return {ok:true,status:200,json:async()=>({id:'ok'})};};
await run("request('/api/commands',{action:'record_decision'})");
assert.equal(sent.options.credentials,'same-origin');assert.equal(sent.options.headers['X-CSRF-Token'],'csrf-secret');assert.equal(sent.options.headers.Authorization,undefined);
// Account screen shows server-derived identity and revocable independent agents,
// with escaped text and no manual credential field.
window.fetch=async()=>({ok:true,status:200,json:async()=>({agents:[{id:'a',name:'<img src=x onerror=alert(1)>',products:['p'],permissions:['read']}]})});
await run('showAccount()');
assert.equal(document.querySelector('#input-token'),null);assert.equal(document.querySelector('#account-agents img'),null);assert.ok(document.querySelector('[data-revoke-agent="a"]'));
run('closeDialog()');
// API expiry removes old protected state and presents an actual sign-in link.
run("state={products:[{id:'p',name:'Private'}]};productID='p';currentForm=null;refreshInFlight=0;");
window.fetch=async()=>({ok:false,status:401,json:async()=>({error:'unauthorized'})});
await assert.rejects(run("request('/api/state')"),/Sign in/);
assert.equal(run('authExpired'),true);assert.equal(run('Object.keys(state).length'),0);assert.ok(document.querySelector('#main a[href="/auth/login"]'));
assert.equal(run('canPollLive()'),false);
// Polling expiry takes the same path rather than retaining sensitive stale UI.
run("authExpired=false;authPrincipal={id:'human/test'};state={products:[{id:'p'}]};refreshInFlight=0;currentForm=null;liveMutations=0;");
document.activeElement?.blur();await run('pollLive()');
assert.equal(run('authExpired'),true);assert.equal(run('Object.keys(state).length'),0);assert.ok(document.querySelector('#notice a[href="/auth/login"]'));
// /auth/me itself can require login, without redirect loops or token fallback.
run("authExpired=false;");window.fetch=async()=>({ok:false,status:401,json:async()=>({auth_mode:'oidc',error:'unauthorized'})});
await run('loadIdentity()');assert.equal(run('authExpired'),true);assert.equal(run('browserToken()'),null);
dom.window.close();
console.log('Browser auth checks passed: local fallback, cookie identity/CSRF, escaped agent account, API/poll expiry and sign-in recovery.');
