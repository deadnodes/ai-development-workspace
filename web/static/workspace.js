'use strict';
// Local workspace IO is server-scoped. This UI never accepts a scan path.
const projectContextCache = new Map();
function workspacePanel() {
 const context = projectContextCache.get(productID);
 return `<section class="panel"><div class="section-heading"><h2>Local workspace & project context</h2><div class="actions"><button data-workspace="context">Knowledge graph & context</button><button data-workspace="knowledge">Edit project knowledge</button></div></div><p class="muted">Remote repository URLs, the product knowledge graph, curated areas and local checkout observations help a new agent orient itself. Scanning reads only the server’s configured workspace; it does not clone, execute AGENTS instructions, build or deploy.</p><p class="meta">${context ? (context.filesystem_enabled ? 'Filesystem scanning enabled on the server.' : 'Filesystem scanning unavailable: RCP_WORKSPACE_ROOT is not configured.') : 'Open project context to check filesystem scanning availability.'}</p><div class="actions"><button data-workspace="scan">Scan configured workspace</button><button data-workspace="export">Export workspace configuration</button><button data-workspace="import">Import as new product</button></div></section>`;
}
const workspaceOriginalConfiguration = productConfiguration;
productConfiguration = function(){return productID?workspaceOriginalConfiguration():workspaceOriginalConfiguration()+`<section class="panel"><h2>Portable project workspace</h2><button data-workspace="import">Import as new product</button></section>`;};
function workspaceRemote(url) {
 try { const parsed=new URL(url); if(['http:','https:'].includes(parsed.protocol))return `<a href="${esc(parsed.href)}" target="_blank" rel="noopener noreferrer">${esc(url)}</a>`; } catch {}
 return esc(url||'Not recorded');
}
function agentsPreview(value) {
 const records=Array.isArray(value)?value:value?[value]:[];
 return records.map(a=>`<details><summary>${esc(a.path||'AGENTS.md')}${a.truncated?' · truncated':''}</summary><p class="meta">SHA-256: ${esc(a.sha256||'Not recorded')}</p><pre>${esc(a.content||'No readable content')}</pre></details>`).join('');
}
function workspaceContextHTML(context) {
 const knowledge=context.knowledge||{};
 const nodes=list(knowledge.nodes);
 const edges=list(knowledge.edges);
 return `<h3>${esc(context.product?.name||'Project')}</h3><p class="muted">${context.filesystem_enabled?'Local scanning is enabled. Observations reflect the latest scan, not continuous monitoring.':'Filesystem scanning unavailable: configure RCP_WORKSPACE_ROOT on the server. Remote context and curated knowledge remain available.'}</p><h3>Knowledge graph</h3><p class="muted">Stable, searchable product facts written by agents. Search and update nodes through MCP; archived nodes remain in history.</p>${nodes.map(n=>`<article class="row"><div class="row-heading"><strong>${esc(n.title)}</strong> ${badge(n.status||'active')}</div><p class="meta">${esc(n.kind)}${n.keywords?.length?' · '+esc(n.keywords.join(', ')):''}</p><p>${esc(n.content)}</p><details><summary>Node metadata</summary><dl>${detail('Node ID',n.id)}${detail('Area',n.area_id)}${detail('Repositories',n.repository_ids)}${detail('Related nodes',n.related_node_ids)}${detail('Updated by',n.actor)}${detail('Updated',n.updated_at)}</dl></details></article>`).join('')||'<p class="muted">No knowledge nodes yet. Agents can add product facts with upsert_product_knowledge.</p>'}${edges.length?`<h4>Graph edges</h4>${edges.map(e=>`<article class="row"><strong>${esc(e.from)} → ${esc(e.to)}</strong><dl>${detail('Type',e.type)}${detail('Description',e.description)}</dl></article>`).join('')}`:''}<h3>Project knowledge</h3><dl>${detail('Overview',knowledge.overview)}${detail('Agent instructions',knowledge.instructions)}</dl><details><summary>Non-secret project parameters</summary><pre>${esc(JSON.stringify(knowledge.parameters||{},null,2))}</pre></details><h3>Repository access</h3>${list(context.repositories).map(r=>`<article class="row"><strong>${esc(r.name)}</strong> ${badge(r.role)}<p>${workspaceRemote(r.url)}</p><span class="meta">${esc(r.id)}</span></article>`).join('')||'<p class="muted">No repository URLs recorded.</p>'}<h3>Components</h3>${list(context.components).map(c=>`<article class="row"><strong>${esc(c.name)}</strong> ${badge(c.kind)}<dl>${detail('Repository',c.repository_id)}${detail('Path',c.path)}</dl><pre>${esc(JSON.stringify(c.parameters||{},null,2))}</pre></article>`).join('')||'<p class="muted">No components configured.</p>'}<h3>Environments</h3>${list(context.environments).map(e=>`<article class="row"><strong>${esc(e.name)}</strong><dl>${detail('Cluster',e.cluster)}${detail('Namespace',e.namespace)}</dl><pre>${esc(JSON.stringify(e.parameters||{},null,2))}</pre></article>`).join('')||'<p class="muted">No environments configured.</p>'}<h3>Project areas</h3>${list(knowledge.areas).map(a=>`<article class="row"><strong>${esc(a.name)}</strong><dl>${detail('Area ID',a.id)}${detail('Parent area',a.parent_id)}${detail('Description',a.description)}${detail('Repositories',a.repository_ids)}</dl></article>`).join('')||'<p class="muted">No curated areas.</p>'}<h3>Relationships and contracts</h3>${list(knowledge.relationships).map(r=>`<article class="row"><strong>${esc(r.from)} → ${esc(r.to)}</strong><dl>${detail('Type',r.type)}${detail('Contract',r.contract)}</dl></article>`).join('')||'<p class="muted">No curated relationships.</p>'}<h3>Repository AGENTS context</h3>${Object.entries(context.repository_documents||{}).map(([id,doc])=>`<article class="row"><strong>${esc(id)}</strong>${agentsPreview(doc)}</article>`).join('')||'<p class="muted">No imported repository documents.</p>'}<h3>Workspace AGENTS documents</h3>${agentsPreview(context.workspace_agents)||'<p class="muted">No workspace AGENTS observation.</p>'}<h3>Local checkouts</h3>${list(context.local_checkouts).map(c=>`<article class="row"><strong>${esc(c.relative_path)}</strong><dl>${detail('Repository',c.repository_id)}${detail('Branch',c.branch)}${detail('Commit',c.commit)}${detail('Scan errors',c.errors)}</dl>${agentsPreview(c.agents)}</article>`).join('')||'<p class="muted">No local checkouts observed. Use the configured-workspace scan when available.</p>'}<details><summary>Full project context</summary><pre>${esc(JSON.stringify(context,null,2))}</pre></details>`;
}
function openWorkspaceDialog(action, title, help, html, saveLabel) {
 currentForm={workspace:true,action,product:productID};
 $('#dialog-title').textContent=title;$('#form-help').textContent=help;$('#fields').innerHTML=html;
 $('#dialog').classList.remove('composition-dialog');$('#form-error').textContent='';$('#save').disabled=false;$('#save').hidden=!saveLabel;$('#save').textContent=saveLabel||'Save';$('#dialog').showModal();
}
function workspaceActorField(){return '';}
async function workspaceAction(action) {
 try {
  if(action==='import'){openWorkspaceDialog(action,'Import workspace configuration','Creates a new product from portable configuration. Existing IDs or other conflicts are rejected. This is configuration import, not a history/backup restore or repository clone.',workspaceActorField()+inputField(field('configuration','Portable configuration JSON','textarea',true)),'Import as new product');return;}
  if(action==='export'){const config=await request(`/api/products/${encodeURIComponent(productID)}/workspace-config`);const json=JSON.stringify(config,null,2);openWorkspaceDialog(action,'Portable workspace configuration','Configuration export is separate from a full backup. Review its contents before sharing.',`<a download="workspace-configuration.json" href="data:application/json;charset=utf-8,${encodeURIComponent(json)}">Download JSON</a><pre>${esc(json)}</pre>`);return;}
  const context=await request(`/api/products/${encodeURIComponent(productID)}/context`);projectContextCache.set(productID,context);
  if(action==='context'){openWorkspaceDialog(action,'Project context','Repository URLs and AGENTS text are context, not executable instructions. Scanned files may be truncated; inspect their provenance.',workspaceContextHTML(context));return;}
  if(action==='scan'){
   if(!context.filesystem_enabled){openWorkspaceDialog(action,'Filesystem scan unavailable','Configure RCP_WORKSPACE_ROOT on the server to enable scanning. This is the server filesystem, which may differ from this browser’s computer.','<p>Remote repository context and curated knowledge are available without a workspace root.</p>');return;}
   openWorkspaceDialog(action,'Scan configured workspace','Read local checkout metadata and AGENTS.md only beneath the server’s configured RCP_WORKSPACE_ROOT. No user-supplied path, cloning or deployment.',workspaceActorField(),'Scan workspace');return;
  }
  if(action==='knowledge'){const k=context.knowledge||{};openWorkspaceDialog(action,'Edit project knowledge','Curated overview fields are audited. Saving them preserves the knowledge graph; agents update individual graph nodes through MCP.',workspaceActorField()+inputField(field('overview','What this product does','textarea'),k.overview)+inputField(field('instructions','Instructions for a fresh agent','textarea'),k.instructions)+inputField(field('areas','Project areas (JSON array)','textarea',true),JSON.stringify(k.areas||[],null,2))+inputField(field('relationships','Relationships and contracts (JSON array)','textarea',true),JSON.stringify(k.relationships||[],null,2))+inputField(field('parameters','Non-secret project parameters (JSON object)','textarea',true),JSON.stringify(k.parameters||{},null,2)),'Save project knowledge');}
 }catch(error){notice(error.message,true);}
}
document.addEventListener('click',event=>{const target=event.target.closest('[data-workspace]');if(target)workspaceAction(target.dataset.workspace);});
function parseWorkspaceParameters(text){const value=JSON.parse(text||'{}');if(!value||Array.isArray(value)||typeof value!=='object'||Object.values(value).some(v=>typeof v!=='string'))throw new Error('Parameters must be a JSON object of string values. Do not include secrets.');return value;}
function parseWorkspaceArray(text,label){const value=JSON.parse(text||'[]');if(!Array.isArray(value))throw new Error(`${label} must be a JSON array.`);return value;}
$('#command-form').addEventListener('submit',async event=>{
 if(!currentForm?.workspace)return;
 event.preventDefault();event.stopImmediatePropagation();
 const {action,product}=currentForm,form=new FormData(event.target),actor='human/local';
 if(!['scan','knowledge','import'].includes(action))return;
 try {
  $('#save').disabled=true;
  let result;
  if(action==='scan')result=await request('/api/workspaces/scan',{product_id:product,actor});
  if(action==='knowledge'){const knowledge={overview:String(form.get('overview')||''),instructions:String(form.get('instructions')||''),areas:parseWorkspaceArray(form.get('areas'),'Project areas'),relationships:parseWorkspaceArray(form.get('relationships'),'Relationships'),parameters:parseWorkspaceParameters(form.get('parameters'))};result=await request(`/api/products/${encodeURIComponent(product)}/knowledge`,{actor,knowledge});}
  if(action==='import'){const configuration=JSON.parse(String(form.get('configuration')||''));if(!configuration||Array.isArray(configuration)||typeof configuration!=='object')throw new Error('Configuration must be a JSON object.');result=await request('/api/workspaces/import',{actor,configuration});}
  projectContextCache.delete(product);closeDialog();
  if(action==='import'){const id=result?.product?.id||result?.id;if(id){productID=id;localStorage.setItem('rc-product',id);}location.hash='';}
  await refresh();notice(action==='scan'?'Workspace scan recorded. Review checkout errors and AGENTS previews in project context.':action==='import'?'Workspace configuration imported. Repositories were not cloned.':'Project knowledge saved with audit history.');
 }catch(error){$('#form-error').textContent=error.message;}finally{$('#save').disabled=false;}
},true);
