import { renderMermaidSVG } from "beautiful-mermaid";
import DOMPurify from "dompurify";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";
import Markdown, { type Components } from "react-markdown";
import rehypeSanitize, { defaultSchema } from "rehype-sanitize";
import remarkGfm from "remark-gfm";

const sanitizeSchema = {
  ...defaultSchema,
  attributes: {
    ...defaultSchema.attributes,
    "*": [...(defaultSchema.attributes?.["*"] ?? []), "dataDispatchSegments"],
  },
};

interface MermaidDiagramProps {
  source: string;
}

type MermaidRender = { svg: string } | { error: string };

export function sanitizeMermaidSvg(svg: string): DocumentFragment {
  const wrapper = document.createElement("div");
  wrapper.innerHTML = DOMPurify.sanitize(`<div>${svg}</div>`, {
    USE_PROFILES: { html: true, svg: true, svgFilters: true },
  });
  const diagram = wrapper.querySelector("svg");
  const fragment = document.createDocumentFragment();
  if (diagram === null) {
    return fragment;
  }

  for (const script of diagram.querySelectorAll("script")) {
    script.remove();
  }
  for (const element of [diagram, ...diagram.querySelectorAll("*")]) {
    for (const attribute of [...element.attributes]) {
      if (attribute.name.toLowerCase().startsWith("on")) {
        element.removeAttribute(attribute.name);
      }
    }
  }
  fragment.append(diagram);
  return fragment;
}

function MermaidDiagram({ source }: MermaidDiagramProps): ReactNode {
  const target = useRef<HTMLDivElement>(null);
  const rendered = useMemo<MermaidRender>(() => {
    try {
      return { svg: renderMermaidSVG(source) };
    } catch (error) {
      return { error: error instanceof Error ? error.message : String(error) };
    }
  }, [source]);

  useEffect(() => {
    if (!("svg" in rendered) || target.current === null) {
      return;
    }
    target.current.replaceChildren(sanitizeMermaidSvg(rendered.svg));
  }, [rendered]);

  if ("error" in rendered) {
    return (
      <figure className="my-4 rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-950">
        <figcaption role="alert">Could not render Mermaid: {rendered.error}</figcaption>
        <pre className="mt-3 overflow-x-auto rounded bg-white p-3 text-slate-900">
          <code>{source}</code>
        </pre>
      </figure>
    );
  }

  return (
    <div
      className="my-4 overflow-x-auto rounded-lg border border-slate-200 bg-white p-4"
      data-testid="mermaid-diagram"
      ref={target}
    />
  );
}

interface MarkdownNode {
  children?: unknown;
  data?: { hProperties?: Record<string, unknown> };
  position?: { end?: { offset?: unknown }; start?: { offset?: unknown } };
  type?: unknown;
  value?: unknown;
}

interface SourceTextSegment {
  from: number;
  renderedFrom: number;
  renderedTo: number;
  to: number;
}

function markdownNode(value: unknown): MarkdownNode | undefined {
  return typeof value === "object" && value !== null ? (value as MarkdownNode) : undefined;
}

function sourceTextSegments(node: unknown): SourceTextSegment[] {
  const segments: SourceTextSegment[] = [];
  let renderedOffset = 0;
  const visit = (value: unknown) => {
    const current = markdownNode(value);
    if (current === undefined) {
      return;
    }
    if (
      (current.type === "text" || current.type === "inlineCode") &&
      typeof current.value === "string"
    ) {
      const renderedFrom = renderedOffset;
      renderedOffset += current.value.length;
      const sourceFrom = current.position?.start?.offset;
      const sourceTo = current.position?.end?.offset;
      const delimiterWidth =
        current.type === "inlineCode" &&
        typeof sourceFrom === "number" &&
        typeof sourceTo === "number"
          ? (sourceTo - sourceFrom - current.value.length) / 2
          : 0;
      if (
        typeof sourceFrom === "number" &&
        Number.isInteger(delimiterWidth) &&
        delimiterWidth >= 0
      ) {
        const from = sourceFrom + delimiterWidth;
        segments.push({
          from,
          renderedFrom,
          renderedTo: renderedOffset,
          to: from + current.value.length,
        });
      }
      return;
    }
    if (Array.isArray(current.children)) {
      for (const child of current.children) {
        visit(child);
      }
    }
  };
  visit(node);
  return segments;
}

