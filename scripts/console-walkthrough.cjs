#!/usr/bin/env node
// Drives the web console in a real Chromium against a running stack
// (default: `FAKE_READY_POLLS=3 FAKE_FAIL_IMAGE=broken make compose-up`):
// takes the README screenshots, checks live updates, reconnect and the
// four-eyes 403, and measures event latency to the browser.
//
//   node scripts/console-walkthrough.cjs        # needs `playwright` + Chromium
//
// Latency is measured per stream event as (browser receive time, Date.now()
// in the page) minus the event's occurredAt (set by the API process when
// the event is created, just before its transaction commits). Browser and
// API run on the same host, so they share a clock.
const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const API = process.env.API || "http://localhost:8080";
const SECRET = process.env.MODELDOCK_JWT_SECRET || "dev-only-secret-change-me-0123456789";
const BIN = process.env.MODELDOCK_BIN || "bin/modeldock";
const OUT = process.env.OUT || "docs/screenshots";
const MODEL = process.env.MODEL || `fraud-detector-${Date.now().toString(36).slice(-4)}`;
const COMPOSE = ["compose", "-f", "deploy/docker-compose.yml"];

const tok = (sub, role) =>
  execFileSync(BIN, ["token", "-sub", sub, "-role", role, "-ttl", "2h"], { env: { ...process.env, MODELDOCK_JWT_SECRET: SECRET } })
    .toString()
    .trim();
const T = { dana: tok("dana", "deployer"), alice: tok("alice", "approver"), erin: tok("erin", "admin"), vic: tok("vic", "viewer") };

