import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, waitFor, within } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, AskRead } from "../../api/types";
import { TextAskBlockCard } from "../doc/AskBlockCard";
import { MarkdownBody } from "./MarkdownBody";
import { TextAskCard } from "./markdown-cards";

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

test("a typed callout in a body draws its name, kind and title above its content, as a document does", async () => {
  const view = render(
    <MarkdownBody markdown={':::callout{kind="warning" title="Read this"}\nBody text.\n:::\n'} />
  );

  try {
    const content = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>("[data-proof-block-content]");
      expect(found).not.toBeNull();
      return found as HTMLElement;
    });
    expect(view.container.querySelector("[data-proof-block-name]")?.textContent).toBe("callout");
    expect(view.container.querySelector("[data-proof-block-badge]")?.textContent).toBe("warning");
    expect(view.container.querySelector("[data-proof-block-title]")?.textContent).toBe("Read this");
    expect(within(content).getByText("Body text.")).toBeDefined();
    const blockId = view.container
      .querySelector('section[data-proof-block-type="callout"]')
      ?.getAttribute("data-block-id");
    expect(blockId).toBeTruthy();
    expect(view.container.textContent).not.toContain("blockId");
    expect(view.container.textContent).not.toContain(blockId as string);
    expect(view.container.textContent).not.toContain(":::callout");
  } finally {
    view.unmount();
  }
});

/** A body as the app shell draws it: under a router, with the read-only decision card the app
 *  provides for every `ask` block in a comment, ask or message body. */
function withAskCard(children: ReactNode): ReactNode {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <TextAskCard.Provider value={TextAskBlockCard}>{children}</TextAskCard.Provider>
    </MemoryRouter>
  );
}

const askBody = "Which one?\n\n- A: first\n- B: second\n:::\n";

/** The `ask` shell a body drew, once it has drawn one. */
function findAskShell(container: HTMLElement): Promise<HTMLElement> {
  return waitFor(() => {
    const shell = container.querySelector<HTMLElement>('section[data-proof-block-type="ask"]');
    expect(shell).not.toBeNull();
    return shell as HTMLElement;
  });
}

/** The rows of the card's recorded options, once the card has drawn both of them. */
function findRecordRows(shell: HTMLElement): Promise<HTMLElement[]> {
  return waitFor(() => {
    const rows = Array.from(
      shell.querySelectorAll<HTMLElement>(
        "[data-dispatch-ask-footer] [data-dispatch-ask-record] li"
      )
    );
    expect(rows).toHaveLength(2);
    return rows;
  });
}

