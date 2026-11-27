// Live E2E for stages 3-5 (owner questions, ideas, dependency graph, next_work) against dev RCP.
// Usage: node scripts/e2e/owner-work.mjs <creds.json(0600)>
// Human A (full_member_a) owns a shared product; A has two agents: an
// orchestrator and a chat agent that relays A's answers. Human B (full_member_b)
// has one agent. All agents act through `release-control mcp` (stdio).
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
const require = createRequire(process.env.PW_DIR || "/opt/homebrew/lib/node_modules/");
const { chromium } = require("playwright");

const URL = process.env.RCP_URL || "https://rcp-dev.tail9c31db.ts.net";
const BIN = process.env.RC_BIN;
const creds = JSON.parse(readFileSync(process.argv[2], "utf8"));
const results = [];
const check = (name, ok, detail = "") => { results.push({ name, ok, detail }); console.log(`${ok ? "PASS" : "FAIL"} ${name}${detail ? " — " + detail : ""}`); };

async function login(profile, who) {
  const proc = spawn(BIN, ["login", "--url", URL, "--profile", profile, "--name", profile], { stdio: ["ignore", "pipe", "pipe"] });
  let out = "";
  const verification = await new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error("no verification URL")), 30000);
    const on = (b) => { out += b; const m = out.match(/Open (\S+)/); if (m) { clearTimeout(t); resolve(m[1]); } };
    proc.stdout.on("data", on); proc.stderr.on("data", on);
  });
  const done = new Promise((resolve) => proc.on("exit", resolve));
  const browser = await chromium.launch();
  const page = await (await browser.newContext()).newPage();
  await page.goto(verification);
  await page.locator("#username").fill(creds[who].username);
  await page.locator("#password").fill(creds[who].password);
  try {
    await Promise.all([page.waitForURL(/\/auth\/device/, { timeout: 30000 }), page.locator("#kc-login").click()]);
  } catch (e) {
    const text = (await page.locator("body").innerText()).replace(/\s+/g, " ").slice(0, 400);
    throw new Error(`login ${who} stuck: ${text}`);
  }
  await page.getByRole("button", { name: "Approve this agent" }).click();
  await page.waitForLoadState("networkidle");
  await browser.close();
  const code = await done;
  if (code !== 0) throw new Error(`login ${profile} exited ${code}`);
}

class Agent {
  constructor(profile) {
    this.p = spawn(BIN, ["mcp", "--url", URL, "--profile", profile], { stdio: ["pipe", "pipe", "inherit"] });
    this.buf = ""; this.next = 1; this.wait = new Map();
    this.p.stdout.on("data", (b) => {
      this.buf += b;
      let i;
      while ((i = this.buf.indexOf("\n")) >= 0) {
        const line = this.buf.slice(0, i); this.buf = this.buf.slice(i + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.id && this.wait.has(msg.id)) { this.wait.get(msg.id)(msg); this.wait.delete(msg.id); }
      }
    });
  }
  rpc(method, params) {
    const id = this.next++;
    this.p.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
    return new Promise((r) => this.wait.set(id, r));
  }
  async init() {
    await this.rpc("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "owner-work-e2e", version: "1" } });
    this.p.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }) + "\n");
  }
  async call(name, args) {
    const r = await this.rpc("tools/call", { name, arguments: args });
    const res = r.result || {};
    const text = (res.content || []).map((c) => c.text || "").join("\n") + (res.structuredContent ? JSON.stringify(res.structuredContent) : "") + (r.error ? r.error.message : "");
    return { error: !!(res.isError || r.error), text, data: res.structuredContent };
  }
  exec(action, productID, data = {}, extra = {}) { return this.call("execute", { action, actor: "owner-work-e2e", product_id: productID, data, ...extra }); }
  close() { this.p.stdin.end(); this.p.kill(); }
}

