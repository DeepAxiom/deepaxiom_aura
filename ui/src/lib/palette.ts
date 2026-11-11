/**
 * Command palette matching.
 *
 * Subsequence matching with three preferences, in this order: a contiguous
 * run beats scattered letters, a match at the start of a word beats one in
 * the middle, and a shorter label beats a longer one. That is enough for
 * "ses" to find Sessions before "Expertise", and for "og" to still find
 * "Open graph".
 */

export interface Command {
  id: string;
  label: string;
  /** Group heading and extra searchable text. */
  group: string;
  hint?: string;
  keywords?: string;
}

/** Score of `query` against `text`; null when it does not match at all. */
export function score(query: string, text: string): number | null {
  const q = query.trim().toLowerCase();
  if (!q) return 0;
  const t = text.toLowerCase();
  let ti = 0;
  let total = 0;
  let run = 0;
  for (const ch of q) {
    if (ch === " ") { run = 0; continue; }
    const found = t.indexOf(ch, ti);
    if (found < 0) return null;
    const boundary = found === 0 || /[\s./:_-]/.test(t[found - 1]);
    run = found === ti && ti > 0 ? run + 1 : 0;
    total += 1 + run * 3 + (boundary ? 4 : 0) - Math.min(found - ti, 6) * 0.25;
    ti = found + 1;
  }
  return total - t.length * 0.01;
}

/** Commands matching `query`, best first. Ties keep the order they were given in. */
export function filterCommands<T extends Command>(commands: readonly T[], query: string, limit = 50): T[] {
  if (!query.trim()) return commands.slice(0, limit);
  const scored: { c: T; s: number; i: number }[] = [];
  commands.forEach((c, i) => {
    const label = score(query, c.label);
    const extra = score(query, `${c.group} ${c.keywords ?? ""} ${c.hint ?? ""}`);
    const s = Math.max(label ?? -Infinity, extra === null ? -Infinity : extra - 6);
    if (s > -Infinity) scored.push({ c, s, i });
  });
  return scored.sort((a, b) => b.s - a.s || a.i - b.i).slice(0, limit).map((x) => x.c);
}
