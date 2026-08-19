/**
 * A small Markdown parser — block structure and inline spans, nothing else.
 *
 * Pure and React-free on purpose: parsing is the part with edge cases worth
 * testing (a pipe inside a code span, a fence that never closes, a list
 * interrupted by a heading), and a parser that returns data instead of JSX can
 * be asserted on directly. `Markdown.tsx` turns these nodes into elements and
 * contains no parsing at all.
 *
 * The subset is exactly what the specs in `spec/` and the guide's tables use:
 * ATX headings, fenced code, unordered and ordered lists, tables, blockquotes,
 * horizontal rules, paragraphs — and inline code, bold, italic, strikethrough
 * and links. Anything outside that renders as literal text rather than being
 * silently dropped, because these documents are normative and a renderer that
 * eats a rule it did not recognise is worse than one that shows it raw.
 *
 * No dependency: a Markdown library is 30–100 kB, and this bundle is compiled
 * into the kernel binary.
 */

export type Inline =
  | { t: "text"; v: string }
  | { t: "code"; v: string }
  | { t: "strong"; v: Inline[] }
  | { t: "em"; v: Inline[] }
  | { t: "del"; v: Inline[] }
  | { t: "link"; v: Inline[]; href: string };

export type Block =
  | { t: "heading"; level: number; v: Inline[]; slug: string }
  | { t: "para"; v: Inline[] }
  | { t: "code"; lang: string; v: string }
  | { t: "list"; ordered: boolean; items: Inline[][] }
  | { t: "table"; head: Inline[][]; rows: Inline[][][] }
  | { t: "quote"; v: Block[] }
  | { t: "hr" };

/* ── inline ──────────────────────────────────────────────────────── */

/**
 * Inline spans, scanned left to right.
 *
 * Code spans are resolved first and never re-scanned, which is what keeps
 * `**not bold**` inside backticks literal — the common case in these documents,
 * where prose about syntax is everywhere.
 */
export function parseInline(src: string): Inline[] {
  const out: Inline[] = [];
  let text = "";

  const flush = () => {
    if (text) out.push({ t: "text", v: text });
    text = "";
  };

  let i = 0;
  while (i < src.length) {
    const rest = src.slice(i);

    // `code` — greedy to the next backtick run of the same length
    if (src[i] === "`") {
      let ticks = 0;
      while (src[i + ticks] === "`") ticks++;
      const fence = "`".repeat(ticks);
      const end = src.indexOf(fence, i + ticks);
      if (end !== -1) {
        flush();
        out.push({ t: "code", v: src.slice(i + ticks, end) });
        i = end + ticks;
        continue;
      }
    }

    // [label](href)
    const link = /^\[([^\]]*)\]\(([^)\s]*)(?:\s+"[^"]*")?\)/.exec(rest);
    if (link) {
      flush();
      out.push({ t: "link", v: parseInline(link[1]), href: link[2] });
      i += link[0].length;
      continue;
    }

    // Emphasis. Longest marker first, so `**` is never read as two `*`.
    const span = matchEmphasis(src, i, rest);
    if (span) {
      flush();
      out.push({ t: span.kind, v: parseInline(span.inner) } as Inline);
      i = span.next;
      continue;
    }

    text += src[i];
    i++;
  }
  flush();
  return out;
}

const EMPHASIS = [
  ["**", "strong"],
  ["~~", "del"],
  ["*", "em"],
  ["_", "em"],
] as const;

/**
 * Match an emphasis span opening at `i`, or return null.
 *
 * Longest marker first so `**bold**` is never mistaken for two `*` spans, and
 * an empty pair (`**` immediately closed) is rejected so a literal `****` stays
 * literal instead of becoming an empty element.
 */
function matchEmphasis(
  src: string,
  i: number,
  rest: string,
): { kind: "strong" | "em" | "del"; inner: string; next: number } | null {
  for (const [marker, kind] of EMPHASIS) {
    if (!rest.startsWith(marker)) continue;
    const end = src.indexOf(marker, i + marker.length);
    if (end === -1) continue;
    const inner = src.slice(i + marker.length, end);
    if (!inner) continue;
    return { kind, inner, next: end + marker.length };
  }
  return null;
}

/* ── blocks ──────────────────────────────────────────────────────── */

/**
 * GitHub-compatible heading slug, so an anchor written in `spec/` resolves to
 * the same id here as it does on GitHub.
 *
 * Each whitespace character becomes its own hyphen rather than a run
 * collapsing into one — GitHub does not collapse, so a heading like
 * "`aura up` — start a node" slugs to `aura-up--start-a-node` with the double
 * hyphen the removed em dash leaves behind. Collapsing would silently break
 * every cross-reference that contains punctuation.
 */
