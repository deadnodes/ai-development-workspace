const ownerWorkCache = new Map();

function resetOwnerWork() {
  ownerWorkCache.clear();
}

function invalidateOwnerWorkGraphs() {
  for (const key of ownerWorkCache.keys()) {
    if (key.startsWith('graph:')) ownerWorkCache.delete(key);
  }
  document.querySelectorAll('.owner-dependencies').forEach(panel => panel.remove());
}

function ownerWorkDecisionOwner(product) {
  const owner = list(state.products).find(item => item.id === product)?.owner;
  return authMode === 'local' ? !owner || owner === 'local' : authPrincipal?.kind === 'human' && authPrincipal.id === owner;
}

function questionsView() {
  const key = `questions:${productID}`;
  const page = ownerWorkCache.get(key);
  return `<div class="page-heading"><div><span class="eyebrow">OWNER INBOX</span><h1>My open questions</h1></div><button type="button" data-work-refresh>Refresh</button></div>
    ${page?.error ? `<p class="error" role="alert">${esc(page.error)}</p>` : ''}
    ${page?.items?.length ? `<form data-owner-answers>
      ${page.items.map(question => `<fieldset class="owner-question" data-question-id="${esc(question.id)}" data-live-key="question-${esc(question.id)}">
        <legend>${esc(question.title)}</legend><p class="meta">${esc(question.kind)}${question.blocking ? ' · Blocking' : ''} · ${Math.floor(question.age_seconds / 60)} min old</p>
        <p class="owner-question-body">${esc(question.body)}</p>
        ${question.context_refs?.length ? `<p class="meta">Context: ${question.context_refs.map(ref => esc(ref.title)).join(', ')}</p>` : ''}
        ${question.recommendation ? `<p><strong>Recommendation:</strong> ${esc(question.options?.find(option => option.id === question.recommendation)?.label || question.recommendation)}</p>` : ''}
        ${list(question.options).map(option => `<label class="owner-option"><input type="radio" name="option-${esc(question.id)}" value="${esc(option.id)}"> ${esc(option.label)}${option.consequence ? ` — ${esc(option.consequence)}` : ''}</label>`).join('')}
        <label class="owner-answer">Answer or explanation<textarea rows="2" placeholder="Leave blank to skip unless choosing an option"></textarea></label>
      </fieldset>`).join('')}
      <label class="owner-option"><input type="checkbox" required> I checked these answers against the recorded context.</label>
      <button type="submit">Submit answers</button>
    </form>` : page?.loaded ? '<p class="empty">No open questions for you.</p>' : page?.error ? '' : '<p class="muted" role="status">Loading questions…</p>'}
    ${page?.next_cursor ? '<button type="button" data-work-more>Load more questions</button>' : ''}`;
}

function featureOwnerWork(feature) {
  const graph = ownerWorkCache.get(`graph:${feature.product_id}`);
  const edges = list(graph?.edges).filter(edge => edge.from_feature_id === feature.id || edge.to_feature_id === feature.id);
  const titles = new Map(list(graph?.nodes).map(node => [node.id, node.title]));
  const promote = feature.status === 'idea' && ownerWorkDecisionOwner(feature.product_id) ? `<button type="button" data-promote-idea="${esc(feature.id)}">Promote to ready for execution</button>` : '';
  if (!promote && !edges.length && !graph?.error) return '';
  return `<section class="owner-dependencies" aria-label="Feature planning">${promote}
    ${graph?.error ? `<p class="error" role="alert">${esc(graph.error)}</p>` : ''}
    ${edges.length ? `<h2>Dependencies</h2><ul>${edges.map(edge => {
      const outgoing = edge.from_feature_id === feature.id;
      const other = outgoing ? edge.to_feature_id : edge.from_feature_id;
      const relation = edge.type === 'relates' ? 'Relates to' : outgoing ? 'Requires' : 'Required by';
      return `<li>${esc(relation)} <strong>${esc(titles.get(other) || other)}</strong> ${badge(edge.status)}${edge.rationale ? `<p class="meta">${esc(edge.rationale)}</p>` : ''}
        ${edge.status === 'proposed' && ownerWorkDecisionOwner(edge.product_id) ? `<div class="owner-edge-actions"><button type="button" data-dependency="${esc(edge.id)}" data-decision="accept">Accept</button><button type="button" data-dependency="${esc(edge.id)}" data-decision="reject">Reject</button></div>` : ''}</li>`;
    }).join('')}</ul>` : ''}</section>`;
}

