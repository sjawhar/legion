import type { HeadlessProofEditor } from "@legion/proof-editor/headless";
import { DOMSerializer, type Node as ProseMirrorNode } from "prosemirror-model";
import { useLayoutEffect, useState } from "react";

import type { BlockSchema } from "../../api/types";
import { loadBlockSchema } from "../doc/schema";
import { parseDispatchReference, referenceSpans, shortForm } from "./routes";

/**
 * The one headless Proof engine every Markdown-bearing surface parses with - `MarkdownBody` for a
 * whole body, `MarkdownPreview` for the clamped line of one - so a question, a comment, a hover
 * card and a search hit all read their source the way the document editor does. Its
 * `DOMSerializer` is built once with it: the schema is fixed for the engine's life, and every
 * render on the page would otherwise rebuild the same serializer.
 */
export interface MarkdownEngine {
  readonly serializer: DOMSerializer;
  /**
   * The parsed document, or `undefined` for a source the parser refuses, which the caller shows
   * as one plain-text node. Proof's schema has no node for CommonMark's raw-HTML spans (a bare
   * tag-shaped substring outside a code span or fence, e.g. `<img src=x>`), nor for a
   * reference-style link or image (`[text][id]` with its `[id]: url` definition), so the parser
   * throws for them. HTML-like text inside a code span or fence parses safely as literal text,
   * so nothing is escaped beforehand: escaping the whole source regardless of context would
   * print literal backslashes around any code span or fence containing something tag-shaped.
   *
   * A throw leaves Proof's parser state mid-document, and the next one or two parses through the
   * same engine throw too, whatever their text (measured: `Fixed **two** bugs` and `1. one\n2.
   * two` both failed after a reference link), so a refused body would blank the formatting of
   * the next turn or card someone else is reading. The engine therefore retires itself on a
   * throw: this parse answers `undefined`, and the next render builds a fresh engine from the
   * module already loaded (about a millisecond) before parsing.
   */
  parse(markdown: string): ProseMirrorNode | undefined;
}

type HeadlessModule = typeof import("@legion/proof-editor/headless");

/**
 * What a single newline inside a paragraph becomes: `"space"` (the document default: hard-wrapped
 * Markdown is one paragraph) or `"line"` (text whose author meant its lines as lines: a model's
 * streamed turn in the live view). Each policy is its own engine, since the parser is built with
 * it; both can be ready at once.
 */
export type SoftBreaks = "space" | "line";

let headlessModule: Promise<HeadlessModule> | undefined;
const engines = new Map<string, Promise<MarkdownEngine>>();
/** The engine of each policy the newest resolved load produced: what a body renders with
 *  synchronously. An entry is cleared by a parse that threw, so the next render loads (and
 *  caches) a replacement. */
const readyEngines = new Map<SoftBreaks, MarkdownEngine>();

function engineKey(blockSchema: BlockSchema, softBreaks: SoftBreaks): string {
  return `${blockSchema.version}:${softBreaks}`;
}

function buildEngine(
  proof: HeadlessProofEditor,
  blockSchema: BlockSchema,
  softBreaks: SoftBreaks
): MarkdownEngine {
  const engine: MarkdownEngine = {
    serializer: DOMSerializer.fromSchema(proof.schema),
    parse(markdown) {
      try {
        return proof.parseMarkdown(markdown);
      } catch {
        if (readyEngines.get(softBreaks) === engine) {
          readyEngines.delete(softBreaks);
          engines.delete(engineKey(blockSchema, softBreaks));
        }
        return undefined;
      }
    },
  };
  return engine;
}

function loadEngine(blockSchema: BlockSchema, softBreaks: SoftBreaks): Promise<MarkdownEngine> {
  const key = engineKey(blockSchema, softBreaks);
  const cached = engines.get(key);
  if (cached !== undefined) {
    return cached;
  }
  headlessModule ??= import("@legion/proof-editor/headless");
  const created = headlessModule
    .then(({ createHeadlessProof }) => createHeadlessProof({ blockSchema, softBreaks }))
    .then((proof) => {
      const engine = buildEngine(proof, blockSchema, softBreaks);
      readyEngines.set(softBreaks, engine);
      return engine;
    });
  engines.set(key, created);
  return created;
}

/** Loads the schema and headless Proof chunk before Markdown-bearing UI needs to render. */
export async function warmMarkdownRenderer(): Promise<void> {
  await loadEngine(await loadBlockSchema(), "space");
}

/**
 * Calls `render` with the engine of `softBreaks`: synchronously when a load has already
 * resolved, so a body renders in the layout phase of the commit that mounts it (a list that
 * inserts a turn sees the turn's full height in that same commit, and `ViewportAnchor`
 * compensates in one measurement); otherwise once the schema and the engine chunk arrive. Only
 * the very first render of a policy on a page, or a schema that failed to load, takes the
 * asynchronous path. Without the server schema (or Proof's headless engine) `render` gets
 * `undefined` and shows the literal text instead: readable, never lost. The cause is reported
 * and the schema cache does not retain the failure, so the next render tries the fetch again.
 * Returns the cancel for the asynchronous path; a cancelled `render` never runs.
 */
export function renderWithEngine(
  render: (engine: MarkdownEngine | undefined) => void,
  softBreaks: SoftBreaks = "space"
): () => void {
  const ready = readyEngines.get(softBreaks);
  if (ready !== undefined) {
    render(ready);
    return () => undefined;
  }
  let cancelled = false;
  void (async () => {
    let engine: MarkdownEngine | undefined;
    try {
      engine = await loadEngine(await loadBlockSchema(), softBreaks);
    } catch (error) {
      console.error("Rendering literal Markdown, the block schema is unavailable", error);
      engine = undefined;
    }
    if (!cancelled) {
      render(engine);
    }
  })();
  return () => {
    cancelled = true;
  };
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
    return renderWithEngine((engine) => {
      const parsed = engine?.parse(markdown);
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
