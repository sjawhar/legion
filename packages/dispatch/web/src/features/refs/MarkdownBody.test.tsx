import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import * as mermaidRenderer from "@legion/proof-editor/mermaid";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, waitFor, within } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { Artifact, AskRead } from "../../api/types";
import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";
import { resetMermaidDiagrams } from "./mermaid-diagrams";

function withQueries(children: ReactNode): ReactNode {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

/** An image artifact of CORE-1 with two versions, as the issue read lists it. */
const picture: Artifact = {
  created_at: "2026-10-04T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-shot",
  issue_key: "CORE-1",
  kind: "image",
  name: "shot.png",
  primary: false,
  project: "CORE",
  slug: "shot-png",
  versions: [1, 2].map((number) => ({
    authors: [{ id: "alice", kind: "user" as const }],
    created_at: "2026-10-04T00:00:00Z",
    named: false,
    number,
    summary: null,
  })),
};

// The dashboard is served from an origin, where a picture's same-origin bytes route resolves.
// happy-dom's page starts at `about:blank`, where no relative `src` resolves and every picture
// would fail at once, so these tests put the page on one. The test DOM is happy-dom's window
// (`__tests__/setup.ts` registers it), which TypeScript knows only as the DOM's.
const testWindow = window as unknown as HappyDOMWindow;
let pageBefore = "about:blank";

describe("Dispatch pictures", () => {
  beforeEach(() => {
    pageBefore = window.location.href;
    testWindow.happyDOM.setURL("https://dispatch.test/issues/CORE-1/conversation");
  });

  afterEach(() => {
    testWindow.happyDOM.setURL(pageBefore);
  });

  test("a Dispatch picture at a version shows inline from its version's bytes, linked to its page", async () => {
    const view = render(
      withQueries(
        <MarkdownBody
          markdown={
            "Look: ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)\n\n![](dispatch://CORE/artifact/plan-png@v3)\n\n![face.png](dispatch://agent/ses-1/artifact/face-png@v2)"
          }
        />
      )
    );

    try {
      const shot = await within(view.container).findByRole("img", { name: "shot.png" });
      expect(shot.getAttribute("src")).toBe("/api/v1/issues/CORE-1/artifacts/shot-png/versions/1");
      expect(shot.getAttribute("loading")).toBe("lazy");
      // Scaled to the column, never taller than 24rem.
      expect(shot.className.split(" ")).toEqual(
        expect.arrayContaining(["max-h-96", "max-w-full", "w-auto", "object-contain"])
      );
      expect(shot.closest("a")?.getAttribute("href")).toBe("/issues/CORE-1/artifacts/shot-png?v=1");
      // A picture with no caption is named by its slug.
      const plan = within(view.container).getByRole("img", { name: "plan-png" });
      expect(plan.getAttribute("src")).toBe("/api/v1/projects/CORE/artifacts/plan-png/versions/3");
      expect(plan.closest("a")?.getAttribute("href")).toBe(
        "/projects/CORE/documents/plan-png?version=3"
      );
      const face = within(view.container).getByRole("img", { name: "face.png" });
      expect(face.getAttribute("src")).toBe("/api/v1/agents/ses-1/artifacts/face-png/versions/2");
      expect(face.closest("a")?.getAttribute("href")).toBe("/agents/ses-1/artifacts/face-png?v=2");
    } finally {
      view.unmount();
    }
  });

  // The composer escapes a file name's brackets and backslashes so the caption stays one; a reader
  // and a screen reader still get the file's own name.
  test("an escaped caption names the picture by its file name", async () => {
    const view = render(
      withQueries(
        <MarkdownBody
          markdown={
            "![face \\[1\\] back\\\\slash.png](dispatch://agent/ses-1/artifact/face-png@v1)"
          }
        />
      )
    );

    try {
      const face = await within(view.container).findByRole("img", {
        name: "face [1] back\\slash.png",
      });
      expect(face.getAttribute("src")).toBe("/api/v1/agents/ses-1/artifacts/face-png/versions/1");
    } finally {
      view.unmount();
    }
  });

  test("the inline variant shows a Dispatch picture as a thumbnail beside its caption", async () => {
    const view = render(
      withQueries(
        <MarkdownBody
          markdown="See ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)"
          variant="inline"
        />
      )
    );

    try {
      const link = await within(view.container).findByRole("link", { name: "shot.png" });
      const thumbnail = link.querySelector("img");
      expect(thumbnail?.getAttribute("src")).toBe(
        "/api/v1/issues/CORE-1/artifacts/shot-png/versions/1"
      );
      expect(thumbnail?.className.split(" ")).toEqual(
        expect.arrayContaining(["h-10", "w-10", "object-cover"])
      );
      expect(view.container.querySelector("div")).toBeNull();
    } finally {
      view.unmount();
    }
  });

  test("a Dispatch picture that pins no version reads as its reference, with the image's thumbnail", async () => {
    const getIssue = spyOn(api, "getIssue").mockResolvedValue({
      artifacts: [picture],
      key: "CORE-1",
      title: "Core one",
    } as never);
    const view = render(
      withQueries(<MarkdownBody markdown="![shot.png](dispatch://CORE-1/artifact/shot-png)" />)
    );

    try {
      const link = await within(view.container).findByRole("link", { name: "shot.png" });
      expect(link.getAttribute("href")).toBe("/issues/CORE-1/artifacts/shot-png");
      expect(link.getAttribute("data-dispatch-ref")).toBe("dispatch://CORE-1/artifact/shot-png");
      // The plain reference's thumbnail is the latest version, in the 40 px shape.
      await waitFor(() =>
        expect(link.querySelector("img")?.getAttribute("src")).toBe(
          "/api/v1/issues/CORE-1/artifacts/shot-png/versions/2"
        )
      );
      expect(link.querySelector("img")?.className.split(" ")).toContain("h-10");
    } finally {
      getIssue.mockRestore();
      view.unmount();
    }
  });

  test("a plain reference whose thumbnail cannot load drops it and keeps the title", async () => {
    const getIssue = spyOn(api, "getIssue").mockResolvedValue({
      artifacts: [picture],
      key: "CORE-1",
      title: "Core one",
    } as never);
    // A hand-written or stale reference can pin a version the server does not serve; the
    // thumbnail's load then fails, and the reference reads as its title with no broken glyph.
    const view = render(
      withQueries(<MarkdownBody markdown="See dispatch://CORE-1/artifact/shot-png@v99" />)
    );

    try {
      const link = await within(view.container).findByRole("link", { name: "shot.png" });
      const thumbnail = await waitFor(() => {
        const image = link.querySelector("img");
        expect(image?.getAttribute("src")).toBe(
          "/api/v1/issues/CORE-1/artifacts/shot-png/versions/99"
        );
        return image as HTMLImageElement;
      });
      fireEvent.error(thumbnail);
      await waitFor(() => expect(link.querySelector("img")).toBeNull());
      expect(link.textContent).toBe("shot.png");
      expect(link.getAttribute("href")).toBe("/issues/CORE-1/artifacts/shot-png?v=99");
    } finally {
      getIssue.mockRestore();
      view.unmount();
    }
  });

  test("a plain reference to a non-image artifact shows no thumbnail", async () => {
    const getIssue = spyOn(api, "getIssue").mockResolvedValue({
      artifacts: [{ ...picture, kind: "file", name: "notes.pdf", slug: "notes-pdf" }],
      key: "CORE-1",
      title: "Core one",
    } as never);
    const view = render(
      withQueries(<MarkdownBody markdown="See dispatch://CORE-1/artifact/notes-pdf" />)
    );

    try {
      const link = await within(view.container).findByRole("link", { name: "notes.pdf" });
      expect(link.querySelector("img")).toBeNull();
    } finally {
      getIssue.mockRestore();
      view.unmount();
    }
  });

  test("a version that is no picture falls back to the reference's title", async () => {
    const getIssue = spyOn(api, "getIssue").mockResolvedValue({
      artifacts: [{ ...picture, kind: "file", name: "notes.pdf", slug: "notes-pdf" }],
      key: "CORE-1",
      title: "Core one",
    } as never);
    const view = render(
      withQueries(<MarkdownBody markdown="![notes](dispatch://CORE-1/artifact/notes-pdf@v1)" />)
    );

    try {
      const image = await within(view.container).findByRole("img", { name: "notes" });
      fireEvent.error(image);
      const link = await within(view.container).findByRole("link", { name: "notes.pdf" });
      expect(link.getAttribute("href")).toBe("/issues/CORE-1/artifacts/notes-pdf?v=1");
      expect(within(view.container).queryByRole("img")).toBeNull();
    } finally {
      getIssue.mockRestore();
      view.unmount();
    }
  });

  test("a picture on another website renders as it always has", async () => {
    const view = render(
      withQueries(<MarkdownBody markdown="![chart](https://example.com/a.png)" />)
    );

    try {
      const image = await within(view.container).findByRole("img", { name: "chart" });
      expect(image.getAttribute("src")).toBe("https://example.com/a.png");
      expect(image.closest("a")).toBeNull();
      expect(image.getAttribute("loading")).toBeNull();
    } finally {
      view.unmount();
    }
  });
});

test("renders a typed callout body through the server schema", async () => {
  const view = render(
    <MarkdownBody
      markdown={':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n'}
    />
  );

  try {
    expect(await within(view.container).findByText("Body text.")).toBeDefined();
    expect(view.container.textContent).not.toContain(":::callout");
  } finally {
    view.unmount();
  }
});

test("a code span containing tag-shaped text renders the literal characters with no backslashes", async () => {
  const view = render(<MarkdownBody markdown="Use `<img src=x>` here" />);

  try {
    const code = await within(view.container).findByText("<img src=x>", { selector: "code" });
    expect(code.textContent).toBe("<img src=x>");
  } finally {
    view.unmount();
  }
});

test("raw unsupported HTML renders as literal text with no element created", async () => {
  const view = render(<MarkdownBody markdown="<img src=x onerror=alert(1)>" />);

  try {
    expect(await within(view.container).findByText("<img src=x onerror=alert(1)>")).not.toBeNull();
    expect(view.container.querySelector("img")).toBeNull();
    expect(view.container.firstElementChild?.getAttribute("data-markdown-fallback")).toBe("true");
  } finally {
    view.unmount();
  }
});

test("a clock time such as 16:25Z renders as Markdown, not as the literal-text fallback", async () => {
  // remark-directive reads `:25Z` as a text directive; refusing the whole body for it would turn
  // every agent progress note with a timestamp into raw Markdown source and leave historical spec
  // versions empty.
  const view = render(
    <MarkdownBody markdown={"Production deploys **held** since 16:25Z.\n\n- retry at 04:20Z"} />
  );

  try {
    const strong = await within(view.container).findByText("held", { selector: "strong" });
    expect(strong).toBeDefined();
    expect(view.container.querySelector("li")?.textContent).toBe("retry at 04:20Z");
    expect(view.container.firstElementChild?.getAttribute("data-markdown-fallback")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a bare dispatch ask reference renders an inline link to the ask route with the resolved question", async () => {
  const askRead: AskRead = {
    ask: {
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      author: { id: "alice", kind: "user" },
      question: "Ship it?",
      options: [],
      multiple: false,
      urgency: "med",
      anchor: null,
      state: "open",
      answer: null,
      opened_event_id: 1,
      created_at: "2026-09-09T00:00:00Z",
      edited_at: null,
    },
    answers: [],
    edits: [],
    followers: [],
    replies: [],
  };
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MarkdownBody markdown="see dispatch://CORE-1/ask/ask-1" />
    </QueryClientProvider>
  );

  try {
    const link = await within(view.container).findByRole("link", { name: "CORE-1 ask" });
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/asks/ask-1");
    await waitFor(() => expect(link.textContent).toBe("Ship it?"));
    expect(within(view.container).getAllByRole("link")).toHaveLength(1);
    expect(getAsk).toHaveBeenCalledWith("ask-1");
  } finally {
    getAsk.mockRestore();
    view.unmount();
  }
});

test("a dispatch reference inside a code span stays literal text, not a link", async () => {
  const view = render(<MarkdownBody markdown="Use `dispatch://CORE-1` as the ref" />);

  try {
    const code = await within(view.container).findByText("dispatch://CORE-1", {
      selector: "code",
    });
    expect(code.textContent).toBe("dispatch://CORE-1");
    expect(view.container.querySelector("a")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("an external URL unrelated to the app renders as an ordinary link", async () => {
  const view = render(<MarkdownBody markdown="See https://example.com/docs for details" />);

  try {
    const link = await within(view.container).findByRole("link", {
      name: "https://example.com/docs",
    });
    expect(link.getAttribute("href")).toBe("https://example.com/docs");
    expect(link.hasAttribute("data-dispatch-ref")).toBe(false);
  } finally {
    view.unmount();
  }
});

test("two references in one body each resolve their own inline link and title", async () => {
  const askRead: AskRead = {
    ask: {
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      author: { id: "alice", kind: "user" },
      question: "Ship it?",
      options: [],
      multiple: false,
      urgency: "med",
      anchor: null,
      state: "open",
      answer: null,
      opened_event_id: 1,
      created_at: "2026-09-09T00:00:00Z",
      edited_at: null,
    },
    answers: [],
    edits: [],
    followers: [],
    replies: [],
  };
  const projectArtifact: Artifact = {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: null,
    kind: "doc",
    name: "Runbook.md",
    primary: false,
    project: "CORE",
    ref_key: "CORE/runbook-md",
    slug: "runbook-md",
    versions: [],
  };
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead);
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockResolvedValue(projectArtifact);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MarkdownBody markdown="Compare dispatch://CORE-1/ask/ask-1 with dispatch://CORE/artifact/runbook-md" />
    </QueryClientProvider>
  );

  try {
    const askLink = await within(view.container).findByRole("link", { name: "Ship it?" });
    const documentLink = await within(view.container).findByRole("link", { name: "Runbook.md" });
    expect(askLink.getAttribute("href")).toBe("/issues/CORE-1/asks/ask-1");
    expect(documentLink.getAttribute("href")).toBe("/projects/CORE/documents/runbook-md");
    expect(within(view.container).getAllByRole("link")).toHaveLength(2);
  } finally {
    getProjectArtifact.mockRestore();
    getAsk.mockRestore();
    view.unmount();
  }
});

test("the inline variant drops a later paragraph's code span instead of leaking it as a bare linkifiable ref", async () => {
  const view = render(
    <MarkdownBody
      markdown={"First line.\n\nSecond `dispatch://CORE-1` paragraph."}
      variant="inline"
    />
  );

  try {
    await waitFor(() => expect(view.container.textContent).toContain("First line."));
    expect(view.container.textContent).not.toContain("dispatch://CORE-1");
    expect(view.container.querySelector("a")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a hand-authored Markdown link to a dispatch:// target resolves through data-dispatch-href", async () => {
  const askRead: AskRead = {
    ask: {
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      author: { id: "alice", kind: "user" },
      question: "Ship it?",
      options: [],
      multiple: false,
      urgency: "med",
      anchor: null,
      state: "open",
      answer: null,
      opened_event_id: 1,
      created_at: "2026-09-09T00:00:00Z",
      edited_at: null,
    },
    answers: [],
    edits: [],
    followers: [],
    replies: [],
  };
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // @legion/proof-editor's Markdown link serializer sanitizes a dispatch:// href to "" (Milkdown
  // only allows http/https/mailto/tel/ftp) and carries the real target in data-dispatch-href
  // instead; this goes through the real parseMarkdown + DOMSerializer pipeline, not an injected
  // anchor, to prove that attribute is what MarkdownBody actually resolves against.
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MarkdownBody markdown="see [the ask](dispatch://CORE-1/ask/ask-1) for details" />
    </QueryClientProvider>
  );

  try {
    const link = await within(view.container).findByRole("link", { name: "Ship it?" });
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/asks/ask-1");
    expect(link.getAttribute("data-dispatch-ref")).toBe("dispatch://CORE-1/ask/ask-1");
    expect(getAsk).toHaveBeenCalledWith("ask-1");
  } finally {
    getAsk.mockRestore();
    view.unmount();
  }
});

// The live view hands each streamed delta to the same body as a longer `markdown`: the body
// re-parses it, so syntax that has closed since the last delta formats, and nothing stays frozen
// at the first delta's text.
test("a body re-renders when its text grows, formatting what has closed since", async () => {
  const view = render(<MarkdownBody markdown="**Bo" />);
  try {
    await waitFor(() => expect(view.container.textContent).toBe("**Bo"));
    view.rerender(<MarkdownBody markdown={"**Bold** so far\n\n- first\n- sec"} />);
    await waitFor(() => expect(view.container.querySelector("strong")?.textContent).toBe("Bold"));
    expect(Array.from(view.container.querySelectorAll("li"), (item) => item.textContent)).toEqual([
      "first",
      "sec",
    ]);
    expect(view.container.textContent).not.toContain("**");
    view.rerender(<MarkdownBody markdown={"**Bold** so far\n\n- first\n- second"} />);
    await waitFor(() =>
      expect(view.container.querySelectorAll("li")[1]?.textContent).toBe("second")
    );
  } finally {
    view.unmount();
  }
});

// ELK, which lays out flowcharts, cannot be built under happy-dom, so these draw sequence
// diagrams; a dangling edge is refused in validation, before any layout.
describe("Mermaid diagrams", () => {
  // The loaded renderer and the drawn-source cache are module state that outlives every render,
  // so each case starts from a page that has loaded and drawn nothing.
  beforeEach(resetMermaidDiagrams);

  const fence = (source: string): string => `\`\`\`mermaid\n${source}\n\`\`\`\n`;
  const figuresIn = (container: HTMLElement): HTMLElement[] =>
    Array.from(container.querySelectorAll<HTMLElement>("figure[data-mermaid-diagram]"));
  const diagramIn = (figure: Element): Element | null | undefined =>
    figure
      .querySelector(".dispatch-mermaid-preview")
      ?.shadowRoot?.querySelector('svg[role="img"][aria-label="Rendered Mermaid diagram"]');

  test("a mermaid block draws its diagram in the preview's shadow root, its source closed behind Mermaid source", async () => {
    const view = render(
      <MarkdownBody markdown={`Flow:\n\n${fence("sequenceDiagram\n  Alice->>Bob: Hi")}`} />
    );
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      const [figure] = figuresIn(view.container);
      const diagram = diagramIn(figure);
      expect(diagram?.textContent).toContain("Alice");
      expect(diagram?.textContent).toContain("Hi");
      expect(figure.querySelector('[role="status"]')).toBeNull();
      const source = figure.querySelector<HTMLDetailsElement>("details.dispatch-mermaid-source");
      expect(source?.open).toBe(false);
      expect(source?.querySelector("summary")?.textContent).toBe("Mermaid source");
      expect(source?.querySelector("pre code")?.textContent).toContain("Alice->>Bob: Hi");
      expect(view.container.textContent).toContain("Flow:");
    } finally {
      view.unmount();
    }
  });

  test("a source the renderer refuses shows the reason and its source open, and no diagram", async () => {
    const view = render(<MarkdownBody markdown={`Broken:\n\n${fence("graph TD\n  A -->")}`} />);
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      const [figure] = figuresIn(view.container);
      expect(figure.querySelector('[role="status"]')?.textContent).toMatch(
        /^Unable to render Mermaid diagram: Dangling Mermaid edge/
      );
      expect(figure.querySelector(".dispatch-mermaid-preview")).toBeNull();
      const source = figure.querySelector<HTMLDetailsElement>("details.dispatch-mermaid-source");
      expect(source?.open).toBe(true);
      expect(source?.querySelector("pre code")?.textContent).toContain("A -->");
    } finally {
      view.unmount();
    }
  });

  test("a dispatch reference in a diagram's label is neither linked nor resolved", async () => {
    const view = render(
      withQueries(
        <MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: see dispatch://CORE-1")} />
      )
    );
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      const diagram = diagramIn(figuresIn(view.container)[0]);
      expect(diagram?.textContent).toContain("dispatch://CORE-1");
      expect(diagram?.querySelector("a")).toBeNull();
      expect(view.container.querySelector("a")).toBeNull();
      expect(within(view.container).queryAllByRole("link")).toHaveLength(0);
    } finally {
      view.unmount();
    }
  });

  // The live view re-parses a streamed turn on every frame: a diagram whose fence has not closed
  // yet stays code, and the diagrams before it in the same body draw.
  test("a body ending in an unclosed mermaid fence keeps it as code until the fence closes", async () => {
    const done = fence("sequenceDiagram\n  Alice->>Bob: Hi");
    const streaming = "```mermaid\nsequenceDiagram\n  Carol->>Dave: Yo";
    const view = render(<MarkdownBody markdown={`${done}\n${streaming}`} />);
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      expect(diagramIn(figuresIn(view.container)[0])?.textContent).toContain("Alice");
      const open = view.container.querySelectorAll("pre");
      expect(open).toHaveLength(2);
      const code = open[1];
      expect(code.textContent).toContain("Carol->>Dave: Yo");
      expect(code.closest("figure")).toBeNull();
      view.rerender(<MarkdownBody markdown={`${done}\n${streaming}\n\`\`\`\n`} />);
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(2));
      expect(diagramIn(figuresIn(view.container)[1])?.textContent).toContain("Carol");
    } finally {
      view.unmount();
    }
  });

  // A link reference definition is no block of the parse, so the diagram is the document's last
  // block while the source's last line is the definition, not the diagram's closing fence. The
  // fence did close, so the diagram is finished and draws.
  test("a closed mermaid fence followed by a link reference definition draws its diagram", async () => {
    const view = render(
      <MarkdownBody
        markdown={`${fence("sequenceDiagram\n  Alice->>Bob: Hi")}\n[spec]: https://example.com/spec\n`}
      />
    );
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      expect(diagramIn(figuresIn(view.container)[0])?.textContent).toContain("Alice");
    } finally {
      view.unmount();
    }
  });

  // CommonMark closes a fence only with a line of the opener's character, at least as long as the
  // opener. A streamed turn's last line that would close some other fence (three backticks under
  // a four-backtick opener, tildes under a backtick one) leaves the block open, so it stays code.
  test("a body ending in a line that cannot close its mermaid fence keeps it as code", async () => {
    const done = fence("sequenceDiagram\n  Alice->>Bob: Hi");
    for (const streaming of [
      "````mermaid\nsequenceDiagram\n  Carol->>Dave: Yo\n```",
      "```mermaid\nsequenceDiagram\n  Carol->>Dave: Yo\n~~~",
    ]) {
      resetMermaidDiagrams();
      const view = render(<MarkdownBody markdown={`${done}\n${streaming}`} />);
      try {
        await waitFor(() => expect(figuresIn(view.container).length).toBeGreaterThan(0));
        expect(figuresIn(view.container)).toHaveLength(1);
        const code = view.container.querySelectorAll("pre")[1];
        expect(code?.textContent).toContain("Carol->>Dave: Yo");
        expect(code?.closest("figure")).toBeNull();
      } finally {
        view.unmount();
      }
    }
  });

  // A fence opened inside a quote or a list item is still open while its quote or item runs to
  // the end of the body, so a diagram streamed there stays code too; closed inside its container,
  // it draws.
  test("a mermaid fence still open inside a quote or a list item keeps it as code", async () => {
    const done = fence("sequenceDiagram\n  Alice->>Bob: Hi");
    for (const [streaming, closing] of [
      ["> ```mermaid\n> sequenceDiagram\n>   Carol->>Dave: Yo", "\n> ```\n"],
      ["- ```mermaid\n  sequenceDiagram\n    Carol->>Dave: Yo", "\n  ```\n"],
    ] as const) {
      resetMermaidDiagrams();
      const view = render(<MarkdownBody markdown={`${done}\n${streaming}`} />);
      try {
        await waitFor(() => expect(figuresIn(view.container).length).toBeGreaterThan(0));
        expect(figuresIn(view.container)).toHaveLength(1);
        const code = view.container.querySelectorAll("pre")[1];
        expect(code?.textContent).toContain("Carol->>Dave: Yo");
        expect(code?.closest("figure")).toBeNull();
        view.rerender(<MarkdownBody markdown={`${done}\n${streaming}${closing}`} />);
        await waitFor(() => expect(figuresIn(view.container)).toHaveLength(2));
      } finally {
        view.unmount();
      }
    }
  });

  test("a body whose source changes while the renderer loads draws only the newest source", async () => {
    // With the engine loaded the body paints in its layout effect, so only the renderer is late.
    await warmMarkdownRenderer();
    const draw = spyOn(mermaidRenderer, "renderMermaidSvg");
    const view = render(<MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: First")} />);
    // A body unmounted while the renderer loads keeps its code block in its own element, which
    // only the cancel stops the late draw from reaching.
    const gone = render(<MarkdownBody markdown={fence("sequenceDiagram\n  Erin->>Finn: Gone")} />);
    try {
      expect(view.container.querySelector("pre code")?.textContent).toContain("First");
      expect(figuresIn(view.container)).toHaveLength(0);
      gone.unmount();
      view.rerender(<MarkdownBody markdown={fence("sequenceDiagram\n  Carol->>Dave: Second")} />);
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      expect(diagramIn(figuresIn(view.container)[0])?.textContent).toContain("Second");
      expect(draw).toHaveBeenCalledTimes(1);
      expect(draw.mock.calls[0]?.[0]).toContain("Carol->>Dave: Second");
    } finally {
      draw.mockRestore();
      view.unmount();
    }
  });
});