import { execFileSync } from "node:child_process";
const idOf = (r) => r.data?.id || (r.text.match(/"id"\s*:\s*"([^"]+)"/) || [])[1];
const short = (r) => r.text.slice(0, 200);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const ts = Date.now().toString(36);
const harness = (s) => ({ kind: "codex", session_ref: s, cwd: "/tmp", host: "e2e" });
const answer = (q, option, verdict = "consistent", extra = {}) => ({ answers: [{ question_id: q, text: "E2E answer", option_id: option, validation: { verdict, note: "e2e" }, ...extra }] });
try {
  await login("ow-a", "full_member_a");
  await login("ow-chat", "full_member_a");
  await login("ow-b", "full_member_b");
  const userOf = (p) => (execFileSync(BIN, ["whoami", "--url", URL, "--profile", p], { encoding: "utf8" }).match(/"user_id"\s*:\s*"([^"]+)"/) || [])[1];
  const aUser = userOf("ow-a"), bUser = userOf("ow-b");
  check("whoami gives both user ids", !!aUser && !!bUser && aUser !== bUser);
  const [a, chat, b] = [new Agent("ow-a"), new Agent("ow-chat"), new Agent("ow-b")];
  await Promise.all([a.init(), chat.init(), b.init()]);

  let r = await a.exec("create_product", undefined, { name: `Owner work E2E ${ts}` });
  const P = idOf(r); check("A creates shared product", !r.error && !!P, r.error ? short(r) : P);
  r = await a.exec("create_feature", P, { title: `ow${ts} feature`, problem: "p", goal: "g" });
  const F = idOf(r); check("A creates feature", !r.error && !!F, short(r));
  r = await a.exec("create_integration", P, { title: "Slice", objective: "Ship" }, { feature_id: F });
  const I = idOf(r); check("A creates integration", !r.error && !!I, short(r));

  // Stage 3: blocking question on own claimed work.
  r = await a.call("claim_execution", { actor: "e2e", product_id: P, integration_id: I, data: { orchestrator_id: "orch-a", agent_id: "a", harness: harness("a-session") } });
  const fence = r.data?.fence ?? r.data?.execution?.fence ?? (r.text.match(/"fence"\s*:\s*(\d+)/) || [])[1];
  check("A claims its integration", !r.error && !!fence, short(r));
  r = await a.exec("ask_question", P, { title: "Scope?", body: "Stable clients only per constraint.", kind: "decision", options: [{ id: "stable", label: "Stable only", consequence: "Preview out" }, { id: "all", label: "All", consequence: "More work" }], recommendation: "stable", context_refs: [F], blocking: true }, { feature_id: F, integration_id: I });
  const Q = idOf(r); check("A asks a blocking question", !r.error && !!Q, short(r));
  r = await a.call("list_questions", { addressed_to: "me" });
  check("question in A's inbox", !r.error && r.text.includes(Q), short(r));
  r = await b.call("list_questions", { addressed_to: "me" });
  check("question absent from B's inbox", !r.error && !r.text.includes(Q), short(r));
  r = await b.exec("answer_questions", P, answer(Q, "stable"));
  check("B cannot answer A's question", r.error, short(r));
  r = await chat.exec("answer_questions", P, answer(Q, "all", "contradicts"));
  check("contradicting answer without override is rejected", r.error && /ANSWER_NEEDS_CLARIFICATION/.test(r.text), short(r));
  r = await chat.exec("answer_questions", P, answer(Q, "stable"));
  check("A's chat agent answers in a batch", !r.error, short(r));
  r = await a.call("get_question", { question_id: Q });
  check("question is answered", !r.error && /"status"\s*:\s*"answered"/.test(r.text), short(r));
  r = await a.call("resume", { feature_id: F });
  check("resume carries the decision memory", !r.error && /Scope\?/.test(r.text), short(r));
  let woke = false;
  for (let i = 0; i < 12 && !woke; i++) {
    r = await a.call("list_execution_events", { product_id: P, unresolved_only: true });
    woke = !r.error && r.text.includes(I);
    if (!woke) await sleep(10000);
  }
  check("answer raises a wake event for A's execution", woke, short(r));
  r = await a.call("release_execution", { actor: "e2e", product_id: P, integration_id: I, data: { fence: Number(fence), outcome: "paused", reason: "e2e" } });

  // Stage 4: ideas and dependency graph.
  r = await a.exec("create_feature", P, { title: `ow${ts} idea`, problem: "p", goal: "g", status: "idea" });
  const IDEA = idOf(r); check("A's agent creates an idea", !r.error && !!IDEA, short(r));
  r = await a.exec("update_feature", P, { status: "ready_for_execution" }, { feature_id: IDEA });
  check("agent cannot promote an idea directly", r.error && /OWNER_APPROVAL_REQUIRED/.test(r.text), short(r));
  r = await a.exec("request_idea_promotion", P, { rationale: "ready" }, { feature_id: IDEA });
  const QP = idOf(r); check("agent requests promotion", !r.error && !!QP, short(r));
  r = await a.exec("answer_questions", P, answer(QP, "approve"));
  check("requesting agent cannot approve its own request", r.error, short(r));
  r = await chat.exec("answer_questions", P, answer(QP, "approve"));
  check("owner's chat agent relays approval", !r.error, short(r));
  r = await a.call("resume", { feature_id: IDEA });
  check("idea is ready_for_execution", !r.error && /ready_for_execution/.test(r.text), short(r));
  r = await chat.exec("propose_dependency", P, { from_feature_id: IDEA, to_feature_id: F, type: "requires", rationale: "after F", accept: true });
  const D1 = idOf(r);
  r = await chat.call("get_dependency_graph", { product_id: P });
  check("dependency graph shows the edge", !r.error && r.text.includes(IDEA) && r.text.includes(F), short(r));
  r = await a.exec("propose_dependency", P, { from_feature_id: F, to_feature_id: IDEA, type: "requires", rationale: "cycle" });
  const D2 = idOf(r);
  r = D2 ? await chat.exec("decide_dependency", P, { dependency_id: D2, decision: "accept" }) : r;
  check("cycle is rejected", r.error && /DEPENDENCY_CYCLE|cycle/i.test(r.text), short(r));

  // Stage 5: next_work is per user.
  r = await b.exec("create_feature", P, { title: `ow${ts} b feature`, problem: "p", goal: "g" });
  const FB = idOf(r);
  r = await b.exec("create_integration", P, { title: "B slice", objective: "Ship" }, { feature_id: FB });
  const IB = idOf(r);
  r = await b.call("claim_execution", { actor: "e2e", product_id: P, integration_id: IB, data: { orchestrator_id: "orch-b", agent_id: "b", harness: harness("b-session") } });
  check("B claims its integration", !r.error, short(r));
  r = await a.call("next_work", { product_id: P });
  check("A's next_work excludes B's work", !r.error && !r.text.includes(IB), short(r));
  check("A's next_work offers the ready idea to plan", !r.error && r.text.includes(IDEA), short(r));
  r = await a.call("status_overview", { product_id: P });
  check("status_overview works", !r.error, short(r));
  r = await a.exec("register_user_orchestrator", P, { harness: { kind: "codex", tmux_target: "e2e:0.0", session_ref: "e2e-orch" } });
  check("A registers a user orchestrator", !r.error, short(r));
  await a.exec("unregister_user_orchestrator", P, {});
  [a, chat, b].forEach((x) => x.close());
  console.log(`E2E product ${P}`);
} catch (e) {
  check("harness", false, String(e.message || e));
}
const failed = results.filter((x) => !x.ok).length;
console.log(`SUMMARY ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
