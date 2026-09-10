export interface SnippetSegment {
  readonly text: string;
  readonly mark: boolean;
}

const HTML_ENTITIES = [
  ["&lt;", "<"],
  ["&gt;", ">"],
  ["&#39;", "'"],
  ["&#34;", '"'],
  ["&amp;", "&"],
] as const;

function decodeEntities(text: string): string {
  let decoded = text;
  for (const [entity, character] of HTML_ENTITIES) {
    decoded = decoded.replaceAll(entity, character);
  }
  return decoded;
}

function hasOnlyBalancedMarkers(snippet: string): boolean {
  let marked = false;

  for (const marker of snippet.matchAll(/<mark>|<\/mark>/gu)) {
    if (marker[0] === "<mark>") {
      if (marked) return false;
      marked = true;
    } else {
      if (!marked) return false;
      marked = false;
    }
  }

  return !marked;
}

/** Splits balanced, non-nested server highlights into plain text runs. Malformed marker markup stays literal. */
export function snippetSegments(snippet: string): SnippetSegment[] {
  if (!hasOnlyBalancedMarkers(snippet)) {
    return snippet === "" ? [] : [{ text: decodeEntities(snippet), mark: false }];
  }

  let mark = false;
  const segments: SnippetSegment[] = [];
  for (const part of snippet.split(/(<mark>|<\/mark>)/u)) {
    if (part === "<mark>") {
      mark = true;
    } else if (part === "</mark>") {
      mark = false;
    } else if (part !== "") {
      segments.push({ text: decodeEntities(part), mark });
    }
  }

  return segments;
}

/** Renders server snippet matches as terminal Markdown bold text. */
export function snippetText(snippet: string): string {
  return snippetSegments(snippet)
    .map(({ text, mark }) => (mark ? `**${text}**` : text))
    .join("");
}
