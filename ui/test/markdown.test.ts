/**
 * The Markdown parser, against the shapes the specs actually contain.
 *
 * Every case here is a construct taken from `spec/c*.md` or the guide's command
 * tables — the table cell whose command contains an escaped pipe, the prose
 * about `**` written inside backticks, the fence that documents JSON. A parser
 * that handles generic Markdown but mangles these renders the contracts wrong,
 * which is the only thing this parser exists to avoid.
 */

import assert from "node:assert/strict";
import { test } from "node:test";

import { outline, parse, parseInline, slugify } from "../src/markdown/parse.ts";

const text = (s: string) => ({ t: "text", v: s });

test("inline: code spans win over emphasis inside them", () => {
  // `gate: "none"` and prose about ** appear all over the specs.
  assert.deepEqual(parseInline("`**not bold**`"), [{ t: "code", v: "**not bold**" }]);
});

test("inline: bold, italic, strikethrough and links", () => {
  assert.deepEqual(parseInline("**a**"), [{ t: "strong", v: [text("a")] }]);
  assert.deepEqual(parseInline("*a*"), [{ t: "em", v: [text("a")] }]);
  assert.deepEqual(parseInline("~~a~~"), [{ t: "del", v: [text("a")] }]);
  assert.deepEqual(parseInline("[l](https://x)"), [
    { t: "link", v: [text("l")], href: "https://x" },
  ]);
});

test("inline: ** is not read as two * spans", () => {
  const [node] = parseInline("**bold**");
  assert.equal(node.t, "strong");
});

test("inline: an unclosed marker stays literal", () => {
  assert.deepEqual(parseInline("2 * 3 = 6"), [text("2 * 3 = 6")]);
  assert.deepEqual(parseInline("`unclosed"), [text("`unclosed")]);
});

test("blocks: fenced code keeps its content verbatim", () => {
  const [block] = parse('```json\n{ "ir": "1" }\n```');
  assert.deepEqual(block, { t: "code", lang: "json", v: '{ "ir": "1" }' });
});

test("blocks: an unclosed fence runs to the end rather than throwing", () => {
  const [block] = parse("```\nstill code\n");
  assert.equal(block.t, "code");
  assert.equal((block as { v: string }).v, "still code");
});

test("blocks: headings carry a GitHub-compatible slug", () => {
  const [block] = parse("## Command reference");
  assert.deepEqual(block, {
    t: "heading",
    level: 2,
    v: [text("Command reference")],
    slug: "command-reference",
  });
  assert.equal(slugify("`aura up` — start a node"), "aura-up--start-a-node");
});

test("blocks: a table cell may contain an escaped pipe", () => {
  // Straight from the guide: `aura promote <p> <op> --mode dry-run\|live`
  const md = "| Command | Purpose |\n|---|---|\n| `--mode a\\|b` | Sets it. |";
  const [block] = parse(md);
  assert.equal(block.t, "table");
  const t = block as Extract<ReturnType<typeof parse>[number], { t: "table" }>;
  assert.equal(t.head.length, 2);
  assert.equal(t.rows.length, 1);
  assert.deepEqual(t.rows[0][0], [{ t: "code", v: "--mode a|b" }]);
});

test("blocks: a pipe inside a code span does not split a cell", () => {
  const md = "| A | B |\n|---|---|\n| `x \\| y` | z |";
  const t = parse(md)[0] as Extract<ReturnType<typeof parse>[number], { t: "table" }>;
  assert.equal(t.rows[0].length, 2, "the escaped pipe must not create a third cell");
});

test("blocks: ordered and unordered lists do not merge", () => {
  const blocks = parse("- a\n- b\n\n1. one\n2. two");
  assert.equal(blocks.length, 2);
  assert.equal((blocks[0] as { ordered: boolean }).ordered, false);
  assert.equal((blocks[1] as { ordered: boolean }).ordered, true);
  assert.equal((blocks[0] as { items: unknown[] }).items.length, 2);
});

test("blocks: a list item absorbs its indented continuation", () => {
  const [block] = parse("- first line\n  continued here\n- second");
  const list = block as Extract<ReturnType<typeof parse>[number], { t: "list" }>;
  assert.equal(list.items.length, 2);
  assert.deepEqual(list.items[0], [text("first line continued here")]);
});

test("blocks: a paragraph stops at the heading that follows it", () => {
  const blocks = parse("prose here\n## Next");
  assert.equal(blocks.length, 2);
  assert.equal(blocks[0].t, "para");
  assert.equal(blocks[1].t, "heading");
});

test("blocks: blockquotes nest their own blocks", () => {
  const [block] = parse("> **note**\n> more");
  const q = block as Extract<ReturnType<typeof parse>[number], { t: "quote" }>;
  assert.equal(q.t, "quote");
  assert.equal(q.v[0].t, "para");
});

test("blocks: horizontal rules", () => {
  assert.deepEqual(parse("---")[0], { t: "hr" });
  assert.deepEqual(parse("***")[0], { t: "hr" });
});

test("a table divider is not mistaken for a horizontal rule", () => {
  const blocks = parse("| A | B |\n|---|---|\n| 1 | 2 |");
  assert.equal(blocks.length, 1);
  assert.equal(blocks[0].t, "table");
});

test("outline lists headings in order with their depth", () => {
  const o = outline(parse("# A\n\ntext\n\n## B\n\n### C"));
  assert.deepEqual(
    o.map((h) => [h.level, h.text, h.slug]),
    [
      [1, "A", "a"],
      [2, "B", "b"],
      [3, "C", "c"],
    ],
  );
});
