import type { DOMSerializer, Node as ProseMirrorNode } from "prosemirror-model";
import {
  type ReactNode,
  type RefObject,
  useContext,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";

import { type AskBlockHost, applyAskBlockShell, askShellSlots } from "../doc/typed-block-drawing";
import { collectViewSlots, ViewBlock, type ViewBlockSlot } from "../view/ViewBlock";
import { collectPictureAnchors, DispatchPicture, type PictureAnchor } from "./DispatchPicture";
import { TextAskCard } from "./markdown-cards";
import { renderWithEngine, type SoftBreaks } from "./markdown-engine";
import { drawMermaidDiagrams } from "./mermaid-diagrams";
import {
  collectReferenceAnchors,
  linkifyDispatchRefs,
  type ReferenceAnchor,
  RefLink,
} from "./RefLink";

/** The anchor list of a body with no references: one shared value, so re-rendering such a body
 *  leaves the state untouched instead of committing a fresh empty array each time. */
const NO_ANCHORS: readonly ReferenceAnchor[] = [];
/** The same, for a body with no Dispatch pictures. */
const NO_PICTURES: readonly PictureAnchor[] = [];
/** The same, for a body with no decision card to mount. */
const NO_ASK_HOSTS: readonly AskBlockHost[] = [];
/** The same, for a body with no `view` block. */
const NO_VIEWS: readonly ViewBlockSlot[] = [];

/**
 * A host for each `ask` shell `paint` drew under `element` (`renderAskBlockShell`), paired in
 * document order with the parsed document's `ask` nodes. A surface that drew no shell for them
 * (`MarkdownPreview` paints one line of inline content) gets none: the shells must match the
 * nodes one for one, every one with its three slots (`askShellSlots`), or no card is mounted and
 * nothing is written. Once every shell has paired, it writes each through `applyAskBlockShell`,
 * and only there, because a card is mounted into it.
 */
function collectAskHosts(element: HTMLElement, parsed: ProseMirrorNode): readonly AskBlockHost[] {
  const shells = element.querySelectorAll<HTMLElement>('section[data-proof-block-type="ask"]');
  if (shells.length === 0) {
    return NO_ASK_HOSTS;
  }
  const nodes: ProseMirrorNode[] = [];
  parsed.descendants((node) => {
    if (node.type.name === "ask") {
      nodes.push(node);
    }
    return !node.isTextblock;
  });
  if (nodes.length !== shells.length) {
    return NO_ASK_HOSTS;
  }
  const paired: {
    readonly content: HTMLElement;
    readonly host: AskBlockHost;
    readonly shell: HTMLElement;
  }[] = [];
  for (const [index, node] of nodes.entries()) {
    const slots = askShellSlots(shells[index]);
    if (slots === undefined) {
      return NO_ASK_HOSTS;
    }
    const { content, footer, header } = slots;
    paired.push({ content, host: { footer, header, key: index, node }, shell: shells[index] });
  }
  for (const { content, host, shell } of paired) {
    applyAskBlockShell(shell, content, host.node);
  }
  return paired.map(({ host }) => host);
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
  /** A `RefLink` portal into every reference anchor of the element, a `DispatchPicture` portal
   *  into every Dispatch picture's placeholder, and the provided decision card (`TextAskCard`)
   *  for every `ask` shell, which portals into the shell's slots itself; rendered inside it. */
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
}

/**
 * What every Markdown-bearing element does once mounted, in the layout phase of the commit
 * (`renderWithEngine`, which says why that phase): the source is parsed and handed to
 * `paint`, or, without an engine or a parse, written into the element as literal text with
 * `isFallback` set. A dispatch:// reference stays literal text to Markdown (it isn't a scheme GFM
 * autolinks), so bare refs then get wrapped into real links; a same-origin dashboard URL is
 * already a real link by then (GFM autolinked it, or the author wrote it as Markdown link
 * syntax). Either way, every reference anchor gets its href rewritten to the SPA route and its
 * text handed to a portal-mounted `RefLink` for the resolved title. `decorate` runs last, on the
 * fallback text as on a painted element, so a `<mark>` it adds never splits a reference before
 * the link pass sees it, and a plain lead it prepends is never read as a reference; it answers the
 * anchors the portals mount into, so it may replace an anchor's element. Each Dispatch picture's
 * placeholder is found after it, wherever it left one (`MarkdownPreview` puts an inert span in a
 * link's place), and gets a portal-mounted `DispatchPicture` in the shape `pictures` names.
 * Where the app provides a decision card (`TextAskCard`), each `ask` shell a parsed `paint` drew
 * gets one, mounted in the same commit (`collectAskHosts`); the card is a dependency, so a new
 * provider value renders again. On a painted body, each fenced `mermaid` block then draws as its
 * diagram (`drawMermaidDiagrams`, which draws a source it has not drawn before late, once its
 * worker answers; the effect's cleanup cancels that). It runs after every other pass, so a
 * `dispatch://` in a diagram's label is never wrapped as a link or collected as a reference.
 * `onRendered` fires after each render lands; the newest callback is the one called, and a new
 * callback alone re-renders nothing. `paint` and `decorate` are dependencies: a caller passes a
 * module-level function or memoises one on what it reads. The options object itself is not one,
 * so a caller builds it inline on every render.
 */
export function useRenderedMarkdown(
  root: RefObject<HTMLElement | null>,
  markdown: string,
  paint: PaintMarkdown,
  { pictures = "block", decorate, onRendered, softBreaks = "space" }: RenderedMarkdownOptions = {}
): RenderedMarkdown {
  const onRenderedRef = useRef(onRendered);
  onRenderedRef.current = onRendered;
  const Card = useContext(TextAskCard);
  const [referenceAnchors, setReferenceAnchors] = useState<readonly ReferenceAnchor[]>(NO_ANCHORS);
  const [pictureAnchors, setPictureAnchors] = useState<readonly PictureAnchor[]>(NO_PICTURES);
  const [askHosts, setAskHosts] = useState<readonly AskBlockHost[]>(NO_ASK_HOSTS);
  const [viewSlots, setViewSlots] = useState<readonly ViewBlockSlot[]>(NO_VIEWS);
  const [isFallback, setIsFallback] = useState(false);

  useLayoutEffect(() => {
    const element = root.current;
    if (element === null) {
      throw new Error("The Markdown element is unavailable.");
    }
    setIsFallback(false);
    let cancelDraw: (() => void) | undefined;
    const cancelRender = renderWithEngine((engine) => {
      const parsed = engine?.parse(markdown, softBreaks);
      let hosts = NO_ASK_HOSTS;
      if (engine === undefined || parsed === undefined) {
        setIsFallback(true);
        element.replaceChildren(document.createTextNode(markdown));
      } else {
        paint(element, parsed, engine.serializer);
        if (Card !== undefined) {
          hosts = collectAskHosts(element, parsed);
        }
      }
      setAskHosts(hosts);
      linkifyDispatchRefs(element);
      const found = collectReferenceAnchors(element);
      const anchors = decorate === undefined ? found : decorate(element, found);
      setReferenceAnchors(anchors.length === 0 ? NO_ANCHORS : anchors);
      const placeholders = collectPictureAnchors(element);
      setPictureAnchors(placeholders.length === 0 ? NO_PICTURES : placeholders);
      const slots = collectViewSlots(element);
      setViewSlots(slots.length === 0 ? NO_VIEWS : slots);
      if (engine !== undefined && parsed !== undefined) {
        cancelDraw = drawMermaidDiagrams(element, parsed, markdown, (source) =>
          engine.parse(source, softBreaks)
        );
      }
      onRenderedRef.current?.();
    });
    return () => {
      cancelRender();
      cancelDraw?.();
    };
  }, [Card, decorate, markdown, paint, root, softBreaks]);

  return {
    isFallback,
    portals: [
      ...referenceAnchors.map(({ anchor, key, route }) =>
        createPortal(<RefLink anchor={anchor} route={route} />, anchor, key)
      ),
      ...pictureAnchors.map(({ anchor, caption, key, route }) =>
        createPortal(
          <DispatchPicture caption={caption} route={route} variant={pictures} />,
          anchor,
          key
        )
      ),
      ...viewSlots.map((slot) =>
        createPortal(
          <ViewBlock caption={slot.caption} data={slot.data} host={slot.host} page={slot.page} />,
          slot.host,
          slot.key
        )
      ),
      ...(Card === undefined
        ? []
        : askHosts.map((host) => <Card host={host} key={`ask-${host.key}`} />)),
    ],
  };
}
