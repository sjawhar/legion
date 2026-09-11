export interface Highlight {
  by: string;
  id: string;
  quote: string;
}

function escapeAttribute(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll('"', "&quot;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}

export function embedHighlight(markdown: string, highlight: Highlight): string | undefined {
  const from = markdown.indexOf(highlight.quote);
  if (from === -1 || markdown.indexOf(highlight.quote, from + 1) !== -1) {
    return undefined;
  }
  const span = `<span data-proof="comment" data-id="${escapeAttribute(highlight.id)}" data-by="${escapeAttribute(highlight.by)}">${highlight.quote}</span>`;
  return `${markdown.slice(0, from)}${span}${markdown.slice(from + highlight.quote.length)}`;
}