async function call(who, method, p, body) {
  const res = await fetch(API + p, {
    method,
    headers: { Authorization: `Bearer ${T[who]}`, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(`${method} ${p}: ${res.status} ${JSON.stringify(json)}`);
  return json;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function waitStatus(id, want, secs = 60) {
  for (let i = 0; i < secs * 4; i++) {
    const d = await call("vic", "GET", `/api/v1/deployments/${id}`);
    if (d.status === want) return d;
    if (d.status === "failed" && want !== "failed") throw new Error(`deployment ${id} failed: ${d.reason}`);
    await sleep(250);
  }
  throw new Error(`timeout waiting for ${id} -> ${want}`);
}
const version = (v, tag, extra = {}) => ({
  version: v,
  image: `modeldock/iris-server:${tag}`,
  metadata: { framework: "sklearn", dataset: "transactions-2026q3", ...extra.metadata },
  resources: { cpuRequest: "100m", memoryRequest: "128Mi", memoryLimit: "256Mi", ...extra.resources },
  scaling: { minReplicas: 1, maxReplicas: 4, targetCPUPercentage: 70, ...extra.scaling },
});

async function signIn(page, who) {
  await page.goto(API + "/#/");
  await page.evaluate(() => sessionStorage.clear());
  await page.reload();
  await page.getByLabel("Paste a JWT").fill(T[who]);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.getByTestId("live-indicator").filter({ hasText: "live" }).waitFor();
}
const shot = (page, name) => page.screenshot({ path: path.join(OUT, `${name}.png`), fullPage: true });

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width: 1280, height: 860 } });
  const checks = [];
  const ok = (what) => {
    checks.push(what);
    console.log("ok -", what);
  };

  await page.goto(API + "/");
  await shot(page, "login");

  // Register the model before the timeline subscribes.
  await call("dana", "POST", "/api/v1/models", { name: MODEL, description: "Card fraud scoring (gradient boosting)" });
  await call("dana", "POST", `/api/v1/models/${MODEL}/versions`, version("1.0.0", "1.0.0", { metadata: { accuracy: "0.931" } }));
  await call("dana", "POST", `/api/v1/models/${MODEL}/versions`,
    version("1.1.0", "1.1.0", { metadata: { accuracy: "0.947", features: "v2" }, resources: { cpuRequest: "250m", memoryLimit: "512Mi" }, scaling: { minReplicas: 2, maxReplicas: 6 } }));
  await call("dana", "POST", `/api/v1/models/${MODEL}/versions`, version("1.2.0", "broken", { metadata: { accuracy: "0.952" } }));

  await signIn(page, "vic");
  await page.goto(`${API}/#/timeline?model=${MODEL}`);
  await page.getByRole("region", { name: "staging timeline" }).waitFor();

  // Latency probe: an EventSource in its own tab (sign-ins reload the main
  // tab) records the receive time of every event.
  const probe = await browser.newPage();
  await probe.goto(API + "/healthz");
  await probe.evaluate((token) => {
    window.__lat = [];
    const es = new EventSource(`/api/v1/events/stream?access_token=${token}`);
    const on = (e) => {
      const m = JSON.parse(e.data);
      window.__lat.push({ type: m.type, ms: Date.now() - Date.parse(m.occurredAt) });
    };
    ["deploy.requested", "deploy.progressing", "deploy.ready", "deploy.failed", "promote.started", "rollback.started",
      "approval.requested", "approval.approved", "approval.rejected"].forEach((t) => es.addEventListener(t, on));
  }, T.vic);
  await sleep(500);

  // 1.0.0 through staging; screenshot while it is progressing.
  const fetchesBefore = [];
  page.on("request", (r) => r.url().includes("/deployments") && fetchesBefore.push(r.url()));
  let d = await call("dana", "POST", `/api/v1/models/${MODEL}/versions/1.0.0/deploy`, { environment: "staging" });
  await page.getByTestId("current-staging").filter({ hasText: "progressing" }).waitFor({ timeout: 15000 });
  ok("timeline shows staging 1.0.0 progressing without reload");
  await waitStatus(d.id, "ready");
  await page.getByTestId("current-staging").filter({ hasText: "ready" }).waitFor({ timeout: 5000 });
  ok("staging badge flips to ready live");
  if (fetchesBefore.length !== 0) throw new Error(`timeline refetched during live updates: ${fetchesBefore}`);
  ok("no history refetch while updating live");

  let a = await call("dana", "POST", `/api/v1/models/${MODEL}/versions/1.0.0/promotion-requests`, { comment: "staging green for 30 min" });
  await call("alice", "POST", `/api/v1/approvals/${a.id}/approve`, { comment: "lgtm" });
  d = await call("dana", "POST", `/api/v1/models/${MODEL}/versions/1.0.0/promote`);
  await waitStatus(d.id, "ready");

  d = await call("dana", "POST", `/api/v1/models/${MODEL}/versions/1.1.0/deploy`, { environment: "staging" });
  await waitStatus(d.id, "ready");
  // erin (admin) requests the promotion, so erin may not approve it.
  a = await call("erin", "POST", `/api/v1/models/${MODEL}/versions/1.1.0/promotion-requests`, { comment: "v2 features, +1.6pt accuracy in staging" });

  // Promote 1.1.0 while watching the prod lane go pending -> progressing -> ready.
  await signIn(page, "alice");
  await page.goto(`${API}/#/approvals`);
  const card = page.getByRole("article", { name: `promotion ${MODEL} 1.1.0` });
  await card.getByRole("table").waitFor();
  await shot(page, "approval-queue");
  ok("approval queue shows the pending request with an inline diff against prod");

  await signIn(page, "erin");
  await page.goto(`${API}/#/approvals`);
  await card.getByRole("button", { name: "Approve" }).click();
  await page.getByRole("alert").filter({ hasText: "Four-eyes" }).waitFor();
  await shot(page, "approval-four-eyes-403");
  ok("requester (admin) gets the four-eyes 403 explained");

  await signIn(page, "alice");
  await page.goto(`${API}/#/approvals`);
  await card.getByLabel("comment").fill("approved, watch p99 after rollout");
  await card.getByRole("button", { name: "Approve" }).click();
  await card.waitFor({ state: "detached" });
  ok("approver approves from the queue");

  await page.goto(`${API}/#/timeline?model=${MODEL}`);
  await page.getByRole("region", { name: "prod timeline" }).waitFor();
  d = await call("dana", "POST", `/api/v1/models/${MODEL}/versions/1.1.0/promote`);
  await page.getByTestId("current-prod").filter({ hasText: "1.1.0" }).filter({ hasText: "progressing" }).waitFor({ timeout: 15000 });
  await shot(page, "timeline-live-progressing");
  await waitStatus(d.id, "ready");
  await page.getByTestId("current-prod").filter({ hasText: "ready" }).waitFor({ timeout: 5000 });
  ok("prod promotion shown live: pending/progressing -> ready");

  // Broken 1.2.0 -> fails -> auto-rollback to 1.1.0 in staging; then a prod rollback.
  d = await call("dana", "POST", `/api/v1/models/${MODEL}/versions/1.2.0/deploy`, { environment: "staging" });
  await waitStatus(d.id, "failed");
  await page.getByRole("region", { name: "staging timeline" }).getByText("auto rollback").waitFor({ timeout: 15000 });
  ok("failed rollout and auto-rollback appear live");
  d = await call("dana", "POST", `/api/v1/models/${MODEL}/rollback`, { environment: "prod" });
  await waitStatus(d.id, "ready");
  await page.getByTestId("current-prod").filter({ hasText: "1.0.0" }).filter({ hasText: "ready" }).waitFor({ timeout: 5000 });
  await sleep(1800); // let the highlight fade
  await shot(page, "timeline");
  ok("manual prod rollback appears live");

  // Stop measuring before the restart: replayed events would include downtime.
  const lat = await probe.evaluate(() => window.__lat);
  await probe.close();

  // Reconnect: restart the API container; indicator goes reconnecting -> live.
  execFileSync("docker", [...COMPOSE, "restart", "api"], { stdio: "ignore" });
  await page.getByTestId("live-indicator").filter({ hasText: "reconnecting" }).waitFor({ timeout: 15000 });
  await shot(page, "reconnecting");
  await page.getByTestId("live-indicator").filter({ hasText: "live" }).waitFor({ timeout: 30000 });
  ok("indicator shows reconnecting during an API restart, then live again");
  // An event after the reconnect still reaches the open page.
  const nextVersion = version("1.3.0", "1.3.0");
  await call("dana", "POST", `/api/v1/models/${MODEL}/versions`, nextVersion);
  d = await call("dana", "POST", `/api/v1/models/${MODEL}/versions/1.3.0/deploy`, { environment: "staging" });
  await page.getByTestId("current-staging").filter({ hasText: "1.3.0" }).waitFor({ timeout: 15000 });
  await waitStatus(d.id, "ready");
  ok("live updates continue after the reconnect");

  await page.goto(`${API}/#/diff?model=${MODEL}&from=1.0.0&to=1.1.0`);
  await page.getByRole("table", { name: "diff 1.0.0 to 1.1.0" }).waitFor();
  await shot(page, "version-diff");
  ok("version diff page renders the field-level diff");

  await page.goto(`${API}/#/models/${MODEL}`);
  await page.getByRole("heading", { name: MODEL }).waitFor();
  await page.getByText("Recent revisions").waitFor();
  await shot(page, "model-graphql");
  ok("model page loads over GraphQL");

  await browser.close();
  const ms = lat.map((x) => x.ms).sort((x, y) => x - y);
  const q = (p) => ms[Math.min(ms.length - 1, Math.floor(p * ms.length))];
  console.log(JSON.stringify({ model: MODEL, checks: checks.length, latencyMs: { n: ms.length, min: ms[0], p50: q(0.5), p95: q(0.95), max: ms[ms.length - 1] } }, null, 2));
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