async function loadOwnerWork(key, path, append = false) {
  const previous = ownerWorkCache.get(key);
  const graph = key.startsWith('graph:');
  if (previous?.loading) return;
  const pending = {...previous, loading: true, error: ''};
  ownerWorkCache.set(key, pending);
  const current = () => key === `questions:${productID}` && featureID() === 'questions' || key === `graph:${selectedFeature()?.product_id}`;
  try {
    const result = await request(path, undefined, graph ? {cache: 'no-store'} : undefined);
    if (ownerWorkCache.get(key) !== pending) return;
    if (append) result.items = [...previous.items, ...result.items];
    ownerWorkCache.set(key, {...result, loaded: true});
  } catch (error) {
    if (ownerWorkCache.get(key) !== pending) return;
    ownerWorkCache.set(key, {...(graph ? {} : previous), loading:false, error:error.message});
  }
  if (current()) render(true);
}

function questionListURL(cursor = '') {
  const query = new URLSearchParams({limit: '25'});
  if (productID) query.set('product_id', productID);
  if (cursor) query.set('cursor', cursor);
  return `/api/questions?${query}`;
}

function syncOwnerWork(route, feature) {
  if (route === 'questions' && !ownerWorkCache.has(`questions:${productID}`)) {
    void loadOwnerWork(`questions:${productID}`, questionListURL());
  } else if (feature && !refreshInFlight && !ownerWorkCache.has(`graph:${feature.product_id}`)) {
    revalidateOwnerGraph(feature);
  }
}

function revalidateOwnerGraph(feature) {
  if (feature && !refreshInFlight) {
    void loadOwnerWork(`graph:${feature.product_id}`, `/api/products/${encodeURIComponent(feature.product_id)}/dependency-graph?include_proposed=true`);
  }
}

$('#main').addEventListener('submit', async event => {
  const form = event.target.closest('[data-owner-answers]');
  if (!form) return;
  event.preventDefault();
  const answers = [...form.querySelectorAll('[data-question-id]')].flatMap(card => {
    const option = card.querySelector('input:checked');
    const text = card.querySelector('textarea').value.trim() || option?.parentElement.textContent.trim();
    if (!text) return [];
    return [{question_id: card.dataset.questionId, text, ...(option ? {option_id: option.value} : {}), validation:{verdict:'consistent', note:'Owner checked the answer against the recorded context.'}}];
  });
  if (!answers.length) return notice('Enter an answer or choose an option before submitting.', true);
  if (answers.length > 50) return notice('Submit up to 50 answers at a time.', true);
  const key = `questions:${productID}`, path = questionListURL();
  const control = form.querySelector('button[type=submit]');
  control.disabled = true;
  try {
    await request('/api/commands', {action:'answer_questions', data:{answers}});
    ownerWorkCache.delete(key);
    await loadOwnerWork(key,path);
    notice(`Saved ${answers.length} answer${answers.length === 1 ? '' : 's'}.`);
  } catch (error) { notice(error.message,true); }
  finally { control.disabled = false; }
});

$('#main').addEventListener('click', async event => {
  const control = event.target.closest('button');
  if (!control) return;
  const questionKey = `questions:${productID}`;
  if (control.hasAttribute('data-work-refresh')) return loadOwnerWork(questionKey,questionListURL());
  if (control.hasAttribute('data-work-more')) return loadOwnerWork(questionKey,questionListURL(ownerWorkCache.get(questionKey)?.next_cursor),true);
  const feature = selectedFeature();
  let command;
  if (control.dataset.promoteIdea) command = {action:'update_feature',feature_id:control.dataset.promoteIdea,data:{status:'ready_for_execution'}};
  if (control.dataset.dependency) command = {action:'decide_dependency',data:{dependency_id:control.dataset.dependency,decision:control.dataset.decision}};
  if (!command) return;
  control.disabled = true;
  try {
    await request('/api/commands', command);
    ownerWorkCache.delete(`graph:${feature?.product_id}`);
    await refresh();
    notice(control.dataset.promoteIdea ? 'Idea promoted.' : 'Dependency decision saved.');
  } catch (error) { notice(error.message,true); }
  finally { control.disabled = false; }
});