const mappedBlockTypes: Record<string, true> = {
  blockquote: true,
  heading: true,
  listItem: true,
  paragraph: true,
};

function annotateSourceSegments(value: unknown): void {
  const current = markdownNode(value);
  if (current === undefined) {
    return;
  }
  if (typeof current.type === "string" && mappedBlockTypes[current.type] === true) {
    const segments = sourceTextSegments(current);
    if (segments.length > 0) {
      current.data ??= {};
      current.data.hProperties ??= {};
      current.data.hProperties.dataDispatchSegments = JSON.stringify(segments);
    }
  }
  if (Array.isArray(current.children)) {
    for (const child of current.children) {
      annotateSourceSegments(child);
    }
  }
}

function remarkSourceSegments() {
  return (tree: unknown) => annotateSourceSegments(tree);
}

// A Range boundary on an element node points between two children (`offset` is a child
// index), not at a text position - e.g. a drag that starts before the first glyph or ends
// past the last glyph of a block, or a native "Select All", resolves there. Descend such a
// boundary into the equivalent leaf position (a text node, or an empty element) so both block
// lookup and offset computation see a point that actually lives inside the selected block.
function resolveBoundary(node: Node, offset: number): { node: Node; offset: number } {
  let current = node;
  let currentOffset = offset;
  while (current.nodeType === Node.ELEMENT_NODE && current.childNodes.length > 0) {
    if (currentOffset < current.childNodes.length) {
      current = current.childNodes[currentOffset] as Node;
      currentOffset = 0;
    } else {
      const last = current.childNodes[current.childNodes.length - 1] as Node;
      currentOffset =
        last.nodeType === Node.TEXT_NODE ? (last as Text).data.length : last.childNodes.length;
      current = last;
    }
  }
  return { node: current, offset: currentOffset };
}

function closestMappedBlock(root: HTMLElement, node: Node): HTMLElement | undefined {
  const element = node instanceof HTMLElement ? node : node.parentElement;
  const block = element?.closest<HTMLElement>("[data-dispatch-segments]");
  return block !== null && block !== undefined && root.contains(block) ? block : undefined;
}

function sourceSegments(block: HTMLElement): SourceTextSegment[] {
  const value = block.dataset.dispatchSegments;
  if (value === undefined) {
    return [];
  }
  try {
    const parsed: unknown = JSON.parse(value);
    if (!Array.isArray(parsed)) {
      return [];
    }
    return parsed.filter(
      (segment): segment is SourceTextSegment =>
        typeof segment === "object" &&
        segment !== null &&
        "from" in segment &&
        "renderedFrom" in segment &&
        "renderedTo" in segment &&
        "to" in segment &&
        typeof segment.from === "number" &&
        typeof segment.renderedFrom === "number" &&
        typeof segment.renderedTo === "number" &&
        typeof segment.to === "number" &&
        Number.isInteger(segment.from) &&
        Number.isInteger(segment.renderedFrom) &&
        Number.isInteger(segment.renderedTo) &&
        Number.isInteger(segment.to) &&
        segment.from <= segment.to &&
        segment.renderedFrom <= segment.renderedTo
    );
  } catch {
    return [];
  }
}

export function quoteOccurrence(text: string, quote: string, from: number): number | undefined {
  let occurrence = 0;
  for (let index = text.indexOf(quote); index !== -1; index = text.indexOf(quote, index + 1)) {
    if (index === from) {
      return occurrence;
    }
    occurrence++;
  }
  return undefined;
}

function rangeOffset(block: HTMLElement, node: Node, offset: number): number | undefined {
  if (!block.contains(node)) {
    return undefined;
  }
  const before = document.createRange();
  before.selectNodeContents(block);
  before.setEnd(node, offset);
  return before.toString().length;
}