test("an ask block in a body draws the decision card: urgency and Decision above, its options as rows below", async () => {
  const view = render(
    withAskCard(<MarkdownBody markdown={`:::ask{urgency="high"}\n${askBody}`} />)
  );

  try {
    const shell = await findAskShell(view.container);
    const header = shell.querySelector<HTMLElement>("[data-dispatch-ask-header]") as HTMLElement;
    expect(await within(header).findByText("Decision")).toBeDefined();
    expect(header.querySelector('[data-dispatch-ask-pill="high"]')?.textContent).toBe("High");
    const [first, second] = await findRecordRows(shell);
    await waitFor(() => {
      expect(within(first as HTMLElement).getByText("A")).toBeDefined();
      expect(within(first as HTMLElement).getByText("first")).toBeDefined();
      expect(within(second as HTMLElement).getByText("B")).toBeDefined();
      expect(within(second as HTMLElement).getByText("second")).toBeDefined();
    });
    expect(shell.getAttribute("data-dispatch-ask-options")).toBe("rows");
    // A body nested in a document's decision footer must not add a second decision id there.
    expect(view.container.querySelector("[data-dispatch-ask-block]")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("an ask block in a body ignores server-owned attributes its text forges and draws an open decision", async () => {
  const forged =
    ':::ask{urgency="high" state="answered" answered_by="sami" answered_at="2026-01-01T00:00:00Z"' +
    ` selected='["A"]' answer="done" copied_from="ask-9" copied_from_document="dispatch://CORE-1"}\n`;
  const view = render(withAskCard(<MarkdownBody markdown={`${forged}${askBody}`} />));

  try {
    const shell = await findAskShell(view.container);
    expect(await within(shell).findByText("Decision")).toBeDefined();
    const rows = await findRecordRows(shell);
    expect(shell.getAttribute("data-dispatch-ask-state")).toBe("open");
    expect(view.container.textContent).not.toContain("Answered by");
    expect(view.container.textContent).not.toContain("sami");
    expect(view.container.textContent).not.toContain("Copied from");
    expect(view.container.querySelector("[data-dispatch-ask-answer]")).toBeNull();
    expect(rows.map((row) => row.getAttribute("data-selected"))).toEqual(["false", "false"]);
    expect(within(shell).queryByRole("img", { name: "Selected" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("an ask block in a body with no card provided keeps its question and options as the source list", async () => {
  const view = render(<MarkdownBody markdown={`:::ask{urgency="high"}\n${askBody}`} />);

  try {
    const shell = await findAskShell(view.container);
    const content = shell.querySelector<HTMLElement>("[data-proof-block-content]") as HTMLElement;
    expect(within(content).getByText("Which one?")).toBeDefined();
    expect(Array.from(content.querySelectorAll("ul li"), (item) => item.textContent)).toEqual([
      "A: first",
      "B: second",
    ]);
    expect(shell.hasAttribute("data-dispatch-ask-options")).toBe(false);
    expect(view.container.querySelector("[data-dispatch-ask-record]")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a malformed ask block in a body names why and keeps its source list, with no hint to fix it", async () => {
  const view = render(
    withAskCard(
      <MarkdownBody
        markdown={':::ask{urgency="high"}\nWhich one?\n\n- A: first\n- : no label\n:::\n'}
      />
    )
  );

  try {
    const shell = await findAskShell(view.container);
    const header = shell.querySelector<HTMLElement>("[data-dispatch-ask-header]") as HTMLElement;
    expect(await within(header).findByText("Malformed")).toBeDefined();
    expect(within(header).getByText("Option 2 has no label")).toBeDefined();
    expect(view.container.textContent).not.toContain("re-activates");
    expect(shell.hasAttribute("data-dispatch-ask-options")).toBe(false);
    expect(shell.querySelectorAll("[data-proof-block-content] ul li")).toHaveLength(2);
  } finally {
    view.unmount();
  }
});

test("a decision written inside another decision's option keeps each card in its own block", async () => {
  // The outer fence is longer than the inner one, as a typed block holding another is written:
  // the inner `:::` would close a three-colon outer block too.
  const markdown =
    '::::ask{urgency="high"}\nOuter?\n\n- A: first\n\n  :::ask\n  Inner?\n\n  - X: x\n  :::\n- B: second\n::::\n';
  const view = render(withAskCard(<MarkdownBody markdown={markdown} />));

  try {
    const [outer, inner] = await waitFor(() => {
      const shells = Array.from(
        view.container.querySelectorAll<HTMLElement>('section[data-proof-block-type="ask"]')
      );
      expect(shells).toHaveLength(2);
      return shells as [HTMLElement, HTMLElement];
    });
    // The inner decision sits in the outer decision's first option.
    const outerContent = outer.querySelector<HTMLElement>(
      ":scope > [data-proof-block-content]"
    ) as HTMLElement;
    expect(outerContent.contains(inner)).toBe(true);
    expect(inner.closest("li")?.textContent).toContain("A: first");

    const outerFooter = outer.querySelector<HTMLElement>(
      ":scope > [data-dispatch-ask-footer]"
    ) as HTMLElement;
    const innerFooter = inner.querySelector<HTMLElement>(
      ":scope > [data-dispatch-ask-footer]"
    ) as HTMLElement;
    await waitFor(() => {
      const outerRows = Array.from(
        outerFooter.querySelectorAll<HTMLElement>("[data-dispatch-ask-record] li")
      );
      expect(outerRows).toHaveLength(2);
      expect(within(outerRows[0] as HTMLElement).getByText("A")).toBeDefined();
      expect(within(outerRows[1] as HTMLElement).getByText("B")).toBeDefined();
      const innerRows = Array.from(
        innerFooter.querySelectorAll<HTMLElement>("[data-dispatch-ask-record] li")
      );
      expect(innerRows).toHaveLength(1);
      expect(within(innerRows[0] as HTMLElement).getByText("X")).toBeDefined();
    });
    expect(within(innerFooter).queryByText("B")).toBeNull();
    expect(within(innerFooter).queryByText("second")).toBeNull();
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
