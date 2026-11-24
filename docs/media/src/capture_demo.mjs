// Records one run of `aura demo` from start to finish: frames for the GIF and
// a still of the result in each language.
//
// Run from a directory where `playwright` is installed, like capture.mjs:
//
//   aura demo --listen 127.0.0.1:7700          # in another terminal; it waits
//   node <repo>/docs/media/src/capture_demo.mjs http://127.0.0.1:7700 <outdir>
//   bash <repo>/docs/media/src/make_demo_gif.sh <outdir> <repo>/docs/media
//
// The demo holds until somebody watches. This opens the page with ?hold so the
// browser is up and idle first, then presses space — nothing of the browser's
// own start-up is in the recording or in the run's numbers. It waits for the
// run to end by reading the page's own state, and adds nothing to it.
//
// Frames, not video. A screencast encoded in real time costs a core, on the
// machine whose latency is on the screen being recorded: the first attempt at
// this slowed the run it was filming several times over. One screenshot every
// EVERY_MS costs almost nothing, and at 12 fps the GIF plays 12*EVERY_MS/1000
// times faster than it happened — three times, by default. The clock on the
// screen shows the real elapsed time throughout.
//
// PW_CHANNEL=msedge (or chrome) uses an installed browser instead of the
// Chromium that `npx playwright install` downloads.
//
// Output: <outdir>/frames/<scene>-NNNNN.png and <outdir>/<scene>-<lang>.png.

import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";

const require = createRequire(path.join(process.cwd(), "noop.js"));
const { chromium } = require("playwright");

const [base, out] = process.argv.slice(2);
if (!base || !out) {
  console.error("usage: node capture_demo.mjs <demo-url> <outdir>");
  process.exit(2);
}
const lang = process.env.DEMO_LANG || "en";
// DEMO_SCENE names the files: "under-fire" for `aura demo`, "versus" for
// `aura demo --versus`. The page is whichever one the demo is serving.
const scene = process.env.DEMO_SCENE || "under-fire";
const every = Number(process.env.EVERY_MS || 250);
const size = { width: 1280, height: 800 };
const frames = path.join(out, "frames");
fs.rmSync(frames, { recursive: true, force: true });
fs.mkdirSync(frames, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const browser = await chromium.launch(process.env.PW_CHANNEL ? { channel: process.env.PW_CHANNEL } : {});
const context = await browser.newContext({ viewport: size, deviceScaleFactor: 1, colorScheme: "dark" });
const page = await context.newPage();
await page.goto(`${base}/?lang=${lang}&hold`);
await page.waitForFunction("state && state.phase === 'waiting'", null, { timeout: 60_000 });
await sleep(1000);

let n = 0;
const shoot = () => page.screenshot({ path: path.join(frames, `${scene}-${String(n++).padStart(5, "0")}.png`) });
await shoot();
await page.keyboard.press("Space");

// One frame per tick of a fixed clock, so a slow screenshot does not stretch
// the recording: the next one is taken when it is due, not a delay later.
const start = Date.now();
let over = 0;
for (let tick = 1; ; tick++) {
  const due = start + tick * every;
  if (due > Date.now()) await sleep(due - Date.now());
  await shoot();
  const phase = await page.evaluate("state && state.phase");
  if (phase === "done" || phase === "failed") {
    if (++over * every >= 4000) break; // hold the last frame long enough to read
  }
  if (Date.now() - start > 300_000) break;
}

// A beat held if the demo says it survived, or the comparison says the node held.
const result = await page.evaluate("({ phase: state.phase, elapsed: state.elapsed, " +
  "beats: state.beats.map(b => [b.id, b.status === 'survived' || (b.status === 'done' && b.held === true)]), " +
  "live: state.live, plain: state.plain, aura: state.aura, meta: state.meta })");
await page.screenshot({ path: path.join(out, `${scene}-${lang}.png`) });

// The same finished run in the other language: the page only re-renders.
const other = lang === "en" ? "es" : "en";
await page.goto(`${base}/?lang=${other}`);
await page.waitForFunction("state && state.phase === 'done'", null, { timeout: 15_000 }).catch(() => {});
await sleep(800);
await page.screenshot({ path: path.join(out, `${scene}-${other}.png`) });

await context.close();
await browser.close();

console.log(JSON.stringify({ frames: n, every_ms: every, ...result }, null, 2));
if (result.phase !== "done" || result.beats.some(([, held]) => !held)) {
  console.error("the node did not hold through every beat; do not publish this recording as if it had");
  process.exit(1);
}
