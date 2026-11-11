// Captures the UI of a running node into PNGs and scene videos.
//
// Run from a directory where `playwright` is installed (it is deliberately not
// a dependency of ui/):
//
//   mkdir cap && cd cap && npm init -y && npm i playwright && npx playwright install chromium
//   node <repo>/docs/media/src/capture.mjs http://localhost:9480 <outdir> [png|video|all]
//
// Needs: the node up, `demo_expertise.py --replicas 3` connected and
// `drive.py register` done. Traffic is produced by spawning drive.py and by
// typing into the UI; nothing on screen is injected into the page.
//
// PNGs land in <outdir> as <name>-<theme>.png. Videos land in <outdir>/video
// with scenes.json giving the offset and length of each scene, which
// make_gifs.sh turns into GIFs.

import { spawn } from "node:child_process";
import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(path.join(process.cwd(), "noop.js"));
const { chromium } = require("playwright");

const [base, out, what = "all"] = process.argv.slice(2);
const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, "..", "..", "..");
const port = new URL(base).port;
fs.mkdirSync(path.join(out, "video"), { recursive: true });

const drive = (...args) =>
  spawn("python", [path.join(here, "drive.py"), ...args], { cwd: repo, env: { ...process.env, AURA_PORT: port }, stdio: "ignore" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const PROMPT = "What happened with order 48213?";

const browser = await chromium.launch();
const problems = [];

async function open(theme, size, video) {
  const ctx = await browser.newContext({
    viewport: size, colorScheme: theme, deviceScaleFactor: 1,
    ...(video ? { recordVideo: { dir: path.join(out, "video"), size } } : {}),
  });
  const page = await ctx.newPage();
  page.on("console", (m) => { if (m.type() === "error") problems.push(m.text()); });
  page.on("pageerror", (e) => problems.push(e.message));
  return { ctx, page, t0: Date.now() };
}

async function talk(page) {
  await page.goto(`${base}/#/studio/notice`);
  await page.waitForSelector(".gnode");
  await page.locator(".studio__tab").nth(1).click();
  await page.waitForTimeout(1200);
  await page.locator(".converse__bar .input").fill(PROMPT);
  await page.keyboard.press("Enter");
}

/* ── stills ─────────────────────────────────────────────────────── */

async function stills(theme) {
  const { ctx, page } = await open(theme, { width: 1600, height: 1000 }, false);
  const shot = (name) => page.screenshot({ path: path.join(out, `${name}-${theme}.png`) });

  // Studio: mid-stream, then held at the gate.
  await talk(page);
  await page.waitForTimeout(1900);
  await shot("studio-live");
  await page.waitForSelector(".gate-card", { timeout: 20000 });
  await page.waitForTimeout(700);
  await shot("studio-gate");
  await page.locator(".gate-card .btn--primary").click();
  await page.waitForTimeout(1500);

  // The same session as a causal waterfall, on the sealed delivery.
  await page.locator(".watch").click();
  await page.waitForSelector(".wf__row");
  await page.locator(".wf__row:has(.wf__seal)").first().click();
  await page.waitForTimeout(600);
  await shot("session-waterfall");

  // Evidence: the newest entry open.
  await page.locator(".detail .receipt").click();
  await page.waitForSelector(".row-detail");
  await page.waitForTimeout(800);
  await shot("evidence-ledger");

  // Catalog, one Expertise open.
  await page.goto(`${base}/#/expertise/${encodeURIComponent("demo/cognitive/draft")}`);
  await page.waitForSelector(".xcard--open");
  await page.waitForTimeout(1500);
  await shot("expertise-catalog");

  // Pulse under load.
  await page.goto(`${base}/#/pulse`);
  const load = drive("run", "--graph", "draft-only", "--sessions", "14", "--turns", "3", "--ramp", "5", "--hold", "3", "--timeout", "90");
  await page.waitForTimeout(11000);
  await shot("pulse");
  await new Promise((r) => load.on("exit", r));

  // Approvals inbox: a gate raised through /mcp, answered from the UI.
  if (theme === "dark") {
    const call = drive("mcp");
    await page.goto(`${base}/#/evidence/approvals`);
    await page.waitForSelector(".inbox__card", { timeout: 15000 });
    await page.waitForTimeout(500);
    await shot("approvals-inbox");
    await page.locator(".inbox__card .btn--danger").click();
    await new Promise((r) => call.on("exit", r));
  }
  await ctx.close();
}

/* ── scenes ─────────────────────────────────────────────────────── */

const SIZE = { width: 1200, height: 750 };
const scenes = [];

async function scene(name, run) {
  const { ctx, page, t0 } = await open("dark", SIZE, true);
  const mark = { name };
  const begin = () => { mark.start = (Date.now() - t0) / 1000; };
  const end = () => { mark.length = (Date.now() - t0) / 1000 - mark.start; };
  await run(page, begin, end);
  const video = page.video();
  await ctx.close();
  const file = path.join(out, "video", `${name}.webm`);
  fs.renameSync(await video.path(), file);
  scenes.push({ ...mark, file: `${name}.webm` });
}

async function videos() {
  // 1. A graph running live: tokens stream on the node, edges carry envelopes,
  //    the kernel holds the motor delivery, a person approves, it is sealed.
  await scene("studio-live", async (page, begin, end) => {
    await page.goto(`${base}/#/studio/notice`);
    await page.waitForSelector(".gnode");
    // Fold the palette away (a toolbar button) so the graph gets the width.
    await page.locator(".studio__tools .iconbtn[aria-pressed]").last().click();
    await page.locator(".studio__tab").nth(1).click();
    await page.waitForTimeout(1500);
    begin();
    await page.locator(".converse__bar .input").pressSequentially(PROMPT, { delay: 28 });
    await page.keyboard.press("Enter");
    await page.waitForSelector(".gate-card", { timeout: 20000 });
    await page.waitForTimeout(1800);
    await page.locator(".gate-card .btn--primary").hover();
    await page.waitForTimeout(500);
    await page.locator(".gate-card .btn--primary").click();
    await page.waitForTimeout(2600);
    end();
  });

  // 2. Pulse while sessions ramp up across three replicas and drain again.
  await scene("pulse-load", async (page, begin, end) => {
    await page.goto(`${base}/#/pulse`);
    await page.waitForTimeout(3000);
    begin();
    const load = drive("run", "--graph", "draft-only", "--sessions", "15", "--turns", "2", "--ramp", "5", "--hold", "1", "--timeout", "60");
    await page.waitForTimeout(13500);
    end();
    await new Promise((r) => load.on("exit", r));
  });

  // 3. Opening a session and walking its causal waterfall to the ledger entry.
  await scene("session-waterfall", async (page, begin, end) => {
    await page.goto(`${base}/#/sessions`);
    await page.waitForSelector(".slist__row");
    await page.locator(".split__tools input").fill("notice");
    await page.waitForTimeout(1200);
    begin();
    const row = page.locator(".slist__row").first();
    await row.hover();
    await page.waitForTimeout(500);
    await row.click();
    await page.waitForSelector(".wf__row");
    await page.waitForTimeout(1500);
    const stream = page.locator(".wf__row", { has: page.locator(".wf__count") }).first();
    await stream.click();
    await page.waitForTimeout(1800);
    const gate = page.locator(".wf__row--gate").first();
    await gate.click();
    await page.waitForTimeout(1500);
    const sealed = page.locator(".wf__row:has(.wf__seal)").first();
    await sealed.click();
    await page.waitForTimeout(1800);
    await page.locator(".detail .receipt").hover();
    await page.waitForTimeout(500);
    await page.locator(".detail .receipt").click();
    await page.waitForSelector(".row-detail");
    await page.waitForTimeout(2500);
    end();
  });

  // 4. The command palette, then building a graph: quick-add two Expertise,
  //    wire them by dragging ports, lay it out.
  await scene("palette-build", async (page, begin, end) => {
    await page.goto(`${base}/#/pulse`);
    await page.waitForTimeout(2500);
    begin();
    await page.keyboard.press("Control+k");
    await page.waitForSelector(".cmdk");
    await page.waitForTimeout(500);
    await page.keyboard.type("new graph", { delay: 55 });
    await page.waitForTimeout(600);
    await page.keyboard.press("Enter");
    await page.waitForSelector(".canvas-surface");
    await page.waitForTimeout(900);
    const box = await page.locator(".canvas-surface").boundingBox();
    const add = async (fx, fy, text) => {
      await page.mouse.dblclick(box.x + box.width * fx, box.y + box.height * fy);
      await page.waitForSelector(".quickadd");
      await page.keyboard.type(text, { delay: 55 });
      await page.waitForTimeout(450);
      await page.keyboard.press("Enter");
      await page.waitForTimeout(600);
    };
    await add(0.5, 0.35, "draft");
    await add(0.8, 0.6, "redact");
    const wire = async (from, to) => {
      const a = await page.locator(from).first().boundingBox();
      const b = await page.locator(to).first().boundingBox();
      await page.mouse.move(a.x + 5, a.y + 5);
      await page.mouse.down();
      await page.mouse.move(b.x + 5, b.y + 5, { steps: 22 });
      await page.mouse.up();
      await page.waitForTimeout(450);
    };
    await wire(".gnode--client .gport--out .gport__dot", ".gnode--cognitive .gport--in .gport__dot");
    await wire(".gnode--cognitive .gport--out .gport__dot >> nth=1", ".gnode--logical .gport--in .gport__dot");
    await wire(".gnode--logical .gport--out .gport__dot", ".gnode--client .gport--in .gport__dot");
    await page.locator(".canvas-surface").click({ position: { x: 24, y: 24 } });
    await page.keyboard.press("t");
    await page.waitForTimeout(500);
    await page.keyboard.press("f");
    await page.waitForTimeout(2200);
    end();
  });

  fs.writeFileSync(path.join(out, "video", "scenes.json"), JSON.stringify(scenes, null, 2));
}

if (what === "png" || what === "all") { await stills("dark"); await stills("light"); }
if (what === "video" || what === "all") await videos();
await browser.close();
console.log(problems.length ? "console errors:\n" + [...new Set(problems)].join("\n") : "console clean");