function sourceOffset(
  segments: SourceTextSegment[],
  renderedOffset: number,
  boundary: "from" | "to"
): number | undefined {
  const matches = segments.filter(
    (segment) =>
      renderedOffset >= segment.renderedFrom &&
      renderedOffset <= segment.renderedTo &&
      segment.to - segment.from === segment.renderedTo - segment.renderedFrom
  );
  const segment = boundary === "from" ? matches[matches.length - 1] : matches[0];
  return segment === undefined ? undefined : segment.from + renderedOffset - segment.renderedFrom;
}

function sourceRangeHasGap(
  segments: SourceTextSegment[],
  renderedFrom: number,
  renderedTo: number
): boolean {
  const selected = segments.filter(
    (segment) => segment.renderedFrom < renderedTo && segment.renderedTo > renderedFrom
  );
  return selected.some((segment, index) => index > 0 && selected[index - 1]?.to !== segment.from);
}

function selectedRange(root: HTMLElement, markdown: string): DocViewSelection | undefined {
  const selection = window.getSelection();
  if (selection === null || selection.rangeCount === 0 || selection.isCollapsed) {
    return undefined;
  }
  const range = selection.getRangeAt(0);
  const start = resolveBoundary(range.startContainer, range.startOffset);
  const end = resolveBoundary(range.endContainer, range.endOffset);
  const startBlock = closestMappedBlock(root, start.node);
  const endBlock = closestMappedBlock(root, end.node);
  if (startBlock === undefined || startBlock !== endBlock) {
    return undefined;
  }
  const from = rangeOffset(startBlock, start.node, start.offset);
  const to = rangeOffset(startBlock, end.node, end.offset);
  if (from === undefined || to === undefined || from === to) {
    return undefined;
  }
  const renderedFrom = Math.min(from, to);
  const renderedTo = Math.max(from, to);
  const segments = sourceSegments(startBlock);
  const absoluteFrom = sourceOffset(segments, renderedFrom, "from");
  const absoluteTo = sourceOffset(segments, renderedTo, "to");
  if (
    absoluteFrom === undefined ||
    absoluteTo === undefined ||
    absoluteFrom === absoluteTo ||
    sourceRangeHasGap(segments, renderedFrom, renderedTo)
  ) {
    return undefined;
  }
  const quote = selection.toString();
  const rect = range.getBoundingClientRect();
  return {
    from: absoluteFrom,
    occurrence: quoteOccurrence(markdown, quote, absoluteFrom),
    quote,
    rect: { bottom: rect.bottom, left: rect.left, right: rect.right, top: rect.top },
    to: absoluteTo,
  };
}

function clearHistoricalHighlights(root: HTMLElement): void {
  for (const mark of root.querySelectorAll("mark.dispatch-anchor-history")) {
    mark.replaceWith(...mark.childNodes);
  }
  root.normalize();
}

function mapsEntireHighlightRange(root: HTMLElement, highlight: DocViewHighlight): boolean {
  let mappedTo = highlight.from;
  for (const block of root.querySelectorAll<HTMLElement>("[data-dispatch-segments]")) {
    for (const segment of sourceSegments(block)) {
      if (
        segment.to <= mappedTo ||
        segment.from >= highlight.to ||
        segment.to - segment.from !== segment.renderedTo - segment.renderedFrom
      ) {
        continue;
      }
      if (segment.from > mappedTo) {
        return false;
      }
      mappedTo = Math.min(segment.to, highlight.to);
      if (mappedTo === highlight.to) {
        return true;
      }
    }
  }
  return false;
}

