import type { HeadlessProofEditor } from "@legion/proof-editor/headless";
import type { Node as ProseMirrorNode } from "prosemirror-model";
import { useLayoutEffect, useState } from "react";

import type { BlockSchema } from "../../api/types";
import { loadBlockSchema } from "../doc/schema";

/**
 * The one headless Proof engine every Markdown-bearing surface parses with - `MarkdownBody` for a
 * whole body, `MarkdownPreview` for the clamped line of one - so a question, a comment, a hover
 * card and a search hit all read their source the way the document editor does.
 */

const headlessProofs = new Map<number, Promise<HeadlessProofEditor>>();
/** The engine the newest resolved load produced: what a body renders with synchronously. */
let readyHeadlessProof: HeadlessProofEditor | undefined;

function loadHeadlessProof(blockSchema: BlockSchema): Promise<HeadlessProofEditor> {
  const cached = headlessProofs.get(blockSchema.version);
  if (cached !== undefined) {
    return cached;
  }
  const created = import("@legion/proof-editor/headless")
    .then(({ createHeadlessProof }) => createHeadlessProof({ blockSchema }))
    .then((proof) => {
      readyHeadlessProof = proof;
      return proof;
    });
  headlessProofs.set(blockSchema.version, created);
  return created;
}

/** Loads the schema and headless Proof chunk before Markdown-bearing UI needs to render. */
export async function warmMarkdownRenderer(): Promise<void> {
  await loadHeadlessProof(await loadBlockSchema());
}

/**
 * Calls `render` with the engine: synchronously when a load has already resolved, so a body
 * renders in the layout phase of the commit that mounts it (a list that inserts a turn sees the
 * turn's full height in that same commit, and `ViewportAnchor` compensates in one measurement);
 * otherwise once the schema and the engine chunk arrive. Only the very first render on a page,
 * or a schema that failed to load, takes the asynchronous path. Without the server schema (or
 * Proof's headless engine) `render` gets `undefined` and shows the literal text instead:
 * readable, never lost. The cause is reported and the schema cache does not retain the failure,
 * so the next render tries the fetch again. Returns the cancel for the asynchronous path; a
 * cancelled `render` never runs.
 */
export function renderWithHeadlessProof(
  render: (proof: HeadlessProofEditor | undefined) => void
): () => void {
  if (readyHeadlessProof !== undefined) {
    render(readyHeadlessProof);
    return () => undefined;
  }
  let cancelled = false;
  void (async () => {
    let proof: HeadlessProofEditor | undefined;
    try {
      proof = await loadHeadlessProof(await loadBlockSchema());
    } catch (error) {
      console.error("Rendering literal Markdown, the block schema is unavailable", error);
      proof = undefined;
    }
    if (!cancelled) {
      render(proof);
    }
  })();
  return () => {
    cancelled = true;
  };
}

/**
 * Proof's schema has no node for CommonMark's raw-HTML block/inline spans (a bare tag-shaped
 * substring outside a code span or fence, e.g. `<img src=x>`), so `parseMarkdown` throws for it.
 * HTML-like text already inside a code span or fence parses safely as literal text (the parser
 * never treats code content as raw HTML), so nothing needs escaping there - pre-escaping the
 * whole source regardless of context, as an earlier version of `MarkdownBody` did, printed
 * literal backslashes around any code span or fence containing something tag-shaped. Answering
 * `undefined` on a parse failure, which the caller shows as one plain-text node, keeps that one
 * edge case inert without corrupting the (far more common) case of tag-shaped text quoted in
 * code.
 */
export function parseMarkdownOrUndefined(
  proof: HeadlessProofEditor,
  markdown: string
): ProseMirrorNode | undefined {
  try {
    return proof.parseMarkdown(markdown);
  } catch {
    return undefined;
  }
}

/** The first textblock's own inline content (marks intact), plus the flattened plain text of
 * every textblock after it, joined with a single space - see `flattenInline`. */
export interface InlineContent {
  head: ProseMirrorNode;
  extra: string;
}

/** A textblock's own text, skipping any code span or code block content — used only for the
 * "extra" (non-first) textblocks `flattenInline` yields, which flatten to plain text and so
 * would otherwise re-expose a code span's `dispatch://` ref as linkifiable bare text once its
 * `code` mark is gone. The first textblock keeps its marks (serialized, not flattened), so its
 * own code spans stay real `<code>` elements and need no such filtering. */
function plainTextExcludingCode(node: ProseMirrorNode): string {
  const parts: string[] = [];
  node.descendants((child) => {
    if (child.type.name === "code_block") {
      return false;
    }
    if (child.isText) {
      const hasCode = child.marks.some((mark) => mark.type.name === "inlineCode");
      if (!hasCode) {
        parts.push(child.text ?? "");
      }
    }
    return true;
  });
  return parts.join("");
}

/** A parsed document collapsed onto one line: the first textblock's inline content verbatim
 * (marks intact) and every later textblock's plain text after it, space-joined, so a list or a
 * multi-paragraph body renders as one run of inline content instead of nesting a `<ul>` or a
 * second `<p>` inside a span. `undefined` for a document with no text at all. */
export function flattenInline(root: ProseMirrorNode): InlineContent | undefined {
  let head: ProseMirrorNode | undefined;
  const extra: string[] = [];
  root.descendants((node) => {
    if (!node.isTextblock) {
      return true;
    }
    if (head === undefined) {
      head = node;
    } else {
      const text = plainTextExcludingCode(node).trim();
      if (text !== "") {
        extra.push(text);
      }
    }
    return false;
  });
  return head === undefined ? undefined : { extra: extra.join(" "), head };
}

/** `.dispatch-markdown` in styles.css overrides Tailwind Typography's fixed palette, heading
 * scale and font size, so a rendered body takes the surrounding text's size and colour. */
export const markdownClassName =
  "dispatch-markdown prose prose-sm prose-slate break-words dark:prose-invert";

/**
 * Markdown projected to one line of plain text and cut to `max` characters, for a place that can
 * hold only a string: a reference link's text, a card's title. Formatting goes (a link's own
 * styling is the formatting there) but no syntax stays: `**Blocking:** the deploy` reads
 * `Blocking: the deploy`, a code span keeps its words, and the cut happens on the projected
 * text, so it never splits a mark. Blocks and hard breaks become single spaces. `undefined`
 * until the engine has answered (one pre-paint commit once it is loaded) and for an `undefined`
 * source; the literal source, cut the same way, when the engine cannot load.
 */
export function useMarkdownHeadline(markdown: string | undefined, max: number): string | undefined {
  const [headline, setHeadline] = useState<string>();
  useLayoutEffect(() => {
    if (markdown === undefined) {
      setHeadline(undefined);
      return;
    }
    return renderWithHeadlessProof((proof) => {
      const parsed = proof === undefined ? undefined : parseMarkdownOrUndefined(proof, markdown);
      const text = (
        parsed === undefined ? markdown : parsed.textBetween(0, parsed.content.size, " ", " ")
      )
        .replace(/\s+/g, " ")
        .trim();
      setHeadline(text.length > max ? `${text.slice(0, max)}…` : text);
    });
  }, [markdown, max]);
  return headline;
}
