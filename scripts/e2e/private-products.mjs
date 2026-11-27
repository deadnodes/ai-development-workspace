// Live private-product E2E against dev RCP.
// Usage: node scripts/e2e/private-products.mjs <creds.json(0600)>
// Two humans (member_a, member_b) each approve their own agents through the
// real device flow; agents then act through `release-control mcp` (stdio).
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
    await this.rpc("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "priv-e2e", version: "1" } });
    this.p.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }) + "\n");
  }
  async call(name, args) {
    const r = await this.rpc("tools/call", { name, arguments: args });
    const res = r.result || {};
    const text = (res.content || []).map((c) => c.text || "").join("\n") + (res.structuredContent ? JSON.stringify(res.structuredContent) : "") + (r.error ? r.error.message : "");
    return { error: !!(res.isError || r.error), text, data: res.structuredContent };
  }
  exec(action, productID, data = {}, extra = {}) { return this.call("execute", { action, actor: "priv-e2e", product_id: productID, data, ...extra }); }
  close() { this.p.stdin.end(); this.p.kill(); }
}

import { execFileSync } from "node:child_process";
const nf = (r) => r.error && /NOT_FOUND|not found/i.test(r.text) && !/FORBIDDEN/.test(r.text);
const ts = Date.now().toString(36);
const marker = `privfeat${ts}`;
try {
  const who = { a: "full_member_a", b: "full_member_b", c: "full_regular" };
  for (const [k, w] of Object.entries(who)) await login(`priv-${k}`, w);
  const whoami = execFileSync(BIN, ["whoami", "--url", URL, "--profile", "priv-b"], { encoding: "utf8" });
  const bUser = (whoami.match(/"user_id"\s*:\s*"([^"]+)"/) || [])[1];
  check("whoami prints B's user id", !!bUser, bUser || whoami.slice(0, 300));
  const ag = {}; for (const k of Object.keys(who)) { ag[k] = new Agent(`priv-${k}`); await ag[k].init(); }

  let r = await ag.a.exec("create_product", undefined, { name: `Private E2E ${ts}`, visibility: "private" });
  const P = r.data?.id || (r.text.match(/"id"\s*:\s*"([^"]+)"/) || [])[1];
  check("A creates private product", !r.error && !!P, r.error ? r.text.slice(0, 200) : P);
  r = await ag.a.exec("add_product_member", P, { user_id: bUser });
  check("A invites B", !r.error, r.error ? r.text.slice(0, 200) : "");
  r = await ag.a.exec("create_feature", P, { title: `${marker} private feature`, problem: `${marker} problem`, goal: `${marker} goal` });
  const F = r.data?.id || (r.text.match(/"id"\s*:\s*"([^"]+)"/) || [])[1];
  check("A creates feature in private product", !r.error && !!F, r.error ? r.text.slice(0, 200) : F);

  r = await ag.a.call("get_project_context", { product_id: P });
  check("A sees product as private with B as member", !r.error && /"visibility"\s*:\s*"private"/.test(r.text) && r.text.includes(bUser), r.error ? r.text.slice(0, 200) : "");

  for (const k of ["b", "c"]) {
    const sees = k === "b";
    const label = { b: "member B", c: "regular C", d: "admin D (not invited)" }[k];
    r = await ag[k].call("get_project_context", { product_id: P });
    check(`${label}: get_project_context ${sees ? "ok" : "NOT_FOUND"}`, sees ? !r.error : nf(r), r.text.slice(0, 160));
    r = await ag[k].call("get_state", {});
    check(`${label}: product ${sees ? "listed" : "absent"} in get_state`, !r.error && (sees ? r.text.includes(P) : !r.text.includes(P)), r.error ? r.text.slice(0, 160) : "");
    r = await ag[k].call("search_context", { product_id: P, query: marker });
    check(`${label}: search ${sees ? "finds feature" : "NOT_FOUND"}`, sees ? !r.error && r.text.includes(F) : nf(r) && !r.text.includes(marker), r.text.slice(0, 160));
    r = await ag[k].call("resume", { feature_id: F });
    check(`${label}: resume feature ${sees ? "ok" : "NOT_FOUND"}`, sees ? !r.error : nf(r), r.text.slice(0, 160));
    if (!sees) {
      r = await ag[k].exec("create_feature", P, { title: "intrude", problem: "x", goal: "x" });
      check(`${label}: cannot write into private product (NOT_FOUND)`, nf(r), r.text.slice(0, 160));
      r = await ag[k].exec("add_product_member", P, { user_id: "human/intruder" });
      check(`${label}: cannot invite itself (NOT_FOUND)`, nf(r), r.text.slice(0, 160));
    }
  }
  r = await ag.b.exec("add_product_member", P, { user_id: "human/intruder" });
  check("member B cannot manage members (not owner)", r.error, r.text.slice(0, 160));
  Object.values(ag).forEach((a) => a.close());
  console.log(`E2E product ${P} feature ${F}`);
} catch (e) {
  check("harness", false, String(e.message || e));
}
const failed = results.filter((x) => !x.ok).length;
console.log(`SUMMARY ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