export function slugify(s: string): string {
  return s
    .toLowerCase()
    .replace(/[`*_~\[\]()]/g, "")
    .replace(/[^a-z0-9\s-]/g, "")
    .trim()
    .replace(/\s/g, "-");
}

/** Split a table row on unescaped pipes, ignoring pipes inside code spans. */
function tableCells(line: string): string[] {
  const cells: string[] = [];
  let cur = "";
  let inCode = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (c === "\\" && line[i + 1] === "|") {
      cur += "|";
      i++;
      continue;
    }
    if (c === "`") inCode = !inCode;
    if (c === "|" && !inCode) {
      cells.push(cur);
      cur = "";
      continue;
    }
    cur += c;
  }
  cells.push(cur);
  // A leading and trailing pipe produce empty edge cells; drop only those.
  if (cells.length && cells[0].trim() === "") cells.shift();
  if (cells.length && cells[cells.length - 1].trim() === "") cells.pop();
  return cells.map((c) => c.trim());
}

const isDivider = (line: string) =>
  /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(line) && line.includes("-");

export function parse(src: string): Block[] {
  const lines = src.replace(/\r\n/g, "\n").split("\n");
  const out: Block[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    if (!line.trim()) {
      i++;
      continue;
    }

    // fenced code — an unclosed fence runs to the end rather than swallowing
    // the document into a parse error
    const fence = /^\s*(```|~~~)\s*(\S*)/.exec(line);
    if (fence) {
      const marker = fence[1];
      const lang = fence[2] ?? "";
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trimStart().startsWith(marker)) {
        body.push(lines[i]);
        i++;
      }
      const closed = i < lines.length;
      i++; // step past the closing fence (or past the end)
      // An unclosed fence ran to EOF, so its last line is whatever the file
      // ended with — usually a trailing newline. Keep blank lines a closed
      // fence deliberately contains; drop the ones EOF contributed.
      if (!closed) {
        while (body.length && !body[body.length - 1].trim()) body.pop();
      }
      out.push({ t: "code", lang, v: body.join("\n") });
      continue;
    }

    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    if (heading) {
      const raw = heading[2].replace(/\s+#+\s*$/, "");
      out.push({
        t: "heading",
        level: heading[1].length,
        v: parseInline(raw),
        slug: slugify(raw),
      });
      i++;
      continue;
    }

    if (/^\s*([-*_])\s*\1\s*\1[\s\-*_]*$/.test(line)) {
      out.push({ t: "hr" });
      i++;
      continue;
    }

    // table: a header row followed by a |---|---| divider
    if (line.includes("|") && i + 1 < lines.length && isDivider(lines[i + 1])) {
      const head = tableCells(line).map(parseInline);
      i += 2;
      const rows: Inline[][][] = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim()) {
        rows.push(tableCells(lines[i]).map(parseInline));
        i++;
      }
      out.push({ t: "table", head, rows });
      continue;
    }

    if (/^\s*>/.test(line)) {
      const body: string[] = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) {
        body.push(lines[i].replace(/^\s*>\s?/, ""));
        i++;
      }
      out.push({ t: "quote", v: parse(body.join("\n")) });
      continue;
    }

    const bullet = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/.exec(line);
    if (bullet) {
      const ordered = /\d/.test(bullet[2]);
      const items: Inline[][] = [];
      while (i < lines.length) {
        const m = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/.exec(lines[i]);
        if (!m || /\d/.test(m[2]) !== ordered) break;
        let content = m[3];
        i++;
        // continuation lines: indented, and not the start of the next item
        while (
          i < lines.length &&
          lines[i].trim() &&
          /^\s+/.test(lines[i]) &&
          !/^(\s*)([-*+]|\d+[.)])\s+/.test(lines[i])
        ) {
          content += " " + lines[i].trim();
          i++;
        }
        items.push(parseInline(content));
      }
      out.push({ t: "list", ordered, items });
      continue;
    }

    // paragraph — runs to a blank line or the start of another block
    const body: string[] = [];
    while (i < lines.length && lines[i].trim()) {
      const l = lines[i];
      if (
        /^(#{1,6})\s/.test(l) ||
        /^\s*(```|~~~)/.test(l) ||
        /^\s*>/.test(l) ||
        /^(\s*)([-*+]|\d+[.)])\s+/.test(l) ||
        (l.includes("|") && i + 1 < lines.length && isDivider(lines[i + 1]))
      ) {
        break;
      }
      body.push(l.trim());
      i++;
    }
    if (body.length) out.push({ t: "para", v: parseInline(body.join(" ")) });
  }

  return out;
}

/** Headings, for a table of contents. */
export function outline(blocks: Block[]): { level: number; text: string; slug: string }[] {
  const flat = (v: Inline[]): string =>
    v
      .map((n) =>
        n.t === "text" || n.t === "code" ? n.v : "v" in n && Array.isArray(n.v) ? flat(n.v) : "",
      )
      .join("");
  return blocks
    .filter((b): b is Extract<Block, { t: "heading" }> => b.t === "heading")
    .map((b) => ({ level: b.level, text: flat(b.v), slug: b.slug }));
}
