'use strict';
// One conditional read loop for every route; mutations/navigation invalidate old reads.
let liveEpoch=0,liveETag='',liveInFlight=false,liveMutations=0,liveFailed=false;
function invalidateLive(){liveEpoch++;liveETag='';}
function beginLiveMutation(){liveMutations++;invalidateLive();return ()=>{liveMutations--;invalidateLive();};}
function liveKey(node){
 if(node.nodeType!==1)return '';
 if(node.id)return `id:${node.id}`;
 if(node.dataset.liveKey)return `key:${node.dataset.liveKey}`;
 if(node.tagName==='A')return `link:${node.getAttribute('href')}`;
 if(node.tagName==='OPTION')return `option:${node.value}`;
 if(node.tagName==='DETAILS')return `details:${node.querySelector('summary')?.textContent.replace(/\(\d+\)/g,'(#)')||''}`;
 if(node.tagName==='SECTION')return `section:${node.querySelector('h2')?.textContent||''}`;
 if(node.tagName==='BUTTON'){const attrs=Array.from(node.attributes).filter(a=>a.name.startsWith('data-')).map(a=>`${a.name}=${a.value}`).sort();if(attrs.length)return `button:${attrs.join('|')}`;}
 return '';
}
function liveCompatible(a,b){return a.nodeType===b.nodeType&&(a.nodeType!==1||a.tagName===b.tagName)&&liveKey(a)===liveKey(b);}
function patchLiveNode(current,next){
 if(current.isEqualNode(next))return;
 if(current.nodeType===3||current.nodeType===8){if(current.nodeValue!==next.nodeValue)current.nodeValue=next.nodeValue;return;}
 if(current.nodeType!==1)return;
 const keepOpen=current.tagName==='DETAILS';
 for(const attr of Array.from(current.attributes))if(!(keepOpen&&attr.name==='open')&&!next.hasAttribute(attr.name))current.removeAttribute(attr.name);
 for(const attr of Array.from(next.attributes))if(!(keepOpen&&attr.name==='open')&&current.getAttribute(attr.name)!==attr.value)current.setAttribute(attr.name,attr.value);
 // Inputs are user state. Polling pauses during editing; never rewrite their values.
 if(['INPUT','TEXTAREA'].includes(current.tagName))return;
 const selected=current.tagName==='SELECT'?current.value:null;
 patchLiveChildren(current,next);
 if(selected!==null&&Array.from(current.options).some(o=>o.value===selected))current.value=selected;
}
function patchLiveChildren(current,next){
 const old=Array.from(current.childNodes),used=new Set(),keyed=new Map(old.filter(n=>liveKey(n)).map(n=>[liveKey(n),n]));
 let cursor=current.firstChild;
 for(const desired of Array.from(next.childNodes)){
  const key=liveKey(desired);
  let match=key?keyed.get(key):cursor;
  if(!match||used.has(match)||!liveCompatible(match,desired))match=old.find(n=>!used.has(n)&&!liveKey(n)&&liveCompatible(n,desired));
  if(match){used.add(match);if(match!==cursor)current.insertBefore(match,cursor);patchLiveNode(match,desired);cursor=match.nextSibling;}
  else {const added=desired.cloneNode(true);current.insertBefore(added,cursor);used.add(added);cursor=added.nextSibling;}
 }
 for(const node of old)if(!used.has(node)&&node.parentNode===current)current.removeChild(node);
}
function patchLiveHTML(node,html){const template=document.createElement('template');template.innerHTML=html;patchLiveChildren(node,template.content);}
function canPollLive(){
 const active=document.activeElement,selection=window.getSelection?.();
 return !document.hidden&&!$('#dialog').open&&!currentForm&&!refreshInFlight&&!liveMutations&&
  !(active&&(active.isContentEditable||['INPUT','TEXTAREA','SELECT'].includes(active.tagName)))&&
  !(selection&&!selection.isCollapsed);
}
async function pollLive(){
 if(liveInFlight||!canPollLive())return;
 liveInFlight=true;const controller=new AbortController(),timeout=window.setTimeout(()=>controller.abort(),10000);const epoch=liveEpoch,route=location.hash,product=productID,token=sessionStorage.getItem('rc-token');
 try{
  const response=await fetch('/api/state',{headers:{...(liveETag?{'If-None-Match':liveETag}:{}),...(token?{'Authorization':`Bearer ${token}`}:{})},cache:'no-store',signal:controller.signal});
  if(epoch!==liveEpoch||route!==location.hash||product!==productID||token!==sessionStorage.getItem('rc-token')||!canPollLive())return;
  if(response.status===304){if(liveFailed)notice('Connection restored. Shared state is current.');liveFailed=false;return;}
  if(!response.ok)throw new Error(`State refresh failed (${response.status})`);
  const next=await response.json();
  if(epoch!==liveEpoch||route!==location.hash||product!==productID||token!==sessionStorage.getItem('rc-token')||!canPollLive())return;
  const x=window.scrollX,y=window.scrollY,focused=document.activeElement;
  state=next;if(!list(state.products).some(p=>p.id===productID))productID=state.products?.[0]?.id||'';
  const feature=selectedFeature();if(feature)productID=feature.product_id;
  render(true);if(focused?.isConnected&&document.activeElement!==focused)focused.focus({preventScroll:true});liveETag=response.headers.get('ETag')||'';
  if(window.scrollX!==x||window.scrollY!==y)window.scrollTo(x,y);
  if(liveFailed)notice('Connection restored. Shared state is current.');liveFailed=false;
 }catch(error){if(epoch===liveEpoch&&!liveFailed){notice('Live update unavailable. Your current view is retained; retry with Refresh.',true);liveFailed=true;}}
 finally{window.clearTimeout(timeout);liveInFlight=false;}
}
document.addEventListener('visibilitychange',()=>{if(!document.hidden)pollLive();});
window.addEventListener('hashchange',()=>invalidateLive());
if(typeof window.setInterval==='function')window.setInterval(pollLive,3000);
