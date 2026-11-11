/**
 * Every string the UI asks for must exist, in both languages.
 *
 * Written because this failed twice for the same reason. Folding a view into
 * another one deletes its translation namespace, the components that moved
 * keep asking for the old keys, and react-i18next answers a missing key with
 * the key itself — so the app does not crash, does not warn, and renders
 * `canvas.inspector.refHelp` at the user in the middle of a panel. The second
 * time, it shipped and sat there through a release.
 *
 * That is exactly the failure a test should own: mechanical, invisible in
 * review, and caught for free by reading the source.
 *
 * Scope and its limits: this walks `t("literal")` calls, which is nearly all
 * of them. Computed keys (`t("converse." + mode)`) cannot be resolved
 * statically, so they are listed explicitly below — a short list that has to
 * be maintained by hand is better than a check that quietly skips them.
 */

import assert from "node:assert/strict";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, extname, join } from "node:path";
import { test } from "node:test";

const here = dirname(fileURLToPath(import.meta.url));
const SRC = join(here, "..", "src");

const en = JSON.parse(readFileSync(join(SRC, "i18n", "en.json"), "utf8")) as object;
const es = JSON.parse(readFileSync(join(SRC, "i18n", "es.json"), "utf8")) as object;

/** Keys built at runtime, which no static walk can see. */
const COMPUTED = [
  "converse.text",
  "converse.voice",
  // Sidebar labels: t(`nav.${id}`), one per ViewId.
  "nav.pulse",
  "nav.studio",
  "nav.sessions",
  "nav.evidence",
  "nav.expertise",
  "nav.projections",
  "nav.graphs",
  "nav.reference",
  // Connection indicator: t(`topbar.state.${state}`) / t(`topbar.help.${state}`).
  "topbar.state.ok",
  "topbar.state.unauthorized",
  "topbar.state.unknown",
  "topbar.state.down",
  "topbar.help.ok",
  "topbar.help.unauthorized",
  "topbar.help.unknown",
  "topbar.help.down",
  // Chosen by a ternary inside t(...), which the literal walk cannot see.
  "palette.themeLight",
  "palette.themeDark",
  "evidence.chainIntact",
  "evidence.chainBroken",
  "evidence.approved",
  "evidence.denied",
  "sessions.expand",
  "sessions.collapse",
  "studio.edge.holding",
  "studio.edge.gate",
  "token.needed",
  "token.have",
  "token.none",
  "pulse.approvalsOff",
  "pulse.approvalsWaiting",
  "pulse.approvalsNone",
  // t(`sessions.filters.${f}`), t(`expertise.types.${type}`), t(`studio.edge.qos.${qos}`).
  "sessions.filters.all",
  "sessions.filters.live",
  "sessions.filters.ended",
  "sessions.filters.errors",
  "expertise.types.sensorial",
  "expertise.types.cognitive",
  "expertise.types.motor",
  "expertise.types.memory",
  "expertise.types.logical",
  "studio.edge.qos.reliable",
  "studio.edge.qos.realtime",
  "studio.edge.qos.bulk",
  "graphs.origin.declared",
  "graphs.origin.planner",
  "reference.tab.commands",
  "reference.tab.contracts",
  "reference.tab.schemas",
  "reference.tab.enums",
  "studio.phase.idle",
  "studio.phase.planning",
  "studio.phase.executing",
  "studio.phase.done",
  "studio.phase.failed",
  "projections.mode.live",
  "projections.mode.dry-run",
  "projections.mode.disabled",
  "gate.approved",
  "gate.denied",
  "studio.keys.search",
  "studio.keys.undo",
  "studio.keys.redo",
  "studio.keys.copy",
  "studio.keys.paste",
  "studio.keys.duplicate",
  "studio.keys.selectAll",
  "studio.keys.copyIR",
  "studio.keys.pasteIR",
  "studio.keys.delete",
  "studio.keys.deselect",
  "studio.keys.boxSelect",
  "studio.keys.pan",
  "studio.keys.zoom",
  "studio.keys.quickAdd",
  "studio.keys.rename",
  "studio.keys.menu",
  "studio.keys.fit",
  "studio.keys.tidy",
  "studio.keys.note",
  "studio.keys.help",
  "studio.keys.disable",
  "studio.keys.frame",
  "studio.keys.history",
];

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) walk(path, out);
    else if ([".ts", ".tsx"].includes(extname(path))) out.push(path);
  }
  return out;
}

