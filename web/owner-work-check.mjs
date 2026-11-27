import assert from 'node:assert/strict';
import {execFile} from 'node:child_process';
import {mkdtemp, readFile, rm} from 'node:fs/promises';
import {createServer} from 'node:http';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {promisify} from 'node:util';

async function checkRevokedDependency() {
  const result = document.body.appendChild(document.createElement('output'));
  result.id = 'owner-work-check';
  const snapshot = {products:[{id:'p',name:'Shared source'}],features:[{id:'f',product_id:'p',title:'Source',status:'active'}]};
  const privateGraph = {
    nodes:[{id:'hidden',title:'Private prerequisite'}],
    edges:[{id:'edge',product_id:'p',from_feature_id:'f',to_feature_id:'hidden',status:'accepted',type:'requires',rationale:'Private edge rationale'}],
  };
  let graph = privateGraph, graphReads = 0, stateStatus = 200, delayedGraph;
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
    return json({}, 404);
  };
  const visible = () => document.querySelector('.owner-dependencies')?.textContent || '';
  const waitFor = async predicate => {
    for (let attempt = 0; attempt < 80; attempt++) {
      if (predicate()) return;
      await new Promise(resolve => setTimeout(resolve, 10));
    }
    throw new Error('Expected graph render did not occur');
  };
  const absent = () => {
    if (/Private prerequisite|Private edge rationale/.test(document.querySelector('#main').textContent)) throw new Error('Revoked graph details are still visible');
  };
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
    result.textContent = 'PASS: dependency graph revocation';
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
      response.end(`(${checkRevokedDependency.toString()})();`);
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
  assert.equal(outcome?.[1], 'PASS: dependency graph revocation');
  console.log(outcome[1]);
} finally {
  server.close();
  await rm(profile,{recursive:true,force:true});
}