function highlightRange(root: HTMLElement, highlight: DocViewHighlight): boolean {
  if (!mapsEntireHighlightRange(root, highlight)) {
    return false;
  }
  for (const block of root.querySelectorAll<HTMLElement>("[data-dispatch-segments]")) {
    const segments = sourceSegments(block);
    const textNodes: Array<{ length: number; text: Text }> = [];
    const walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT);
    for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
      const textNode = text as Text;
      textNodes.push({ length: textNode.data.length, text: textNode });
    }
    for (const segment of segments) {
      if (
        highlight.from >= segment.to ||
        highlight.to <= segment.from ||
        segment.to - segment.from !== segment.renderedTo - segment.renderedFrom
      ) {
        continue;
      }
      const renderedFrom =
        segment.renderedFrom + Math.max(highlight.from, segment.from) - segment.from;
      const renderedTo = segment.renderedFrom + Math.min(highlight.to, segment.to) - segment.from;
      let offset = 0;
      for (const { length, text } of textNodes) {
        const start = Math.max(renderedFrom - offset, 0);
        const end = Math.min(renderedTo - offset, length);
        if (start < end) {
          const range = document.createRange();
          range.setStart(text, start);
          range.setEnd(text, end);
          const mark = document.createElement("mark");
          mark.className = "dispatch-anchor-history";
          mark.dataset.dispatchAnchorHistory = "true";
          range.surroundContents(mark);
        }
        offset += length;
      }
    }
  }
  return true;
}

const components: Components = {
  code({ children, className, node: _node, ...props }) {
    const source = String(children).replace(/\n$/, "");
    if (className?.split(" ").includes("language-mermaid")) {
      return <MermaidDiagram source={source} />;
    }
    return (
      <code className={className} {...props}>
        {children}
      </code>
    );
  },
};

export interface DocViewHighlight {
  from: number;
  to: number;
}

export interface DocViewSelection {
  from: number;
  occurrence?: number;
  quote: string;
  rect: { bottom: number; left: number; right: number; top: number };
  to: number;
}

interface DocViewProps {
  highlight?: DocViewHighlight;
  markdown: string;
  onSelectionChange?: (selection: DocViewSelection | undefined) => void;
}

export function DocView({ highlight, markdown, onSelectionChange }: DocViewProps): ReactNode {
  const root = useRef<HTMLElement>(null);
  const [historicalHighlightMissing, setHistoricalHighlightMissing] = useState(false);
  const [selectionUnsupported, setSelectionUnsupported] = useState(false);
  const highlightedMarkdown = useRef(markdown);
  useEffect(() => {
    const article = root.current;
    if (article === null) {
      return;
    }
    const markdownChanged = highlightedMarkdown.current !== markdown;
    highlightedMarkdown.current = markdown;
    if (markdownChanged || highlight !== undefined) {
      clearHistoricalHighlights(article);
    }
    const historicalRangeMapped = highlight === undefined || highlightRange(article, highlight);
    setHistoricalHighlightMissing(!historicalRangeMapped);
    return () => clearHistoricalHighlights(article);
  }, [highlight, markdown]);

  const reportSelection = () => {
    if (root.current === null || onSelectionChange === undefined) {
      return;
    }
    const selection = selectedRange(root.current, markdown);
    const browserSelection = window.getSelection();
    setSelectionUnsupported(
      selection === undefined &&
        browserSelection !== null &&
        browserSelection.rangeCount > 0 &&
        !browserSelection.isCollapsed
    );
    onSelectionChange(selection);
  };

  return (
    <>
      {selectionUnsupported ? (
        <p className="mb-2 text-sm text-amber-800" role="status">
          This selection cannot be anchored. Select text within one Markdown block.
        </p>
      ) : null}
      {historicalHighlightMissing ? (
        <p className="mb-2 text-sm text-amber-800" role="status">
          Text changed. The selected range no longer exists in this document.
        </p>
      ) : null}
      <article
        className="prose prose-slate max-w-none break-words"
        onKeyUp={reportSelection}
        onMouseUp={reportSelection}
        ref={root}
      >
        <Markdown
          components={components}
          rehypePlugins={[[rehypeSanitize, sanitizeSchema]]}
          remarkPlugins={[remarkGfm, remarkSourceSegments]}
        >
          {markdown}
        </Markdown>
      </article>
    </>
  );
}
