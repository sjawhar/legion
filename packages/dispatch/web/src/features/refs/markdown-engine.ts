import type { HeadlessProofEditor } from "@legion/proof-editor/headless";
import type { Node as ProseMirrorNode } from "prosemirror-model";
import { useLayoutEffect, useState } from "react";

import type { BlockSchema } from "../../api/types";
import { loadBlockSchema } from "../doc/schema";
import { parseDispatchReference, referenceSpans, shortForm } from "./routes";

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

/** `.dispatch-markdown` in styles.css overrides Tailwind Typography's fixed palette, heading
 * scale and font size, so a rendered body takes the surrounding text's size and colour. */
export const markdownClassName =
  "dispatch-markdown prose prose-sm prose-slate break-words dark:prose-invert";

/** Text outside code with every `dispatch://` reference written as its short form
 *  (`dispatch://CORE-1/ask/a1` reads `CORE-1 ask`), as a rendered body's link reads until its
 *  title resolves; a reference in code stays as its author wrote it, as it does in a body. */
function shortenReferences(text: string): string {
  let shortened = "";
  let cursor = 0;
  for (const { start, value } of referenceSpans(text, "dispatch://")) {
    const route = parseDispatchReference(value);
    if (route !== undefined) {
      shortened += `${text.slice(cursor, start)}${shortForm(route)}`;
      cursor = start + value.length;
    }
  }
  return shortened + text.slice(cursor);
}

/** The words a parsed document renders, on one line: text as it reads (references shortened),
 *  raw inline HTML as the literal characters a body shows for it, and every block boundary,
 *  hard break or other leaf a space. */
function plainWords(doc: ProseMirrorNode): string {
  const parts: string[] = [];
  doc.descendants((node, _position, parent) => {
    if (node.isText) {
      const code =
        parent?.type.spec.code === true || node.marks.some((mark) => mark.type.spec.code === true);
      parts.push(code ? (node.text ?? "") : shortenReferences(node.text ?? ""));
    } else if (node.isBlock || node.isLeaf) {
      parts.push(node.type.name === "html" ? String(node.attrs.value ?? "") : " ");
    }
    return true;
  });
  return parts.join("");
}

/** A headline and whether it had to be cut to fit. */
export interface MarkdownHeadline {
  readonly text: string;
  readonly cut: boolean;
}

/**
 * Markdown projected to one line of plain text and cut to `max` characters, for a place that can
 * hold only a string: a reference link's text, a card's title, a hover tooltip. Formatting goes
 * (a link's own styling is the formatting there) but no syntax stays: `**Blocking:** the deploy`
 * reads `Blocking: the deploy`, a code span keeps its words, a `dispatch://` reference reads as
 * its short form, and the cut happens on the projected text, so it never splits a mark (nor a
 * character outside the Basic Multilingual Plane). Blocks and hard breaks become single spaces.
 * `cut` says whether the text was cut, which an ellipsis the author typed cannot fake.
 * `undefined` until the engine has answered (one pre-paint commit once it is loaded) and for an
 * `undefined` source; the literal source, cut the same way, when the engine cannot load.
 */
export function useMarkdownHeadline(
  markdown: string | undefined,
  max: number
): MarkdownHeadline | undefined {
  const [headline, setHeadline] = useState<MarkdownHeadline>();
  useLayoutEffect(() => {
    if (markdown === undefined) {
      setHeadline(undefined);
      return;
    }
    return renderWithHeadlessProof((proof) => {
      const parsed = proof === undefined ? undefined : parseMarkdownOrUndefined(proof, markdown);
      const characters = Array.from(
        (parsed === undefined ? markdown : plainWords(parsed)).replace(/\s+/g, " ").trim()
      );
      const cut = characters.length > max;
      setHeadline({
        cut,
        text: cut ? `${characters.slice(0, max).join("")}…` : characters.join(""),
      });
    });
  }, [markdown, max]);
  return headline;
}
