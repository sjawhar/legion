import { renderMermaidSVG } from "beautiful-mermaid";
import DOMPurify from "dompurify";
import { type ReactNode, useEffect, useMemo, useRef } from "react";
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

export function DocView({ markdown }: { markdown: string }): ReactNode {
  return (
    <article className="prose prose-slate max-w-none break-words">
      <Markdown
        components={components}
        rehypePlugins={[rehypeSanitize]}
        remarkPlugins={[remarkGfm]}
      >
        {markdown}
      </Markdown>
    </article>
  );
}
