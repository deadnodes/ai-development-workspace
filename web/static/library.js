'use strict';
// Package records are attributed evidence, not a package publishing executor.
Object.assign(forms,{
 classify_repository:{label:'Classify repository purpose',help:'Update this Product attachment’s role. Existing component kinds and environment mappings must remain compatible. The physical instance repository identity is unchanged.',fields:[field('role','Repository purpose','repository_role',true)]},
 configure_publication:{label:'Configure library publication',help:'Record where this library is intended to publish. This does not upload a package, authenticate to a registry or create an environment deployment.',fields:[field('application_id','Library component','library_component',true),field('format','Package format','publication_format',true),field('registry_url','Registry URL','url',true),field('package_name','Package name','text',true)]},
 record_package_artifact:{label:'Record package artifact',help:'Record an artifact produced by an external build/publisher. Source, version, checksum and URI are caller-attested provenance; saving this record does not verify registry availability or publish anything.',fields:[field('application_id','Library component','library_component',true),field('publication_target_id','Publication target','publication_target',true),field('integration_id','Related integration (optional)','product_integrations_single'),field('source_commit','Exact source commit','text',true),field('version','Package version','text',true),field('checksum','Package checksum','text',true),field('uri','Published artifact URI','url',true),field('build_url','External build URL (optional)','url')]}
});
function repositoryPurpose(repo){return repo.role||productItems('repository_bindings').filter(b=>b.repository_id===repo.id).at(-1)?.role||'SOURCE';}
function latestRepositoryBindings(){return Array.from(new Map(productItems('repository_bindings').map(b=>[b.repository_id,b])).values());}
function libraryOptionsFor(f){
 const enums={component_kind:'component_kinds',repository_role:'repository_roles',publication_format:'publication_formats'};
 if(enums[f.type])return list(statusMetadata[enums[f.type]]).map(v=>({id:v,title:v}));
 if(f.type==='source_repository')return productItems('repositories').filter(r=>['SOURCE','APPLICATION','LIBRARY','MIXED'].includes(repositoryPurpose(r)));
 if(f.type==='library_component')return productItems('applications').filter(a=>a.kind==='LIBRARY');
 if(f.type==='publication_target'){const app=$('#input-application_id').value||currentForm?.attrs.application;return productItems('publication_targets').filter(p=>p.application_id===app).map(p=>({...p,title:`${p.package_name} · ${p.format} · ${p.registry_url}`}));}
 if(f.type==='product_integrations_single'){const ids=new Set(productItems('features').map(f=>f.id));return list(state.integrations).filter(i=>ids.has(i.feature_id));}
 return null;
}
function libraryFormValues(action,attrs){
 if(action==='classify_repository')return {role:productItems('repositories').find(r=>r.id===attrs.id)?.role||'SOURCE'};
 if(action==='create_repository')return {role:'APPLICATION'};
 if(action==='create_application')return {kind:'APPLICATION'};
 if(action==='configure_publication'||action==='record_package_artifact')return {application_id:attrs.application,publication_target_id:attrs.publication};
 return null;
}
function setupLibraryForm(action,attrs){
 if(action!=='record_package_artifact')return;
 const update=()=>{const select=$('#input-publication_target_id');select.innerHTML=libraryOptionsFor({type:'publication_target'}).map(p=>`<option value="${esc(p.id)}" ${p.id===attrs.publication?'selected':''}>${esc(p.title)}</option>`).join('');};
 $('#input-application_id').onchange=update;update();
}
function transformLibraryCommand(command){
 if(command.action==='classify_repository'){delete command.feature_id;delete command.integration_id;return;}
 if(!['configure_publication','record_package_artifact'].includes(command.action))return;
 command.product_id=productID;delete command.feature_id;
 if(command.action==='configure_publication')delete command.integration_id;
 if(command.action==='record_package_artifact'&&!command.data.build_url)delete command.data.build_url;
}
function libraryComponentPanel(app){
 const targets=productItems('publication_targets').filter(p=>p.application_id===app.id),artifacts=productItems('package_artifacts').filter(p=>p.application_id===app.id);
 return `<p class="muted">Library package publication. No environment deployment is configured for this component.</p><div class="actions">${button('configure_publication','Configure publication',{application:app.id})}${targets.length?button('record_package_artifact','Record package artifact',{application:app.id}):''}</div>${targets.map(p=>`<dl>${detail('Package',p.package_name)}${detail('Format',p.format)}${detail('Registry',p.registry_url)}${detail('Publication target',p.id)}</dl>`).join('')}<h4>Recorded package artifacts</h4><p class="muted">External publisher evidence; registry availability is not verified.</p>${artifacts.map(a=>`<article class="row"><h4>${esc(a.version)}</h4><dl>${detail('Source commit',a.source_commit)}${detail('Checksum',a.checksum)}${detail('Artifact URI',a.uri)}${detail('Build',a.build_url)}${detail('Integration',a.integration_id)}</dl><p class="meta">${esc(a.actor)} · ${esc(date(a.created_at))}</p></article>`).join('')||'<p class="muted">No package artifacts recorded.</p>'}`;
}
