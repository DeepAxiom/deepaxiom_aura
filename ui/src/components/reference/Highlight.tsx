/**
 * Marks every occurrence of the search query inside a string.
 *
 * Splits on the match rather than replacing into HTML: the reference renders
 * spec text and command syntax, both of which contain angle brackets and
 * ampersands that an innerHTML path would either mangle or execute.
 */

export function Highlight({ text, query }: { text: string; query: string }) {
  if (!query) return <>{text}</>;

  const parts: React.ReactNode[] = [];
  const lower = text.toLowerCase();
  let at = 0;
  let key = 0;

  for (;;) {
    const found = lower.indexOf(query, at);
    if (found === -1) break;
    if (found > at) parts.push(text.slice(at, found));
    parts.push(
      <mark key={key++} className="hl">
        {text.slice(found, found + query.length)}
      </mark>,
    );
    at = found + query.length;
  }

  if (at === 0) return <>{text}</>;
  parts.push(text.slice(at));
  return <>{parts}</>;
}
