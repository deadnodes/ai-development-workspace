import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {JSDOM} from 'jsdom';
const html=fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8');
function browser(url,stored){
 const dom=new JSDOM(html,{url,runScripts:'outside-only'}),w=dom.window;
 w.localStorage.setItem('rc-product',stored);w.fetch=()=>new Promise(()=>{});
 const run=s=>vm.runInContext(s,dom.getInternalVMContext());
 for(const f of ['app','external'])run(fs.readFileSync(new URL(`./static/${f}.js`,import.meta.url),'utf8'));
 run(`state={products:[{id:'a',name:'A'},{id:'b',name:'B'}],features:[{id:'feature-b',product_id:'b',title:'Feature B'}]};render()`);
 return {w,run,dom};
}
const one=browser('http://localhost/?product=a#delivery','b');
const two=browser(one.w.location.href,'b');
for(const b of [one,two]){assert.equal(b.run('productID'),'a');assert.match(b.w.document.querySelector('#main').textContent,/Environments/);}
one.w.history.replaceState(null,'','/?product=a');one.run('render()');
assert.equal(one.w.document.querySelector('[data-action=delete_product]'),null);
one.w.history.replaceState(null,'','/?product=a#configuration');one.run('render()');
assert.ok(one.w.document.querySelector('[data-action=delete_product]'));
assert.equal(one.run("forms.delete_product.fields[0].name"),'name');
one.w.history.replaceState(null,'','/?product=a#delivery');one.run('render()');
const selector=one.w.document.querySelector('#product');selector.value='b';selector.dispatchEvent(new one.w.Event('change'));
assert.equal(one.w.location.search,'?product=b');assert.equal(one.w.location.hash,'#delivery');
one.w.history.back();await new Promise(r=>setTimeout(r,30));assert.equal(one.run('productID'),'a');
one.w.history.forward();await new Promise(r=>setTimeout(r,30));assert.equal(one.run('productID'),'b');
const legacy=browser('http://localhost/#feature-b','a');assert.equal(legacy.w.location.search,'?product=b');
const operations=browser('http://localhost/#operations','a');assert.equal(operations.w.location.search,'?product=a');
const unknown=browser('http://localhost/?product=missing#delivery','a');assert.equal(unknown.run('productID'),'missing');
for(const b of [one,two,legacy,operations,unknown])b.dom.window.close();
for(const visible of [true,false]){
 const {w,run,dom}=browser('http://localhost/?product=stale#bookmark','a');
 const calls=[];
 const view={products:[{id:'b',name:'B'}],features:visible?[{id:'bookmark',product_id:'b',title:'Bookmarked feature'}]:[],projection:{product_id:'b'}};
 w.fetch=async path=>{
  if(path==='/api/statuses')return {ok:true,json:async()=>({})};
  calls.push(path);
  if(path==='/api/ui-state?'||visible&&path==='/api/ui-state?feature_id=bookmark')return {ok:true,json:async()=>view};
  return {ok:false,status:404,json:async()=>({error:'not found'})};
 };
 await run('refresh()');
 assert.deepEqual(calls,['/api/ui-state?product_id=stale&feature_id=bookmark','/api/ui-state?feature_id=bookmark',...(visible?[]:['/api/ui-state?'])]);
 assert.equal(w.location.search,'?product=b');
 assert.equal(w.location.hash,visible?'#bookmark':'');
 assert.equal(w.document.querySelector('#breadcrumb').textContent,visible?'Bookmarked feature':'Overview');
 dom.window.close();
}
console.log('Product routes, history and stale-hint bookmark recovery passed.');
