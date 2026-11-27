// Live per-user isolation E2E against dev RCP.
// Usage: node scripts/e2e/user-isolation.mjs <ids.json> <creds.json(0600)>
// Two humans (member_a, member_b) each approve their own agents through the
// real device flow; agents then act through `release-control mcp` (stdio).
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
const require = createRequire(process.env.PW_DIR || "/opt/homebrew/lib/node_modules/");
const { chromium } = require("playwright");

const URL = process.env.RCP_URL || "https://rcp-dev.tail9c31db.ts.net";
const BIN = process.env.RC_BIN;
const ids = JSON.parse(readFileSync(process.argv[2], "utf8"));
const creds = JSON.parse(readFileSync(process.argv[3], "utf8"));
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
  await Promise.all([page.waitForURL(/\/auth\/device/, { timeout: 30000 }), page.locator("#kc-login").click()]);
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
    await this.rpc("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "iso-e2e", version: "1" } });
    this.p.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }) + "\n");
  }
  async call(name, args) {
    const r = await this.rpc("tools/call", { name, arguments: args });
    const res = r.result || {};
    const text = (res.content || []).map((c) => c.text || "").join("\n") + (res.structuredContent ? JSON.stringify(res.structuredContent) : "") + (r.error ? r.error.message : "");
    return { error: !!(res.isError || r.error), text, data: res.structuredContent };
  }
  exec(action, integration, data = {}, extra = {}) {
    const named = ["claim_execution", "heartbeat_execution", "report_execution", "release_execution", "takeover_execution"];
    if (named.includes(action)) return this.call(action, { actor: "e2e", product_id: ids.product_id, integration_id: integration, data });
    return this.call("execute", { action, actor: "e2e", product_id: ids.product_id, integration_id: integration, data, ...extra });
  }
  close() { this.p.stdin.end(); this.p.kill(); }
}

const harness = (s) => ({ kind: "codex", session_ref: s, cwd: "/tmp", host: "mini" });
try {
  await login("iso-a1", "full_member_a");
  await login("iso-a2", "full_member_a");
  await login("iso-b1", "full_member_b");
  const [a1, a2, b1] = [new Agent("iso-a1"), new Agent("iso-a2"), new Agent("iso-b1")];
  await Promise.all([a1.init(), a2.init(), b1.init()]);

  let r = await a1.exec("claim_execution", ids.i1, { orchestrator_id: "orch-a", agent_id: "a1", harness: harness("a1") });
  check("A claims unassigned I1", !r.error, r.error ? r.text : "");
  const fence = r.data?.fence ?? r.data?.execution?.fence ?? JSON.parse(r.text || "{}").fence;
  let ctx = await a1.call("get_integration_context", { integration_id: ids.i1 });
  check("I1 is assigned to A's human", /"assignee"\s*:\s*"human\//.test(ctx.text), "");
  const assigneeA = (ctx.text.match(/"assignee"\s*:\s*"([^"]+)"/) || [])[1];

  for (const [action, data] of [
    ["claim_execution", { orchestrator_id: "orch-b", agent_id: "b1", harness: harness("b1") }],
    ["takeover_execution", { orchestrator_id: "orch-b", agent_id: "b1", harness: harness("b1"), fence, state_check_reference: "e2e" }],
    ["heartbeat_execution", { fence }],
    ["record_progress", { title: "foreign write", completed: ["x"] }],
  ]) {
    const extra = action === "record_progress" ? { feature_id: ids.feature_id } : {};
    r = await b1.exec(action, ids.i1, data, extra);
    check(`B cannot ${action} on A's I1`, r.error && /NOT_ASSIGNEE/.test(r.text), r.text.slice(0, 160));
  }

  r = await a2.exec("heartbeat_execution", ids.i1, { fence });
  check("A's second agent continues I1 with the fence", !r.error, r.error ? r.text.slice(0, 160) : "");

  r = await b1.exec("claim_execution", ids.i2, { orchestrator_id: "orch-b", agent_id: "b1", harness: harness("b1") });
  check("B claims unassigned I2", !r.error, r.error ? r.text.slice(0, 160) : "");
  ctx = await b1.call("get_integration_context", { integration_id: ids.i2 });
  const assigneeB = (ctx.text.match(/"assignee"\s*:\s*"([^"]+)"/) || [])[1];
  check("I2 assigned to B, different human from A", !!assigneeB && !!assigneeA && assigneeA !== assigneeB);

  r = await a1.exec("claim_execution", ids.i2, { orchestrator_id: "orch-a", agent_id: "a1", harness: harness("a1") });
  check("A cannot claim B's I2", r.error && /NOT_ASSIGNEE/.test(r.text), r.text.slice(0, 160));

  for (const [ag, i, f] of [[a1, ids.i1, fence]]) {
    r = await ag.exec("release_execution", i, { fence: f, outcome: "paused", reason: "e2e done" });
  }
  [a1, a2, b1].forEach((a) => a.close());
} catch (e) {
  check("harness", false, String(e.message || e));
}
const failed = results.filter((x) => !x.ok).length;
console.log(`SUMMARY ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
