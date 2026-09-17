'use strict';
// Flow forms submit the same commands as MCP; readiness is decided by the server.
const scenarioDefinitionFields=[field('title','Scenario title','text',true),field('objective','Behavior to verify','textarea',true),field('mechanism','Execution mechanism','select',true,['manual','browser','e2e','integration','unit','regression','smoke','migration','custom']),field('preconditions','Preconditions','lines'),field('steps','Execution steps','lines',true),field('expected_outcomes','Expected outcomes','lines',true),field('integration_ids','Integrations covered','product_integrations'),field('blocking','Required for release verification','boolean')];
Object.assign(forms,{
 assemble_environment:{label:'Assemble active work',help:'Creates a new immutable DEV/TEST composition from the latest captured branch revisions. Leave integrations empty to include all active captured work. The generated branches are disposable; the composition and operation remain the source of truth. A merge conflict pauses the operation and asks for resolution.',fields:[field('environment_id','Target environment','environment',true),field('name','Composition name'),field('integration_ids','Integrations (empty = all active captured work)','product_integrations')]},
 reconcile_composition:{label:'Reconcile this composition',help:'Starts real Git composition, builds and configured environment updates for these exact revisions. Parent and child operations preserve evidence. Runtime still requires an attributed observation.',fields:[]},
 prepare_release_candidate:{label:'Prepare release candidate',help:'Select ready integration revisions. This action advances the configured main branches to the selected candidate and builds exact artifacts. Scenario verification is required before promotion. Recheck the pinned base and selected revisions before approving.',fields:[field('name','Candidate name','text',true),field('environment_id','Promotion target environment','environment',true),field('approve_main_update','I approve advancing configured main branches to these exact selected revisions','boolean')]},
 promote_release_candidate:{label:'Promote this candidate',help:'Promotes the exact verified candidate artifacts. The server checks current scenario versions, complete component evidence and open findings. This can update the configured production GitOps target. Runtime health is recorded separately.',fields:[field('approve','I approve promoting this exact candidate to its configured environment','boolean')]},
 create_hotfix:{label:'Create hotfix integration',help:'Preserve the finding and feature context in a small independent fix. The hotfix follows the same verification and release requirements.',fields:[field('title','Hotfix title','text',true),field('objective','Fix objective','textarea',true),field('finding_id','Related finding (optional)','finding'),field('repositories','Repositories','repositories'),field('remaining','Remaining work','lines')]},
 record_runtime_observation:{label:'Record runtime observation',help:'Record what you actually observed for this exact child operation. These are human/agent assertions; the control plane is not claiming that it queried Flux or Kubernetes. Explain the evidence and report unhealthy outcomes too.',fields:[field('environment_id','Observed environment','environment',true),field('gitops_commit','Observed GitOps commit','text',true),field('artifact_digest','Observed artifact digest','text',true),field('healthy','I observed healthy runtime for this commit and digest','boolean'),field('details','Observation details and evidence references','textarea',true)]},
 create_test_scenario:{label:'Define test scenario',help:'Describe behavior and expected outcomes. The definition is versioned; recording a definition does not execute the test.',fields:scenarioDefinitionFields},
 revise_test_scenario:{label:'Create scenario revision',help:'Append a complete new definition. Earlier versions and runs remain historical; the latest blocking version needs fresh passing evidence.',fields:scenarioDefinitionFields},
 record_scenario_run:{label:'Record scenario execution',help:'Choose an exact deployed composition or built release candidate. Component source/digest identities come from its child operations. Record actual execution results and evidence; this form does not run a browser or test executor.',fields:[field('scenario_version_id','Exact scenario version','scenario',true),field('target','Exact tested target','scenario_target',true),field('environment_id','Execution environment (optional for candidate)','environment'),field('result','Observed result','select',true,['passed','failed','blocked']),field('observations','Observations','lines',true),field('artifacts','Evidence artifacts','artifacts'),field('finding_ids','Linked findings','findings')]}
});
forms.configure_environment.label='Configure environment GitOps';
forms.configure_environment.help='Explicitly configure DEV, TEST or PROD delivery for one application. Enabling the target permits real GitOps operations; candidate promotion still requires explicit approval and verification.';
forms.configure_environment.fields.find(f=>f.name==='purpose').options=['DEV','TEST','PROD'];
forms.configure_environment.fields.find(f=>f.name==='allow_deploy').label='Enable real deployment operations for this target';
function flowOptionsFor(f){
 if(f.type==='finding')return inFeature('findings');
 if(f.type==='findings')return productItems('findings');
 if(f.type==='scenario')return productItems('scenario_versions').map(s=>({...s,title:`${s.title} · version ${s.version} (${s.id})`}));
 if(f.type==='scenario_target')return scenarioTargets().map(t=>({id:t.value,title:t.label}));
 return null;
}
function scenarioTargets(){
 const operations=productItems('operations');
 const candidates=operations.filter(o=>o.kind==='RELEASE_CANDIDATE'&&o.status==='SUCCEEDED'&&o.deployment_state==='READY_FOR_VERIFICATION').map(o=>({value:`candidate:${o.id}`,label:`Candidate ${o.composition_snapshot?.name||o.id}`,operation:o}));
 const compositions=new Map();for(const o of operations)if(o.kind==='COMPOSE'&&o.status==='SUCCEEDED'&&o.deployment_state==='DEPLOYED'&&o.composition_snapshot?.id)compositions.set(o.composition_snapshot.id,{value:`composition:${o.composition_snapshot.id}`,label:`Deployed ${o.composition_snapshot.name||o.composition_snapshot.id}`,operation:o});
 return [...candidates,...compositions.values()];
}
function flowFormValues(action,attrs){
 if(action==='assemble_environment')return {environment_id:productItems('environments').find(e=>['DEV','TEST'].includes(String(e.name).toUpperCase()))?.id};
 if(action==='prepare_release_candidate')return {approve_main_update:false};
 if(action==='promote_release_candidate')return {approve:false};
 if(action==='record_runtime_observation'){const op=productItems('operations').find(o=>o.id===attrs.id);return {healthy:false,environment_id:op?.environment_id,gitops_commit:op?.gitops_result?.commit_sha,artifact_digest:op?.artifact?.digest};}
 if(action==='create_hotfix'){const finding=inFeature('findings').find(f=>f.id===attrs.finding);return {finding_id:attrs.finding,title:finding?`Fix: ${finding.title}`:'',objective:finding?.body||''};}
 if(action==='revise_test_scenario')return productItems('scenario_versions').find(s=>s.id===attrs.scenario)||{};
 if(action==='record_scenario_run')return {scenario_version_id:attrs.scenario,target:attrs.target,result:'blocked'};
 return null;
}
function setupFlowForm(action){
 if(action!=='record_scenario_run')return;
 const panel=document.createElement('div');panel.id='scenario-component-evidence';$('#fields').append(panel);$('#input-target').onchange=renderScenarioEvidence;renderScenarioEvidence();
}
function targetComponents(value){
 const target=scenarioTargets().find(t=>t.value===value);if(!target)return [];
 return list(target.operation.child_ids).map(id=>productItems('operations').find(o=>o.id===id)).filter(Boolean).map(op=>({application_id:op.application_id,operation_id:op.id,source_sha:op.snapshot?.revision?.head_commit||'',artifact_digest:op.artifact?.digest||''}));
}
function renderScenarioEvidence(){
 const value=$('#input-target').value,components=targetComponents(value),deployed=value.startsWith('composition:');
 $('#scenario-component-evidence').innerHTML='<h3>Exact tested outputs</h3>'+(components.map(c=>`<fieldset class="application-row" data-scenario-component="${esc(c.operation_id)}"><legend>${esc(productItems('applications').find(a=>a.id===c.application_id)?.name||c.application_id)}</legend><dl>${detail('Child operation',c.operation_id)}${detail('Source commit',c.source_sha)}${detail('Artifact digest',c.artifact_digest)}</dl><div class="field"><label for="evidence-${esc(c.operation_id)}">${deployed?'Deployment and execution evidence *':'Execution location / deployment evidence (optional)'}</label><textarea id="evidence-${esc(c.operation_id)}" data-deployment-evidence ${deployed?'required':''}></textarea></div></fieldset>`).join('')||'<p class="muted">Choose a target with complete component outputs.</p>');
}
function transformFlowCommand(command,fd,attrs,form){
 const action=command.action;
 if(['assemble_environment','prepare_release_candidate','create_test_scenario','revise_test_scenario','record_scenario_run'].includes(action)){command.product_id=productID;delete command.feature_id;delete command.integration_id;}
 if(['reconcile_composition','promote_release_candidate','record_runtime_observation'].includes(action)){delete command.feature_id;delete command.integration_id;delete command.product_id;}
 if(action==='prepare_release_candidate'&&!command.data.approve_main_update)throw new Error('Review the exact revisions and explicitly approve advancing main before preparing this candidate.');
 if(action==='promote_release_candidate'&&!command.data.approve)throw new Error('Explicit approval is required to promote this exact candidate.');
 if(action==='create_hotfix'&&!command.data.finding_id)delete command.data.finding_id;
 if(action==='revise_test_scenario'){const version=productItems('scenario_versions').find(s=>s.id===attrs.scenario);if(!version)throw new Error('Scenario version is unavailable. Refresh before revising.');command.data.scenario_id=version.scenario_id;}
 if(action==='record_scenario_run'){
  const target=String(fd.get('target')||'');const selected=scenarioTargets().find(t=>t.value===target);if(!selected)throw new Error('Choose a completed target with exact component outputs.');
  delete command.data.target;if(!command.data.environment_id)delete command.data.environment_id;
  const [kind,...identity]=target.split(':');command.data[kind==='candidate'?'candidate_operation_id':'composition_id']=identity.join(':');
  const components=targetComponents(target);if(!components.length||components.length!==list(selected.operation.child_ids).length)throw new Error('The target is missing component operations.');
  command.data.components=components.map(c=>{if(!c.source_sha||!c.artifact_digest)throw new Error('All tested components require exact source and artifact identities.');const row=Array.from(form.querySelectorAll('[data-scenario-component]')).find(r=>r.dataset.scenarioComponent===c.operation_id);const evidence=row?.querySelector('[data-deployment-evidence]')?.value.trim()||'';if(kind==='composition'&&!evidence)throw new Error('Record deployment/execution evidence for every component.');return {...c,deployment_evidence:evidence};});
 }
}
function flowCompositionStatus(c){const op=productItems('operations').filter(o=>o.kind==='COMPOSE'&&o.composition_snapshot?.id===c.id).at(-1);return op?`${op.status} · ${op.deployment_state||op.phase||'Execution started'}`:'Planned / not deployed';}
function flowCompositionActions(c){return `<div class="actions">${button('reconcile_composition','Reconcile exact composition',{id:c.id})}<a href="#operations">Operation evidence</a></div>`;}
function flowFeaturePanel(feature){return `<section class="panel"><div class="section-heading"><h2>Hotfix work</h2>${button('create_hotfix','Create hotfix integration')}</div><p class="muted">Keep fixes linked to this feature’s intent and findings. Verification and release selection remain explicit.</p>${inFeature('findings').filter(f=>f.status!=='resolved').map(f=>`<p>${esc(f.title)} ${button('create_hotfix','Plan fix',{finding:f.id})}</p>`).join('')}</section>`;}
function flowOperationActions(op){
 if(op.kind==='RELEASE_CANDIDATE'&&op.status==='SUCCEEDED'&&op.deployment_state==='READY_FOR_VERIFICATION')return `<p class="muted">Built candidate. Current blocking scenarios and findings determine promotion eligibility on the server.</p><div class="actions">${button('record_scenario_run','Record candidate verification',{target:`candidate:${op.id}`})}${button('promote_release_candidate','Review exact candidate promotion',{id:op.id})}</div>`;
 if(op.parent_id&&op.kind==='DEPLOY'&&op.gitops_result?.commit_sha&&op.artifact?.digest&&!op.build_only)return `<div class="actions">${button('record_runtime_observation','Record runtime observation',{id:op.id})}</div>`;
 return '';
}
function flowProductPanel(){
 const candidates=productItems('operations').filter(o=>o.kind==='RELEASE_CANDIDATE');const versions=productItems('scenario_versions');const latest=new Map();for(const s of versions)latest.set(s.scenario_id,s);
 return `<section class="panel"><div class="section-heading"><h2>Release candidates</h2>${button('prepare_release_candidate','Prepare selected ready work')}</div><p class="muted">Select exact independent work, advance the approved main branches, build immutable artifacts, verify, then promote the same candidate.</p>${candidates.slice().reverse().map(o=>`<article data-live-key="candidate-${esc(o.id)}" class="row"><h3>${esc(o.composition_snapshot?.name||o.id)}</h3>${badge(o.status)} ${badge(o.deployment_state)}<dl>${detail('Candidate operation',o.id)}${detail('Target environment',o.environment_id)}${detail('Child operations',o.child_ids)}${detail('State / attention',o.detail)}</dl>${flowOperationActions(o)}<button data-provider-query="/api/operations/${encodeURIComponent(o.id)}" data-query-title="Exact candidate evidence">Inspect source, digests and history</button></article>`).join('')||'<p class="muted">No release candidates prepared.</p>'}</section><section class="panel"><div class="section-heading"><h2>Versioned test scenarios</h2><div class="actions">${button('create_test_scenario','Define scenario')}${button('record_scenario_run','Record execution')}</div></div>${Array.from(latest.values()).map(s=>`<article data-live-key="scenario-${esc(s.scenario_id)}" class="row"><div class="row-heading"><h3>${esc(s.title)} · v${esc(s.version)}</h3>${badge(s.blocking?'blocking':'advisory')}</div><p>${esc(s.objective)}</p><dl>${detail('Preconditions',s.preconditions)}${detail('Steps',s.steps)}${detail('Expected outcomes',s.expected_outcomes)}${detail('Integrations',s.integration_ids)}</dl><div class="actions">${button('revise_test_scenario','New definition version',{scenario:s.id})}${button('record_scenario_run','Record execution',{scenario:s.id})}</div><details><summary>Definition and execution history</summary><pre>${esc(JSON.stringify({versions:versions.filter(v=>v.scenario_id===s.scenario_id),runs:productItems('scenario_runs').filter(r=>versions.some(v=>v.scenario_id===s.scenario_id&&v.id===r.scenario_version_id))},null,2))}</pre></details></article>`).join('')||'<p class="muted">Define scenarios before recording actual verification evidence.</p>'}</section>`;
}
const originalOperationCard=operationCard;
operationCard=function(op){const links=op.parent_id||list(op.child_ids).length?`<dl>${detail('Parent operation',op.parent_id)}${detail('Child operations',op.child_ids)}</dl>`:'';return originalOperationCard(op).replace(/<\/article>$/,links+flowOperationActions(op)+'</article>');};
