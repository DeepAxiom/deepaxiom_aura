/**
 * Renders the block tree from `parse.ts`. Contains no parsing.
 *
 * The split is the point: every edge case lives in the parser, where it is
 * unit-tested, and this file stays a total function from node type to element.
 * Adding a block kind means one case here and one case there, never a regex in
 * a component.
 *
 * Nothing is rendered as HTML. The specs are trusted content, but they are also
 * *data* — one day a registry will serve a skill's README through this same
 * renderer — and a component that reaches for dangerouslySetInnerHTML once
 * keeps it forever.
 */

import { useMemo } from "react";
import { type Block, type Inline, parse } from "./parse";

function InlineNodes({ nodes }: { nodes: Inline[] }) {
  return (
    <>
      {nodes.map((n, i) => {
        switch (n.t) {
          case "text":
            return <span key={i}>{n.v}</span>;
          case "code":
            return <code key={i}>{n.v}</code>;
          case "strong":
            return (
              <strong key={i}>
                <InlineNodes nodes={n.v} />
              </strong>
            );
          case "em":
            return (
              <em key={i}>
                <InlineNodes nodes={n.v} />
              </em>
            );
          case "del":
            return (
              <del key={i}>
                <InlineNodes nodes={n.v} />
              </del>
            );
          case "link": {
            // An in-document anchor scrolls; anything else leaves the app, and
            // leaving the app from a page served by the node means a new tab
            // with no referrer.
            const internal = n.href.startsWith("#");
            return (
              <a
                key={i}
                href={n.href}
                {...(internal ? {} : { target: "_blank", rel: "noreferrer noopener" })}
              >
                <InlineNodes nodes={n.v} />
              </a>
            );
          }
        }
      })}
    </>
  );
}

function BlockNode({ block }: { block: Block }) {
  switch (block.t) {
    case "heading": {
      const H = `h${Math.min(block.level, 6)}` as "h1";
      return (
        <H id={block.slug} className="md__h">
          <InlineNodes nodes={block.v} />
        </H>
      );
    }
    case "para":
      return (
        <p>
          <InlineNodes nodes={block.v} />
        </p>
      );
    case "code":
      return (
        <pre className="md__pre">
          <code data-lang={block.lang}>{block.v}</code>
        </pre>
      );
    case "list": {
      const L = block.ordered ? "ol" : "ul";
      return (
        <L>
          {block.items.map((item, i) => (
            <li key={i}>
              <InlineNodes nodes={item} />
            </li>
          ))}
        </L>
      );
    }
    case "table":
      return (
        <div className="md__table-wrap">
          <table className="table">
            <thead>
              <tr>
                {block.head.map((cell, i) => (
                  <th key={i}>
                    <InlineNodes nodes={cell} />
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {block.rows.map((row, i) => (
                <tr key={i}>
                  {row.map((cell, j) => (
                    <td key={j}>
                      <InlineNodes nodes={cell} />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
    case "quote":
      return (
        <blockquote className="md__quote">
          {block.v.map((b, i) => (
            <BlockNode key={i} block={b} />
          ))}
        </blockquote>
      );
    case "hr":
      return <hr className="md__hr" />;
  }
}

export function Markdown({ source }: { source: string }) {
  // Parsing a 27 kB contract on every keystroke elsewhere in the view is the
  // one performance trap in this component.
  const blocks = useMemo(() => parse(source), [source]);
  return (
    <div className="md">
      {blocks.map((b, i) => (
        <BlockNode key={i} block={b} />
      ))}
    </div>
  );
}
