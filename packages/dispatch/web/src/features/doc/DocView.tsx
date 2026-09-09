import { renderMermaidSVG } from "beautiful-mermaid";
import DOMPurify from "dompurify";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";
import Markdown, { type Components } from "react-markdown";
import rehypeSanitize from "rehype-sanitize";
import remarkGfm from "remark-gfm";

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
    if (current.type === "text" && typeof current.value === "string") {
      const renderedFrom = renderedOffset;
      renderedOffset += current.value.length;
      const from = current.position?.start?.offset;
      const to = current.position?.end?.offset;
      if (typeof from === "number" && typeof to === "number") {
        segments.push({ from, renderedFrom, renderedTo: renderedOffset, to });
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

function markdownSourceSegments(node: unknown): Record<string, string> {
  const segments = sourceTextSegments(node);
  return segments.length === 0 ? {} : { "data-dispatch-segments": JSON.stringify(segments) };
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

function selectedRange(root: HTMLElement): DocViewSelection | undefined {
  const selection = window.getSelection();
  if (selection === null || selection.rangeCount === 0 || selection.isCollapsed) {
    return undefined;
  }
  const range = selection.getRangeAt(0);
  const startBlock = closestMappedBlock(root, range.startContainer);
  const endBlock = closestMappedBlock(root, range.endContainer);
  if (startBlock === undefined || startBlock !== endBlock) {
    return undefined;
  }
  const from = rangeOffset(startBlock, range.startContainer, range.startOffset);
  const to = rangeOffset(startBlock, range.endContainer, range.endOffset);
  if (from === undefined || to === undefined || from === to) {
    return undefined;
  }
  const absoluteFrom = sourceOffset(sourceSegments(startBlock), Math.min(from, to), "from");
  const absoluteTo = sourceOffset(sourceSegments(startBlock), Math.max(from, to), "to");
  if (absoluteFrom === undefined || absoluteTo === undefined || absoluteFrom === absoluteTo) {
    return undefined;
  }
  const rect = range.getBoundingClientRect();
  return {
    from: absoluteFrom,
    quote: selection.toString(),
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

function highlightRange(root: HTMLElement, highlight: DocViewHighlight): void {
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
}

const components: Components = {
  blockquote({ children, node, ...props }) {
    return (
      <blockquote {...markdownSourceSegments(node)} {...props}>
        {children}
      </blockquote>
    );
  },
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
  h1({ children, node, ...props }) {
    return (
      <h1 {...markdownSourceSegments(node)} {...props}>
        {children}
      </h1>
    );
  },
  h2({ children, node, ...props }) {
    return (
      <h2 {...markdownSourceSegments(node)} {...props}>
        {children}
      </h2>
    );
  },
  h3({ children, node, ...props }) {
    return (
      <h3 {...markdownSourceSegments(node)} {...props}>
        {children}
      </h3>
    );
  },
  h4({ children, node, ...props }) {
    return (
      <h4 {...markdownSourceSegments(node)} {...props}>
        {children}
      </h4>
    );
  },
  h5({ children, node, ...props }) {
    return (
      <h5 {...markdownSourceSegments(node)} {...props}>
        {children}
      </h5>
    );
  },
  h6({ children, node, ...props }) {
    return (
      <h6 {...markdownSourceSegments(node)} {...props}>
        {children}
      </h6>
    );
  },
  li({ children, node, ...props }) {
    return (
      <li {...markdownSourceSegments(node)} {...props}>
        {children}
      </li>
    );
  },
  p({ children, node, ...props }) {
    return (
      <p {...markdownSourceSegments(node)} {...props}>
        {children}
      </p>
    );
  },
};

export interface DocViewHighlight {
  from: number;
  to: number;
}

export interface DocViewSelection {
  from: number;
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
  const [selectionUnsupported, setSelectionUnsupported] = useState(false);
  useEffect(() => {
    const article = root.current;
    if (article === null) {
      return;
    }
    clearHistoricalHighlights(article);
    if (highlight !== undefined) {
      highlightRange(article, highlight);
    }
    return () => clearHistoricalHighlights(article);
  }, [highlight]);

  const reportSelection = () => {
    if (root.current === null || onSelectionChange === undefined) {
      return;
    }
    const selection = selectedRange(root.current);
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
      <article
        className="prose prose-slate max-w-none break-words"
        onKeyUp={reportSelection}
        onMouseUp={reportSelection}
        ref={root}
      >
        <Markdown
          components={components}
          rehypePlugins={[rehypeSanitize]}
          remarkPlugins={[remarkGfm]}
        >
          {markdown}
        </Markdown>
      </article>
    </>
  );
}
