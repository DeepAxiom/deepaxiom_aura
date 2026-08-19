/**
 * Every JSON Schema under spec/schemas, in full.
 *
 * These are what the kernel validates against, so they are shown as-is rather
 * than rendered into a prose description that could disagree with them.
 */

import { useState } from "react";
import { SCHEMAS } from "../../reference/generated";
import { Highlight } from "./Highlight";

export function SchemasPane({ query }: { query: string }) {
  const q = query.trim().toLowerCase();
  const matching = SCHEMAS.filter(
    (s) => !q || s.path.toLowerCase().includes(q) || s.body.toLowerCase().includes(q),
  );
  const [openPath, setOpenPath] = useState(SCHEMAS[0]?.path ?? "");
  const open = matching.find((s) => s.path === openPath) ?? matching[0];

  return (
    <div className="ref-pane ref-pane--split">
      <nav className="ref-side">
        {matching.map((s) => (
          <button
            key={s.path}
            className={`ref-side__item ${open?.path === s.path ? "ref-side__item--active" : ""}`}
            onClick={() => setOpenPath(s.path)}
          >
            <span className="ref-side__label">
              <Highlight text={s.path} query={q} />
            </span>
          </button>
        ))}
      </nav>

      {open && (
        <article className="ref-doc">
          <header className="ref-doc__head">
            <h1>{open.title || open.path}</h1>
            <code>spec/schemas/{open.path}</code>
            {open.id && <p className="ref-doc__status">{open.id}</p>}
          </header>
          <pre className="md__pre">
            <code>
              <Highlight text={open.body} query={q} />
            </code>
          </pre>
        </article>
      )}
    </div>
  );
}