/** Every `t("some.key")` in the source, with the file it came from. */
function usedKeys(): Map<string, string> {
  const found = new Map<string, string>();
  for (const file of walk(SRC)) {
    const source = readFileSync(file, "utf8");
    // The trailing [,)] is what tells a whole key from the literal half of a
    // concatenation: `t("converse." + mode)` is a computed key, and reporting
    // "converse." as missing would be a false alarm — the kind that trains
    // people to add exclusions instead of translations.
    for (const m of source.matchAll(/\bt\(\s*"([a-zA-Z0-9_.]+)"\s*[,)]/g)) {
      if (!found.has(m[1])) found.set(m[1], file.slice(SRC.length + 1));
    }
  }
  return found;
}

function lookup(bundle: object, key: string): unknown {
  return key.split(".").reduce<unknown>(
    (node, part) => (node && typeof node === "object" ? (node as Record<string, unknown>)[part] : undefined),
    bundle,
  );
}

/** Every leaf path in a bundle, for the reverse check. */
function leaves(node: unknown, prefix = "", out: string[] = []): string[] {
  if (typeof node !== "object" || node === null) {
    out.push(prefix);
    return out;
  }
  for (const [k, v] of Object.entries(node)) leaves(v, prefix ? `${prefix}.${k}` : k, out);
  return out;
}

test("every key the UI asks for exists in English", () => {
  const missing: string[] = [];
  for (const [key, file] of usedKeys()) {
    if (typeof lookup(en, key) !== "string") missing.push(`${key}  (${file})`);
  }
  assert.deepEqual(missing, [], "react-i18next renders a missing key as itself, in front of the user");
});

test("every key the UI asks for exists in Spanish", () => {
  const missing: string[] = [];
  for (const [key, file] of usedKeys()) {
    if (typeof lookup(es, key) !== "string") missing.push(`${key}  (${file})`);
  }
  assert.deepEqual(missing, [], "an untranslated key falls back to the key, not to English");
});

test("keys built at runtime exist too", () => {
  for (const key of COMPUTED) {
    assert.equal(typeof lookup(en, key), "string", `en is missing ${key}`);
    assert.equal(typeof lookup(es, key), "string", `es is missing ${key}`);
  }
});

test("the two bundles have exactly the same shape", () => {
  const a = leaves(en).sort();
  const b = leaves(es).sort();
  assert.deepEqual(
    a.filter((k) => !b.includes(k)), [],
    "in English but not Spanish",
  );
  assert.deepEqual(
    b.filter((k) => !a.includes(k)), [],
    "in Spanish but not English",
  );
});

test("a plural form exists in both languages or in neither", () => {
  // `key_one` beside `key` is how a count reads as "1 event" instead of
  // "1 events". i18next falls back to the bare key for every other count, so
  // the bare key must exist wherever a `_one` does.
  for (const [name, bundle] of [["en", en], ["es", es]] as const) {
    for (const key of leaves(bundle)) {
      if (!key.endsWith("_one")) continue;
      const bare = key.slice(0, -"_one".length);
      assert.equal(typeof lookup(bundle, bare), "string", `${name}: ${key} has no ${bare}`);
    }
  }
});

test("the product word is Expertise, not skill, wherever a person reads it", () => {
  // The wire and the SDK still say `skill` (paths, manifest keys, directory
  // names); the UI's own prose must not. Allowed: a path or command in
  // backticks or with a slash, which names a real file or route.
  const offenders: string[] = [];
  for (const [name, bundle] of [["en", en], ["es", es]] as const) {
    for (const key of leaves(bundle)) {
      const value = String(lookup(bundle, key));
      const prose = value.replace(/`[^`]*`/g, "").replace(/\bskills\/[\w-]+/g, "");
      if (/\bskills?\b/i.test(prose)) offenders.push(`${name}:${key}`);
    }
  }
  assert.deepEqual(offenders, []);
});

test("no namespace is left behind with nothing using it", () => {
  // The other half of the same bug: a view is deleted, its strings are not,
  // and the bundle grows a section nobody can reach.
  const used = [...usedKeys().keys(), ...COMPUTED];
  const roots = new Set(Object.keys(en));
  for (const key of used) roots.delete(key.split(".")[0]);
  assert.deepEqual([...roots], [], "these namespaces are no longer referenced anywhere");
});
