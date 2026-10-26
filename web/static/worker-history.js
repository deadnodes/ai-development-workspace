'use strict';
// One bounded page per collection, for one visible scope. No background reads.
const workerHistoryPageSize=10;
let workerHistory = null;
function workerHistoryScope(){
 if(authExpired||!productID)return null;
 const feature=selectedFeature(),route=featureID();
 if(route&&!feature)return null;
 return {product:productID,feature:feature?.id||'',token:browserToken()};
}
function sameWorkerScope(a,b){return !!a&&!!b&&a.product===b.product&&a.feature===b.feature&&a.token===b.token;}
function resetWorkerHistory(){
 if(workerHistory)for(const page of Object.values(workerHistory.pages))page.controller?.abort();
 workerHistory=null;
}
function workerHistoryEntry(){
 const scope=workerHistoryScope();
 if(!sameWorkerScope(workerHistory?.scope,scope)){
  resetWorkerHistory();
  if(scope)workerHistory={scope,open:false,job:'',pages:{jobs:{}}};
 }
 return workerHistory;
}
function workerHistoryControl(action,label,collection='',disabled=false){return `<button type="button" data-worker-history="${action}" data-collection="${esc(collection)}"${disabled?' disabled':''}>${esc(label)}</button>`;}
function workerHistoryPanel(){
 const entry=workerHistoryEntry();if(!entry)return '';
 return `<section id="worker-history" class="panel spaced worker-history" aria-labelledby="worker-history-title"><div class="section-heading"><div><h2 id="worker-history-title">Native worker history</h2><p class="meta">${entry.scope.feature?'Jobs linked to frozen Feature revisions':'All Product jobs, including base-only and unknown attribution'}</p></div><button type="button" data-worker-history="toggle" aria-expanded="${entry.open}" aria-controls="worker-history-content">${entry.open?'Hide history':'Show history'}</button></div><div id="worker-history-content"${entry.open?'':' hidden'}>${entry.open?workerHistoryContent(entry):''}</div></section>`;
}
function workerHistoryContent(entry){
 return `<p class="muted">Recorded status is historical metadata, not verified build success, registry publication or deployment. GitHub Actions history is separate.</p><p class="meta">Live pages follow job or record IDs, not time order. Start from the beginning to find concurrent inserts before your cursor.</p>${entry.job?`<div class="worker-history-job"><h3 id="worker-history-selected" tabindex="-1">Selected job</h3><code>${esc(entry.job)}</code><p class="meta">Attempts and frozen links have separate pages. Links include every Feature captured in this shared job, even when viewing one Feature.</p>${workerHistoryCollection(entry,'attempts')}${workerHistoryCollection(entry,'links')}</div>`:''}${workerHistoryCollection(entry,'jobs')}`;
}
function workerHistoryCollection(entry,collection){
 const page=entry.pages[collection]||{},title={jobs:'Jobs',attempts:'All attempts',links:'Frozen Feature / revision links'}[collection];
 const controls=`<div class="actions">${workerHistoryControl('start',page.cursor?'From start':'Refresh',collection,!!page.loading)}${page.error?workerHistoryControl('retry','Retry',collection,!!page.loading):''}${workerHistoryControl('next','Next page',collection,!!page.loading||!page.data?.pagination.has_more)}</div>`;
 return `<section class="worker-history-collection" aria-label="${title}" aria-busy="${!!page.loading}"><div class="section-heading"><h3>${title}</h3>${controls}</div><p class="meta" role="status">${page.loading?'Loading '+title.toLowerCase()+'…':page.data?`${page.data.items.length} records on this page${page.data.pagination.has_more?' · More available':' · End of current traversal'}`:''}</p>${page.error?`<p role="alert" class="error">${esc(page.error)}</p>`:page.data?(page.data.items.map(item=>workerHistoryRecord(item,collection,entry)).join('')||`<p class="muted">${collection==='jobs'?'No native worker jobs recorded in this scope.':collection==='attempts'?'No attempts recorded for this job.':'No asserted Feature links. This can mean base-only input or unknown / malformed attribution; no membership is inferred.'}</p>`):''}</section>`;
}
function workerHistoryTime(value){
 if(!value||String(value).startsWith('0001-01-01'))return 'Unknown';
 return Number.isFinite(Date.parse(value))?date(value):'Unknown (invalid recorded timestamp)';
}
function workerHistoryRecord(item,collection,entry){
 const provenance={recorded_revision:'Recorded immutable revision',recorded_snapshot_unverified:'Recorded frozen snapshot · unverified',unknown:'Unknown attribution'}[item.provenance]||'Unknown attribution';
 const capability={local_oci_build:'OCI build',source_check:'Source check',registry_publish:'Registry publication',registry_observe:'Registry observation'}[item.capability]||item.capability||'Unknown capability';
 const feature=list(state.features).find(f=>f.id===item.feature_id&&f.product_id===entry.scope.product);
 const featureURL=`?${new URLSearchParams({product:entry.scope.product})}#${encodeURIComponent(item.feature_id||'')}`;
 return `<article class="row" data-live-key="worker-${esc(collection)}-${esc(item.id)}"><div class="row-heading"><strong>${collection==='jobs'?esc(capability):collection==='attempts'?'Attempt':'Frozen revision'}</strong>${collection!=='links'?`<span class="tag">Recorded: ${esc(item.recorded_status||'unknown')}</span>`:''}</div><code>${esc(item.id)}</code>${collection==='jobs'?`<p class="meta">${esc(provenance)} · ${esc(item.attempts||0)} attempts · ${esc(item.links||0)} frozen links</p><button type="button" data-worker-history="select" data-job="${esc(item.id)}" aria-pressed="${entry.job===item.id}">View attempts & links</button>`:''}${collection==='links'?`<p><a href="${esc(featureURL)}">${esc(feature?.title||item.feature_id)}</a></p>`:''}<details><summary>Source & exact metadata references</summary><dl class="worker-history-metadata">${collection==='links'?detail('Feature ID',item.feature_id):''}${detail('Repository',item.repository_id)}${detail('Source SHA',item.source_sha)}${detail('Revision',item.revision_id)}${detail('Integration',item.integration_id)}${detail('Operation',item.operation_id)}${collection==='links'?detail('Provenance',provenance):''}${collection!=='links'?detail('Created',workerHistoryTime(item.created_at))+detail('Updated',workerHistoryTime(item.updated_at)):''}</dl><dl>${detail('Input hash',item.input_hash)}${detail('Recipe hash',item.recipe_hash)}${detail('Delivery',item.delivery_id)}${detail('Parent',item.parent_id)}${detail('Snapshot hash',item.snapshot_hash)}${detail('Parent hash',item.parent_hash)}${detail('Capability',item.capability)}${detail('Provenance value',item.provenance||'unknown')}</dl></details></article>`;
}
function paintWorkerHistory(){
 const host=document.getElementById('worker-history');if(!host)return;
 const template=document.createElement('template');template.innerHTML=workerHistoryPanel();
 const next=template.content.firstElementChild;
 if(typeof patchLiveNode==='function')patchLiveNode(host,next);else host.replaceWith(next);
}
function syncWorkerHistory(){workerHistoryEntry();}
function validWorkerHistoryPage(data,scope,job,collection){
 return data?.projection==='native_worker_history'&&data.product_id===scope.product&&(data.feature_id||'')===scope.feature&&(data.job_id||'')===job&&data.collection===collection&&Array.isArray(data.items)&&data.items.length<=workerHistoryPageSize&&data.items.every(item=>item&&typeof item.id==='string'&&item.id.length>0&&item.id.length<=128&&item.product_id===scope.product&&(!job||item.job_id===job)&&(collection!=='links'||['feature_id','revision_id','integration_id','repository_id'].every(key=>typeof item[key]==='string'&&item[key].length>0))&&Object.values(item).every(v=>typeof v==='string'?v.length<=128:typeof v==='number'&&Number.isSafeInteger(v)&&v>=0))&&typeof data.pagination?.has_more==='boolean'&&(!data.pagination.has_more||(typeof data.pagination.next_cursor==='string'&&data.pagination.next_cursor.length>0&&data.pagination.next_cursor.length<=2048));
}
async function loadWorkerHistory(collection,cursor=''){
 const entry=workerHistoryEntry();if(!entry?.open)return;
 const job=collection==='jobs'?'':entry.job;if(collection!=='jobs'&&!job)return;
 const old=entry.pages[collection];old?.controller?.abort();
 const page={loading:true,cursor,controller:new AbortController()};entry.pages[collection]=page;
 const scope=entry.scope,query=new URLSearchParams({product_id:scope.product,collection,limit:String(workerHistoryPageSize)});
 if(scope.feature)query.set('feature_id',scope.feature);if(job)query.set('job_id',job);if(cursor)query.set('cursor',cursor);
 const current=()=>workerHistory===entry&&entry.pages[collection]===page&&sameWorkerScope(scope,workerHistoryScope());
 paintWorkerHistory();
 try{
  const response=await fetch(`/api/worker-history?${query}`,{method:'GET',credentials:'same-origin',cache:'no-store',headers:scope.token?{Authorization:`Bearer ${scope.token}`}:{},signal:page.controller.signal});
  if(!current())return;
  if(response.status===401){if(authMode==='oidc'){requireSignIn();return;}throw new Error('Access expired or unavailable. Check Settings, then retry.');}
  if(response.status===403)throw new Error('You do not have access to this history. Check the selected Product and your account, then retry.');
  if(response.status===404||response.status===501)throw new Error('Native worker history is unavailable on this server. Retry after the bounded reader is available.');
  if(!response.ok)throw new Error(`History could not be loaded (${response.status}). Retry, or start from the beginning if the scope or cursor changed.`);
  const raw=await response.text();if(!current())return;
  if(new TextEncoder().encode(raw).length>32768)throw new Error('History page exceeded its metadata limit. Retry after checking the server.');
  let data;try{data=JSON.parse(raw);}catch{throw new Error('History returned an invalid response. Retry after checking the server.');}
  if(!validWorkerHistoryPage(data,scope,job,collection))throw new Error('History returned invalid or mismatched scope metadata. Retry from the beginning.');
  page.data=data;
 }catch(error){if(current()&&error.name!=='AbortError')page.error=error.message||'History unavailable. Retry.';}
 finally{if(current()){page.loading=false;paintWorkerHistory();}}
}
document.addEventListener('click',event=>{
 const button=event.target.closest('[data-worker-history]');if(!button||button.disabled)return;
 const entry=workerHistoryEntry();if(!entry)return;
 const action=button.dataset.workerHistory,collection=button.dataset.collection;
 if(action==='toggle'){
  entry.open=!entry.open;
  if(!entry.open){for(const page of Object.values(entry.pages)){page.controller?.abort();page.loading=false;}}
  paintWorkerHistory();if(entry.open&&!entry.pages.jobs.data)loadWorkerHistory('jobs');return;
 }
 if(action==='select'){
  const job=button.dataset.job;if(!entry.pages.jobs.data?.items.some(item=>item.id===job))return;
  for(const key of ['attempts','links']){entry.pages[key]?.controller?.abort();delete entry.pages[key];}
  entry.job=job;loadWorkerHistory('attempts');loadWorkerHistory('links');document.getElementById('worker-history-selected')?.focus();return;
 }
 if(!['jobs','attempts','links'].includes(collection))return;
 const page=entry.pages[collection]||{};
 if(action==='next'&&!page.data?.pagination.has_more)return;
 if(collection==='jobs'){entry.job='';for(const key of ['attempts','links']){entry.pages[key]?.controller?.abort();delete entry.pages[key];}}
 loadWorkerHistory(collection,action==='next'?page.data.pagination.next_cursor:action==='retry'?page.cursor:'');
});
