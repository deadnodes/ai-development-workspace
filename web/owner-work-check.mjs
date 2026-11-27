import assert from 'node:assert/strict';
import {execFile} from 'node:child_process';
import {mkdtemp, readFile, rm} from 'node:fs/promises';
import {createServer} from 'node:http';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {promisify} from 'node:util';

async function checkRevokedOwnerWork() {
  const result = document.body.appendChild(document.createElement('output'));
  result.id = 'owner-work-check';
  const snapshot = {products:[{id:'p',name:'Shared source'}],features:[{id:'f',product_id:'p',title:'Source',status:'active'}]};
  const privateGraph = {
    nodes:[{id:'hidden',title:'Private prerequisite'}],
    edges:[{id:'edge',product_id:'p',from_feature_id:'f',to_feature_id:'hidden',status:'accepted',type:'requires',rationale:'Private edge rationale'}],
  };
  const privateQuestions = {items:[{id:'secret',title:'Private question',body:'Private answer context',kind:'decision',age_seconds:0,context_refs:[{title:'Private context title'}]}],next_cursor:'secret-cursor'};
  let graph = privateGraph, graphReads = 0, stateStatus = 200, delayedGraph;
  let questions = privateQuestions, questionReads = 0, delayedQuestions;
  const json = (value, status = 200) => new Response(JSON.stringify(value), {status});
  window.setInterval = () => 0;
  window.fetch = async (path, options) => {
    if (path === '/auth/me') return json({}, 404);
    if (path === '/api/statuses') return json({catalog:{},transitions:{}});
    if (path.startsWith('/api/ui-state?')) {
      return stateStatus === 304 ? new Response(null,{status:304}) : json(snapshot, stateStatus);
    }
    if (path.includes('/dependency-graph')) {
      if (options.cache !== 'no-store') throw new Error('Graph fetch must bypass browser cache');
      graphReads++;
      return graph === 'delay' ? new Promise(resolve => { delayedGraph = () => resolve(json(privateGraph)); }) :
        graph === 'denied' ? json({error:'Access denied'}, 403) : json(graph);
    }
    if (path.startsWith('/api/questions?')) {
      if (options.cache !== 'no-store') throw new Error('Inbox fetch must bypass browser cache');
      questionReads++;
      return questions === 'delay' ? new Promise(resolve => { delayedQuestions = () => resolve(json(privateQuestions)); }) :
        questions === 'denied' ? new Response('Not found', {status:404}) : json(questions);
    }
    return json({}, 404);
  };
  const visible = () => document.querySelector('.owner-dependencies')?.textContent || '';
  const waitFor = async predicate => {
    for (let attempt = 0; attempt < 80; attempt++) {
      if (predicate()) return;
      await new Promise(resolve => setTimeout(resolve, 10));
    }
    throw new Error(`Expected render did not occur: ${document.querySelector('#main').textContent.trim().slice(0, 180)}`);
  };
  const absent = () => {
    if (/Private prerequisite|Private edge rationale/.test(document.querySelector('#main').textContent)) throw new Error('Revoked graph details are still visible');
  };
  const inboxAbsent = () => {
    if (/Private question|Private answer context|Private context title/.test(document.querySelector('#main').textContent) || document.querySelector('[data-work-more]')) throw new Error('Revoked inbox details or pagination are still visible');
    const page = ownerWorkCache.get('questions:p');
    if (page?.items?.length || page?.next_cursor) throw new Error('Revoked inbox details or pagination are still cached');
  };
  const inboxVisible = () => /Private answer context/.test(document.querySelector('#main').textContent) && /Private context title/.test(document.querySelector('#main').textContent) && document.querySelector('[data-work-more]');
  try {
    await refresh();
    await waitFor(() => /Private prerequisite/.test(visible()) && /Private edge rationale/.test(visible()));

    graph = 'delay';
    revalidateOwnerGraph(selectedFeature());
    await waitFor(() => delayedGraph);
    graph = {nodes:[],edges:[]};
    const refreshed = refresh();
    absent();
    await refreshed;
    await waitFor(() => ownerWorkCache.get('graph:p')?.loaded && !ownerWorkCache.get('graph:p')?.loading);
    delayedGraph();
    await new Promise(resolve => setTimeout(resolve, 10));
    absent();

    graph = privateGraph;
    await refresh();
    await waitFor(() => /Private prerequisite/.test(visible()));
    graph = 'denied';
    const previousReads = graphReads;
    stateStatus = 304;
    await pollLive();
    await waitFor(() => graphReads > previousReads && /Access denied/.test(visible()));
    absent();
    if (ownerWorkCache.get('graph:p').edges) throw new Error('Denied graph retained private edges');

    graph = privateGraph;
    stateStatus = 200;
    await refresh();
    await waitFor(() => /Private prerequisite/.test(visible()));
    stateStatus = 503;
    await refresh();
    absent();
    stateStatus = 200;
    history.replaceState(null, '', '#questions');
    await refresh();
    await waitFor(inboxVisible);

    questions = 'denied';
    document.querySelector('[data-work-refresh]').click();
    inboxAbsent();
    await waitFor(() => ownerWorkCache.get('questions:p')?.error);
    inboxAbsent();

    questions = privateQuestions;
    await refresh();
    await waitFor(inboxVisible);
    questions = 'delay';
    revalidateOwnerQuestions();
    await waitFor(() => delayedQuestions);
    questions = 'denied';
    await pollLive();
    await waitFor(() => ownerWorkCache.get('questions:p')?.error);
    delayedQuestions();
    await new Promise(resolve => setTimeout(resolve, 10));
    inboxAbsent();

    questions = privateQuestions;
    await refresh();
    await waitFor(inboxVisible);
    questions = 'denied';
    stateStatus = 304;
    const readsBeforeDenial = questionReads;
    await pollLive();
    await waitFor(() => questionReads > readsBeforeDenial && ownerWorkCache.get('questions:p')?.error);
    inboxAbsent();
    result.textContent = 'PASS: dependency graph and question inbox revocation';
  } catch (error) {
    result.textContent = `FAIL: ${error.message}`;
  }
}

