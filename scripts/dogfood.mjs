// Explicit, repeatable local dogfooding bootstrap. Never runs at app startup.
const endpoint = process.env.RC_URL || 'http://127.0.0.1:8090';
const headers = { 'Content-Type': 'application/json' };
if (process.env.RC_TOKEN) headers.Authorization = `Bearer ${process.env.RC_TOKEN}`;
async function command(action, refs, data) {
  const response = await fetch(`${endpoint}/api/commands`, {
    method: 'POST', headers,
    body: JSON.stringify({ action, actor: 'agent/bootstrap', ...refs, data }),
  });
  const value = await response.json();
  if (!response.ok) throw new Error(JSON.stringify(value));
  return value;
}
const response = await fetch(`${endpoint}/api/state`, { headers });
if (!response.ok) throw new Error(`Cannot read application state: ${response.status}`);
const state = await response.json();
const exists = id => Object.values(state).some(rows => Array.isArray(rows) && rows.some(row => row.id === id));
async function create(action, id, refs, data) {
  if (!exists(id)) await command(action, { id, ...refs }, data);
}
await create('create_product', 'rcp', {}, { name: 'Release Control Plane', description: 'Semantic development state for humans and autonomous coding agents.' });
await create('create_feature', 'rcp-bootstrap', { product_id: 'rcp' }, {
 title: 'RCP-001 · First usable control plane',
 problem: 'Mutable state files lose structured history and require conversation context; a shared dev branch couples unrelated releases.',
 goal: 'Persist intent, independently releasable integrations, verification evidence and structured handoffs through the same UI/API/MCP application layer.',
 requirements: ['One Go binary and PostgreSQL', 'Shared business logic for HTTP, UI and MCP', 'Append-only audit and evidence', 'Resume without previous conversation'],
 constraints: ['No automatic production changes', 'No distributed event sourcing', 'Keep the first version a modular monolith'],
 context: 'Architecture and schema rationale: docs/ARCHITECTURE.md and docs/CONTRACT.md. This feature is now the project development-state authority.',
 owner: 'human/vlad',
});
await create('create_integration', 'rcp-core', { feature_id: 'rcp-bootstrap' }, {
 title: 'INT-01 · Core vertical slice', objective: 'Create, work, verify, resolve findings, complete and hand off through durable shared application state.',
 rationale: 'Prove the domain and agent continuity before adding provider automation.', position: 1,
 acceptance_criteria: ['Domain invariant tests pass', 'HTTP and MCP round trip against real PostgreSQL', 'State survives reopening storage', 'Usable embedded UI'],
 remaining: ['Review the bootstrap verification evidence and accept the first slice.'], owner: 'agent/bootstrap', working_areas: ['internal/**', 'web/**'],
});
await create('create_integration', 'rcp-migrate-state', { feature_id: 'rcp-bootstrap' }, {
 title: 'INT-02 · Migrate bootstrap development state', objective: 'Use this Feature and structured Handoffs for subsequent work; retire IMPLEMENTATION_STATE.md as the source of truth.',
 rationale: 'The product must replace its own temporary state-file scaffolding as soon as resume works.', position: 2,
 acceptance_criteria: ['Bootstrap intent, decisions and follow-up work are stored here', 'A fresh agent can resume rcp-bootstrap', 'Temporary file points to this feature only'],
 remaining: ['Verify fresh-agent resume and complete this tracked migration integration.'],
});
await create('create_integration', 'rcp-git-flux', { feature_id: 'rcp-bootstrap' }, {
 title: 'INT-03 · Read-only Git and Flux adapters', objective: 'Populate commit/deployment/reconciliation observations through provider interfaces.',
 rationale: 'Connect semantic development state to real DEV visibility after the core model is accepted.', position: 3, dependencies: ['rcp-core'],
 acceptance_criteria: ['Provider-neutral Git adapter', 'Read-only Flux source and reconciliation observations', 'Unknown runtime is never shown healthy'],
 remaining: ['Choose initial Git provider and credentials', 'Add read-only Git adapter', 'Add read-only Flux adapter', 'Add Kubernetes observation adapter'],
});
await create('record_decision', 'rcp-decision-monolith', { feature_id: 'rcp-bootstrap' }, {
 title: 'One application service, PostgreSQL, embedded browser UI', body: 'HTTP and MCP invoke the same commands. Individually typed JSON records plus transactional append-only events use one PostgreSQL implementation. The browser UI is embedded plain JavaScript.',
 reason: 'Keep startup and evolution simple while retaining structured state and provenance. Normalize hot query paths when scale requires it.',
});
await create('record_discovery', 'rcp-discovery-evidence', { feature_id: 'rcp-bootstrap', integration_id: 'rcp-core' }, {
 title: 'Finding resolution and verification success are separate actions', body: 'Resolving a finding does not alter the original failed check. Blocking verification still needs a passing rerun, and revision/environment matching protects against stale evidence.',
});
await create('handoff', 'rcp-bootstrap-handoff', { feature_id: 'rcp-bootstrap', integration_id: 'rcp-migrate-state' }, {
 title: 'Bootstrap development state moved into Release Control Plane',
 completed: ['Architecture and invariants documented', 'Domain/persistence and shared application commands implemented', 'Embedded UI, HTTP and MCP interfaces implemented', 'Tracked follow-up integrations created'],
 current: 'Validate first-slice evidence and accept the migration task.',
 remaining: ['Human acceptance of first slice', 'Read-only Git/Flux/Kubernetes adapters'],
 next: ['Call resume with feature_id rcp-bootstrap', 'Review verification evidence and repository tests', 'Continue INT-03 after core acceptance'],
 warnings: ['No live Git/Flux/Kubernetes integration yet', 'No automatic deployment or branch composition', 'Actor attribution is not RBAC', 'Use application state, not IMPLEMENTATION_STATE.md, for ongoing context'],
});
console.log(`Dogfooding state available: ${endpoint}/#rcp-bootstrap`);
