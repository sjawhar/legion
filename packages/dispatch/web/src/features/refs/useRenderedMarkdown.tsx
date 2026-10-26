import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import type { DOMSerializer, Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, type RefObject, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";

import type { ReferenceSource } from "../../api/client";
import { collectPictureAnchors, DispatchPicture, type PictureAnchor } from "./DispatchPicture";
import { linkableTexts, renderWithEngine, type SoftBreaks } from "./markdown-engine";
import {
  type BareReferences,
  collectReferenceAnchors,
  linkifyDispatchRefs,
  type ReferenceAnchor,
  RefLink,
} from "./RefLink";
import { issueQuery, referenceQuestionsQuery, referenceRetry } from "./reference-target";
import { textReferences } from "./routes";

/** The anchor list of a body with no references: one shared value, so re-rendering such a body
 *  leaves the state untouched instead of committing a fresh empty array each time. */
const NO_ANCHORS: readonly ReferenceAnchor[] = [];
/** The same, for a body with no Dispatch pictures. */
const NO_PICTURES: readonly PictureAnchor[] = [];

/**
 * What the issue keys and `#id`s markdown writes bare name (`BareReferences`), asked once per
 * distinct key and once per source: a key names an issue once `GET /api/v1/issues/{key}` answers
 * it (the issue query a reference's title reads, so the link costs no second request), and a `#id`
 * a question once the question map of the document source's text belongs to names it
 * (`referenceQuestionsQuery`), which is asked only for a body that writes a `#id` and a surface
 * that names its source. Only what a renderer can link is asked about: a key or `#id` the
 * server's reading of the source finds (`textReferences`) that also stands in the text the parsed
 * document links in (`linkableTexts`, the engine's own parse), so one written only in code or in a
 * link's destination costs no request; without an engine or a parse, the whole source is that
 * text, as the element then shows it. A 404 is final (`referenceRetry`, and `issueQuery` asks no
 * second time on a remount or a refocus; `issue.created` refetches it). Until an answer lands, and
 * when it is a 404, the key or `#id` names nothing, and the body shows it as written. The unfurl
 * cards read a body through it too (`Unfurl`), from the same cached queries.
 */
export function useBareReferences(
  markdown: string,
  source: ReferenceSource | undefined
): BareReferences {
  const written = useMemo(() => {
    const keys = new Set<string>();
    let question = false;
    for (const reference of textReferences(markdown)) {
      if (reference.kind === "key") {
        keys.add(reference.key);
      } else if (reference.kind === "question") {
        question = true;
      }
    }
    return { keys, question };
  }, [markdown]);
  // The linkable keys, joined, and whether a linkable `#id` is written: primitives, so a scan that
  // finds what the last one found re-renders nothing.
  const [linkableKeys, setLinkableKeys] = useState("");
  const [writesQuestion, setWritesQuestion] = useState(false);
  useLayoutEffect(() => {
    if (written.keys.size === 0 && !written.question) {
      setLinkableKeys("");
      setWritesQuestion(false);
      return;
    }
    return renderWithEngine((engine) => {
      const parsed = engine?.parse(markdown);
      const keys = new Set<string>();
      let question = false;
      for (const text of parsed === undefined ? [markdown] : linkableTexts(parsed)) {
        for (const reference of textReferences(text)) {
          if (reference.kind === "key" && written.keys.has(reference.key)) {
            keys.add(reference.key);
          } else if (reference.kind === "question" && written.question) {
            question = true;
          }
        }
      }
      setLinkableKeys([...keys].join(" "));
      setWritesQuestion(question);
    });
  }, [markdown, written]);
  const keys = useMemo(() => (linkableKeys === "" ? [] : linkableKeys.split(" ")), [linkableKeys]);
  const retry = referenceRetry(useQueryClient());
  // The keys whose issue exists, joined: a string, so an answer that changes nothing re-renders
  // nothing.
  const existing = useQueries({
    queries: keys.map((key) => ({ ...issueQuery(key), retry })),
    combine: (results) =>
      keys.filter((_key, index) => results[index]?.data !== undefined).join(" "),
  });
  const readsQuestions = source !== undefined && writesQuestion;
  const questionMap = useQuery({
    ...referenceQuestionsQuery(source),
    enabled: readsQuestions,
    retry,
  });
  const questions = readsQuestions ? questionMap.data?.questions : undefined;
  return useMemo(
    () => ({
      issues: new Set(existing === "" ? [] : existing.split(" ")),
      questions: new Map(questions?.map((question) => [question.block_id, question.ask])),
    }),
    [existing, questions]
  );
}

/** How a surface fills its element from the parsed document: `MarkdownBody` serializes the whole
 *  tree, `MarkdownPreview` one line of inline content. The serializer is the engine's: the
 *  schema's own, except that it writes a Dispatch picture as the placeholder a `DispatchPicture`
 *  portals into (`markdown-engine.ts`). */
export type PaintMarkdown = (
  element: HTMLElement,
  parsed: ProseMirrorNode,
  serializer: DOMSerializer
) => void;

export interface RenderedMarkdown {
  /** The engine could not load or parse the source, and the element holds it as literal text. */
  isFallback: boolean;
  /** A `RefLink` portal into every reference anchor of the element and a `DispatchPicture`
   *  portal into every Dispatch picture's placeholder; rendered inside it. */
  portals: ReactNode;
}

/** A surface's last pass over its rendered element, given the reference anchors the link pass
 *  found (each already emptied for its portal). It answers the anchors to portal into: the same
 *  ones, or whatever element it put in each one's place. */
export type DecorateMarkdown = (
  element: HTMLElement,
  anchors: readonly ReferenceAnchor[]
) => readonly ReferenceAnchor[];

/** What a surface tells `useRenderedMarkdown` beyond its element, source and paint; each has a
 *  default, so a surface names only what it changes. */
export interface RenderedMarkdownOptions {
  /** The shape each Dispatch picture's `DispatchPicture` takes: `block` (the default) at the
   *  column's width, `inline` as a thumbnail beside its caption. */
  pictures?: "block" | "inline";
  /** The surface's last pass over its rendered element (`DecorateMarkdown`); none by default. */
  decorate?: DecorateMarkdown;
  /** Called after each render lands; none by default. */
  onRendered?: () => void;
  /** The parse policy (`SoftBreaks`): `space`, the default, or `line`. */
  softBreaks?: SoftBreaks;
  /** The item whose text this is (`ReferenceSource`), which a `#id` in it resolves against: its
   *  document's question blocks. Without one a `#id` stays text; a bare issue key links either
   *  way. */
  source?: ReferenceSource;
}

/**
 * What every Markdown-bearing element does once mounted, in the layout phase of the commit
 * (`renderWithEngine`, which says why that phase): the source is parsed and handed to
 * `paint`, or, without an engine or a parse, written into the element as literal text with
 * `isFallback` set. A dispatch:// reference stays literal text to Markdown (it isn't a scheme GFM
 * autolinks), so bare refs then get wrapped into real links, as do the issue keys and `#id`s it
 * writes bare once they resolve (`useBareReferences`: a key whose issue exists, a `#id` that names
 * a question of `source`'s document; the body renders again when an answer lands); a same-origin
 * dashboard URL is already a real link by then (GFM autolinked it, or the author wrote it as
 * Markdown link syntax). Either way, every reference anchor gets its href rewritten to the SPA
 * route and its text handed to a portal-mounted `RefLink` for the resolved title. `decorate` runs
 * last, on the fallback text as on a painted element, so a `<mark>` it adds never splits a
 * reference before the link pass sees it, and a plain lead it prepends is never read as a
 * reference; it answers the anchors the portals mount into, so it may replace an anchor's element.
 * Each Dispatch picture's placeholder is found after it, wherever it left one (`MarkdownPreview`
 * puts an inert span in a link's place), and gets a portal-mounted `DispatchPicture` in the shape
 * `pictures` names. `onRendered` fires after each render lands; the newest callback is the one
 * called, and a new callback alone re-renders nothing. `paint` and `decorate` are dependencies: a
 * caller passes a module-level function or memoises one on what it reads. The options object
 * itself is not one, so a caller builds it inline on every render.
 */
export function useRenderedMarkdown(
  root: RefObject<HTMLElement | null>,
  markdown: string,
  paint: PaintMarkdown,
  {
    pictures = "block",
    decorate,
    onRendered,
    softBreaks = "space",
    source,
  }: RenderedMarkdownOptions = {}
): RenderedMarkdown {
  const onRenderedRef = useRef(onRendered);
  onRenderedRef.current = onRendered;
  const [referenceAnchors, setReferenceAnchors] = useState<readonly ReferenceAnchor[]>(NO_ANCHORS);
  const [pictureAnchors, setPictureAnchors] = useState<readonly PictureAnchor[]>(NO_PICTURES);
  const [isFallback, setIsFallback] = useState(false);
  const bare = useBareReferences(markdown, source);

  useLayoutEffect(() => {
    const element = root.current;
    if (element === null) {
      throw new Error("The Markdown element is unavailable.");
    }
    setIsFallback(false);
    return renderWithEngine((engine) => {
      const parsed = engine?.parse(markdown, softBreaks);
      if (engine === undefined || parsed === undefined) {
        setIsFallback(true);
        element.replaceChildren(document.createTextNode(markdown));
      } else {
        paint(element, parsed, engine.serializer);
      }
      linkifyDispatchRefs(element, bare);
      const found = collectReferenceAnchors(element);
      const anchors = decorate === undefined ? found : decorate(element, found);
      setReferenceAnchors(anchors.length === 0 ? NO_ANCHORS : anchors);
      const placeholders = collectPictureAnchors(element);
      setPictureAnchors(placeholders.length === 0 ? NO_PICTURES : placeholders);
      onRenderedRef.current?.();
    });
  }, [bare, decorate, markdown, paint, root, softBreaks]);

  return {
    isFallback,
    portals: [
      ...referenceAnchors.map(({ anchor, key, route, text }) =>
        createPortal(<RefLink route={route} text={text} />, anchor, key)
      ),
      ...pictureAnchors.map(({ anchor, caption, key, route }) =>
        createPortal(
          <DispatchPicture caption={caption} route={route} variant={pictures} />,
          anchor,
          key
        )
      ),
    ],
  };
}
