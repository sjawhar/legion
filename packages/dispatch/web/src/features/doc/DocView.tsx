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

function sourceOffsets(node: unknown): Record<string, string> {
  if (
    typeof node !== "object" ||
    node === null ||
    !("position" in node) ||
    typeof node.position !== "object" ||
    node.position === null ||
    !("start" in node.position) ||
    !("end" in node.position) ||
    typeof node.position.start !== "object" ||
    node.position.start === null ||
    typeof node.position.end !== "object" ||
    node.position.end === null ||
    !("offset" in node.position.start) ||
    !("offset" in node.position.end) ||
    typeof node.position.start.offset !== "number" ||
    typeof node.position.end.offset !== "number"
  ) {
    return {};
  }
  return {
    "data-dispatch-from": String(node.position.start.offset),
    "data-dispatch-to": String(node.position.end.offset),
  };
}

function closestMappedBlock(root: HTMLElement, node: Node): HTMLElement | undefined {
  const element = node instanceof HTMLElement ? node : node.parentElement;
  const block = element?.closest<HTMLElement>("[data-dispatch-from]");
  return block !== null && block !== undefined && root.contains(block) ? block : undefined;
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

function selectedRange(root: HTMLElement, markdown: string): DocViewSelection | undefined {
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
  const sourceFrom = Number(startBlock.dataset.dispatchFrom);
  const sourceTo = Number(startBlock.dataset.dispatchTo);
  if (
    !Number.isInteger(sourceFrom) ||
    !Number.isInteger(sourceTo) ||
    markdown.slice(sourceFrom, sourceTo) !== startBlock.textContent
  ) {
    return undefined;
  }
  const from = rangeOffset(startBlock, range.startContainer, range.startOffset);
  const to = rangeOffset(startBlock, range.endContainer, range.endOffset);
  if (from === undefined || to === undefined || from === to) {
    return undefined;
  }
  const absoluteFrom = sourceFrom + Math.min(from, to);
  const absoluteTo = sourceFrom + Math.max(from, to);
  const quote = markdown.slice(absoluteFrom, absoluteTo);
  if (quote !== selection.toString()) {
    return undefined;
  }
  const rect = range.getBoundingClientRect();
  return {
    from: absoluteFrom,
    quote,
    rect: { bottom: rect.bottom, left: rect.left, right: rect.right, top: rect.top },
    to: absoluteTo,
  };
}

function highlightRange(root: HTMLElement, markdown: string, highlight: DocViewHighlight): void {
  for (const block of root.querySelectorAll<HTMLElement>("[data-dispatch-from]")) {
    const sourceFrom = Number(block.dataset.dispatchFrom);
    const sourceTo = Number(block.dataset.dispatchTo);
    if (
      !Number.isInteger(sourceFrom) ||
      !Number.isInteger(sourceTo) ||
      highlight.from < sourceFrom ||
      highlight.to > sourceTo ||
      markdown.slice(sourceFrom, sourceTo) !== block.textContent
    ) {
      continue;
    }
    const textNodes: Text[] = [];
    const walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT);
    for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
      textNodes.push(text as Text);
    }
    let offset = sourceFrom;
    for (const text of textNodes) {
      const start = Math.max(highlight.from - offset, 0);
      const end = Math.min(highlight.to - offset, text.data.length);
      if (start < end) {
        const range = document.createRange();
        range.setStart(text, start);
        range.setEnd(text, end);
        const mark = document.createElement("mark");
        mark.className = "dispatch-anchor-history";
        mark.dataset.dispatchAnchorHistory = "true";
        range.surroundContents(mark);
      }
      offset += text.data.length;
    }
    return;
  }
}

const components: Components = {
  blockquote({ children, node, ...props }) {
    return (
      <blockquote {...sourceOffsets(node)} {...props}>
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
  li({ children, node, ...props }) {
    return (
      <li {...sourceOffsets(node)} {...props}>
        {children}
      </li>
    );
  },
  p({ children, node, ...props }) {
    return (
      <p {...sourceOffsets(node)} {...props}>
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
    if (highlight === undefined || root.current === null) {
      return;
    }
    highlightRange(root.current, markdown, highlight);
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
          This selection cannot be anchored. Select text within one unchanged markdown block.
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