const browser = process.env.RCP_CHROME;
assert.ok(browser, 'Set RCP_CHROME to the path to Chromium or chrome-headless-shell');
const profile = await mkdtemp(join(tmpdir(), 'rcp-owner-work-browser-'));
const root = new URL('./static/', import.meta.url);
const server = createServer(async (request, response) => {
  const path = new URL(request.url, 'http://localhost').pathname;
  try {
    if (path === '/owner-work-check.js') {
      response.setHeader('Content-Type', 'text/javascript');
      response.end(`(${checkRevokedOwnerWork.toString()})();`);
    } else if (path === '/') {
      response.setHeader('Content-Type', 'text/html');
      response.end((await readFile(new URL('index.html', root), 'utf8')).replace('</head>', '<script defer src="/owner-work-check.js"></script></head>'));
    } else {
      assert.match(path, /^\/[\w.-]+\.(js|css)$/);
      response.setHeader('Content-Type', path.endsWith('.js') ? 'text/javascript' : 'text/css');
      response.end(await readFile(new URL(path.slice(1), root)));
    }
  } catch {
    response.writeHead(404).end();
  }
});

try {
  await new Promise((resolve, reject) => server.once('error', reject).listen(0, '127.0.0.1', resolve));
  const {stdout} = await promisify(execFile)(browser, ['--headless','--no-sandbox','--single-process','--disable-gpu','--no-first-run',`--user-data-dir=${profile}`,'--dump-dom','--virtual-time-budget=4000',`http://127.0.0.1:${server.address().port}/?product=p#f`], {timeout:20000});
  const outcome = stdout.match(/<output id="owner-work-check">([^<]*)/);
  assert.equal(outcome?.[1], 'PASS: dependency graph and question inbox revocation');
  console.log(outcome[1]);
} finally {
  server.close();
  await rm(profile,{recursive:true,force:true});
}
