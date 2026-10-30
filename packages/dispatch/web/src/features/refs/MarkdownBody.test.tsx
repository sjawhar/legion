import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, waitFor, within } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type {
  AskRead,
  IssueReferenceSummary,
  IssueReferenceSummaryDocument,
} from "../../api/types";
import { TextAskBlockCard } from "../doc/AskBlockCard";
import { MarkdownBody } from "./MarkdownBody";
import { TextAskCard } from "./markdown-cards";
import { markdownWorkerThreshold, setParseDeadline, warmMarkdownRenderer } from "./markdown-engine";
import { flushTasks, installFakeParseWorker, restoreParseWorker } from "./parse-worker-fixture";

function withQueries(children: ReactNode): ReactNode {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

/** An image document of CORE-1 whose latest version is 2, as the issue summary lists it. */
const picture: IssueReferenceSummaryDocument = {
  id: "artifact-shot",
  kind: "image",
  latest_version: 2,
  name: "shot.png",
  primary: false,
  slug: "shot-png",
};

/** CORE-1's summary read, the one read behind a reference to it or to its documents. */
function issueSummary(documents: IssueReferenceSummaryDocument[]): IssueReferenceSummary {
  return { documents, key: "CORE-1", project: "CORE", status: "in_progress", title: "Core one" };
}

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
    const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(
      issueSummary([picture])
    );
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
      getIssueSummary.mockRestore();
      view.unmount();
    }
  });

  test("a plain reference whose thumbnail cannot load drops it and keeps the title", async () => {
    const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(
      issueSummary([picture])
    );
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
      getIssueSummary.mockRestore();
      view.unmount();
    }
  });

  test("a plain reference to a non-image artifact shows no thumbnail", async () => {
    const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(
      issueSummary([{ ...picture, kind: "file", name: "notes.pdf", slug: "notes-pdf" }])
    );
    const view = render(
      withQueries(<MarkdownBody markdown="See dispatch://CORE-1/artifact/notes-pdf" />)
    );

    try {
      const link = await within(view.container).findByRole("link", { name: "notes.pdf" });
      expect(link.querySelector("img")).toBeNull();
    } finally {
      getIssueSummary.mockRestore();
      view.unmount();
    }
  });

  test("a version that is no picture falls back to the reference's title", async () => {
    const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(
      issueSummary([{ ...picture, kind: "file", name: "notes.pdf", slug: "notes-pdf" }])
    );
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
      getIssueSummary.mockRestore();
      view.unmount();
    }
  });

  // A picture's source and a link's target are nodes an author named explicitly, so a document
  // whose name is past the bound text is held to (2683 characters, which a new upload's reference
  // key still holds) is drawn from its version's bytes and opened by its page, not left a
  // `dispatch://` source the browser cannot fetch or a dead link.
  test("a long-named picture is drawn and an explicit link to a long-named document opens it", async () => {
    const slug = "a".repeat(2683);
    const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(
      issueSummary([{ ...picture, kind: "doc", name: "design.md", slug }])
    );
    const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "The design.",
      token: "sha256:design",
      version: 1,
    });
    const view = render(
      withQueries(
        <MarkdownBody
          markdown={`![shot](dispatch://CORE-1/artifact/${slug}@v1)\n\nSee [design](dispatch://CORE-1/artifact/${slug}).`}
        />
      )
    );

    try {
      const shot = await within(view.container).findByRole("img", { name: "shot" });
      expect(shot.getAttribute("src")).toBe(`/api/v1/issues/CORE-1/artifacts/${slug}/versions/1`);
      expect(shot.closest("a")?.getAttribute("href")).toBe(`/issues/CORE-1/artifacts/${slug}?v=1`);
      const link = await within(view.container).findByRole("link", { name: "design.md" });
      expect(link.getAttribute("href")).toBe(`/issues/CORE-1/artifacts/${slug}`);
      expect(link.getAttribute("data-dispatch-ref")).toBe(`dispatch://CORE-1/artifact/${slug}`);
    } finally {
      getArtifactText.mockRestore();
      getIssueSummary.mockRestore();
      view.unmount();
    }
  });

  // A URL left as text is text to the server's `text.Extract`, which cites a dashboard page only
  // within the bounds the graph keys: a document past them stays the URL it is, and a block past
  // them (257 bytes) is its document. A link its author wrote names its target, so it opens the
  // same long-named document.
  test("a bare dashboard URL past the bounds stays a plain link; an author's link to it still opens the document", async () => {
    const slug = "a".repeat(2683);
    const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(
      issueSummary([
        { ...picture, kind: "doc", name: "design.md", slug },
        { ...picture, id: "artifact-plan", kind: "doc", name: "plan.md", slug: "plan-md" },
      ])
    );
    const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "The design.",
      token: "sha256:design",
      version: 1,
    });
    const page = `https://dispatch.test/issues/CORE-1/artifacts/${slug}`;
    const plan = "https://dispatch.test/issues/CORE-1/artifacts/plan-md";
    const view = render(
      withQueries(
        <MarkdownBody
          markdown={`Bare: ${page}\n\nSee [design](${page}) and [design](dispatch://CORE-1/artifact/${slug}).\n\nPlan: ${plan} and ${plan}#b-${"b".repeat(257)}`}
        />
      )
    );

    try {
      const designs = await within(view.container).findAllByRole("link", { name: "design.md" });
      expect(designs).toHaveLength(2);
      for (const link of designs) {
        expect(link.getAttribute("href")).toBe(`/issues/CORE-1/artifacts/${slug}`);
        expect(link.getAttribute("data-dispatch-ref")).toBe(`dispatch://CORE-1/artifact/${slug}`);
      }
      const plans = await within(view.container).findAllByRole("link", { name: "plan.md" });
      expect(plans).toHaveLength(2);
      for (const link of plans) {
        expect(link.getAttribute("href")).toBe("/issues/CORE-1/artifacts/plan-md");
        expect(link.getAttribute("data-dispatch-ref")).toBe("dispatch://CORE-1/artifact/plan-md");
      }
      const bare = within(view.container).getByRole("link", { name: page });
      expect(bare.getAttribute("href")).toBe(page);
      expect(bare.getAttribute("data-dispatch-ref")).toBeNull();
    } finally {
      getArtifactText.mockRestore();
      getIssueSummary.mockRestore();
      view.unmount();
    }
  });

  // The `dispatch://` forms of a URL left as text: an angle-bracket autolink, and a Markdown link
  // whose words are its own target, the form a document stores one in. Proof's serializer moves
  // their target into `data-dispatch-href`; they are text to the server all the same, so past the
  // bounds they stay plain, and within them they are references. A `dispatch://` link its author
  // gave words of its own still opens the long-named document.
  test("a dispatch:// URL left as text past the bounds stays plain; an author's dispatch:// link with its own words still opens", async () => {
    const slug = "a".repeat(2683);
    const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(
      issueSummary([
        { ...picture, kind: "doc", name: "design.md", slug },
        { ...picture, id: "artifact-plan", kind: "doc", name: "plan.md", slug: "plan-md" },
      ])
    );
    const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "The design.",
      token: "sha256:design",
      version: 1,
    });
    const long = `dispatch://CORE-1/artifact/${slug}`;
    const plan = "dispatch://CORE-1/artifact/plan-md";
    const block = `${plan}#b-${"b".repeat(257)}`;
    const view = render(
      withQueries(
        <MarkdownBody
          markdown={`Angle: <${long}>\n\nSelf: [${long}](${long})\n\nBlock: <${block}>\n\nSee [design](${long}).\n\nPlan: <${plan}> and [${plan}](${plan})`}
        />
      )
    );

    try {
      const designs = await within(view.container).findAllByRole("link", { name: "design.md" });
      expect(designs).toHaveLength(1);
      expect(designs[0]?.getAttribute("href")).toBe(`/issues/CORE-1/artifacts/${slug}`);
      expect(designs[0]?.getAttribute("data-dispatch-ref")).toBe(long);
      const plans = await within(view.container).findAllByRole("link", { name: "plan.md" });
      expect(plans).toHaveLength(2);
      for (const link of plans) {
        expect(link.getAttribute("data-dispatch-ref")).toBe(plan);
      }
      // Past the bounds each is its words as plain text: no link to click, and no
      // `data-dispatch-href` for the hover card (`RefPreview`'s trigger) to find.
      for (const [label, words] of [
        ["Angle:", long],
        ["Self:", long],
        ["Block:", block],
      ] as const) {
        const paragraph = within(view.container).getByText(label, { exact: false });
        expect(paragraph.textContent).toBe(`${label} ${words}`);
        expect(paragraph.querySelector("a")).toBeNull();
        expect(paragraph.querySelector("[data-dispatch-href],[data-dispatch-ref]")).toBeNull();
      }
    } finally {
      getArtifactText.mockRestore();
      getIssueSummary.mockRestore();
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

test("a container directive the schema refuses shows its fence lines and the body still formats", async () => {
  const view = render(
    <MarkdownBody markdown={"Some **bold** text\n\n:::note\nA note line\n:::\n"} />
  );

  try {
    expect(await within(view.container).findByText("bold", { selector: "strong" })).toBeDefined();
    const paragraphs = Array.from(view.container.querySelectorAll("p"), (p) => p.textContent);
    expect(paragraphs).toEqual(["Some bold text", ":::note", "A note line", ":::"]);
    expect(view.container.querySelector("[data-markdown-fallback]")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a decision the read-only parse refuses leaves the valid decision beside it its own card", async () => {
  // A heading breaks an ask's content rule, so the read-only parse draws that block as its fence
  // lines around its content and makes no `ask` node of it; the card must still find the valid
  // decision's node, whichever side of the refused block it sits on.
  const refused = ":::ask\n## Heading\n\nWhich?\n\n- X: x\n:::\n";
  const valid = `:::ask{urgency="high"}\n${askBody}`;
  for (const markdown of [`${refused}\n${valid}`, `${valid}\n${refused}`]) {
    const view = render(withAskCard(<MarkdownBody markdown={markdown} />));

    try {
      const shell = await findAskShell(view.container);
      const shells = view.container.querySelectorAll('section[data-proof-block-type="ask"]');
      expect(shells).toHaveLength(1);
      // The refused block reads as its source lines, outside the one shell.
      const fence = within(view.container).getByText(":::ask", { selector: "p" });
      expect(shell.contains(fence)).toBe(false);
      const heading = within(view.container).getByText("Heading", { selector: "h2" });
      expect(heading.closest("section")).toBeNull();

      const header = shell.querySelector<HTMLElement>(
        ":scope > [data-dispatch-ask-header]"
      ) as HTMLElement;
      expect(await within(header).findByText("Decision")).toBeDefined();
      expect(header.querySelector('[data-dispatch-ask-pill="high"]')?.textContent).toBe("High");
      const footer = shell.querySelector<HTMLElement>(
        ":scope > [data-dispatch-ask-footer]"
      ) as HTMLElement;
      await waitFor(() => {
        const rows = Array.from(
          footer.querySelectorAll<HTMLElement>("[data-dispatch-ask-record] li")
        );
        expect(rows).toHaveLength(2);
        expect(within(rows[0] as HTMLElement).getByText("A")).toBeDefined();
        expect(within(rows[1] as HTMLElement).getByText("B")).toBeDefined();
      });
      expect(shell.getAttribute("data-dispatch-ask-options")).toBe("rows");
      expect(within(footer).queryByText("X")).toBeNull();
      expect(view.container.querySelector("[data-markdown-fallback]")).toBeNull();
    } finally {
      view.unmount();
    }
  }
});

test("a Pandoc fenced div reads as text and the body still formats", async () => {
  const view = render(<MarkdownBody markdown={"Some **bold** text\n\n::: warning\nBody\n:::\n"} />);

  try {
    expect(await within(view.container).findByText("bold", { selector: "strong" })).toBeDefined();
    expect(view.container.textContent).toContain("::: warning");
    expect(view.container.querySelector("[data-markdown-fallback]")).toBeNull();
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
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
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

test("a block reference reads as the headline of its text: the pinned version's, opening that version, or the current one, opening the block", async () => {
  // The pinned link's path names the spec's slug, which the issue's summary carries: no link to
  // a block reads the full issue.
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(
    issueSummary([
      { ...picture, id: "spec", kind: "doc", name: "Design.md", primary: true, slug: "design-md" },
    ])
  );
  const getIssue = spyOn(api, "getIssue");
  const node = {
    block_id: "p1",
    changed_version: 3,
    document: "dispatch://CORE-1/spec",
    id: "CORE-1/spec#p1",
    issue_key: "CORE-1",
    kind: "block" as const,
    project: "CORE",
    ref: "dispatch://CORE-1/spec#b-p1",
    text: "Ship it on **Monday**.",
  };
  const getReferences = spyOn(api, "getReferences").mockImplementation((reference) =>
    Promise.resolve(
      reference.endsWith("@v2")
        ? {
            edges: [],
            node: {
              ...node,
              pin: { ref: reference, state: "changed", text: "Ship it on Friday.", version: 2 },
            },
          }
        : { edges: [], node }
    )
  );
  const view = render(
    withQueries(
      <MarkdownBody markdown="Now dispatch://CORE-1/spec#b-p1 and then dispatch://CORE-1/spec#b-p1@v2." />
    )
  );

  try {
    const current = await within(view.container).findByRole("link", { name: "Ship it on Monday." });
    expect(current.getAttribute("href")).toBe("/issues/CORE-1/spec#b-p1");
    const pinned = await within(view.container).findByRole("link", { name: "Ship it on Friday." });
    await waitFor(() =>
      expect(pinned.getAttribute("href")).toBe("/issues/CORE-1/artifacts/design-md?v=2")
    );
    expect(pinned.getAttribute("data-dispatch-ref")).toBe("dispatch://CORE-1/spec#b-p1@v2");
    expect(getIssue).not.toHaveBeenCalled();
  } finally {
    getReferences.mockRestore();
    getIssue.mockRestore();
    getIssueSummary.mockRestore();
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

test("two references in one body each resolve their own inline link and title, reading only what each shows", async () => {
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
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    replies: [],
  };
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead);
  const getProjectDocumentSummary = spyOn(api, "getProjectDocumentSummary").mockResolvedValue({
    id: "artifact-1",
    kind: "doc",
    latest_version: 4,
    name: "Runbook.md",
    project: "CORE",
    slug: "runbook-md",
  });
  const getIssue = spyOn(api, "getIssue");
  const getIssueSummary = spyOn(api, "getIssueSummary");
  const getArtifactText = spyOn(api, "getArtifactText");
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
    expect(getProjectDocumentSummary).toHaveBeenCalledWith("CORE", "runbook-md");
    // An ask link reads its ask alone, and an inline document link shows no text, so reads none.
    expect(getIssue).not.toHaveBeenCalled();
    expect(getIssueSummary).not.toHaveBeenCalled();
    expect(getArtifactText).not.toHaveBeenCalled();
  } finally {
    getArtifactText.mockRestore();
    getIssueSummary.mockRestore();
    getIssue.mockRestore();
    getProjectDocumentSummary.mockRestore();
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
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
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

// Each list is parted from the next by a plain paragraph, so each parses as its own list.
test("a task item draws one disabled checkbox, ticked as its source is, in every list shape", async () => {
  const view = render(
    <MarkdownBody
      markdown={[
        "- [x] done\n- [ ] todo",
        "Loose:",
        "- [x] loose\n\n  second para",
        "Ordered:",
        "1. [x] one\n2. [ ] two",
        "Nested:",
        "- [x] parent\n  - [ ] child",
        "Mixed:",
        "- plain\n- [x] mixed",
      ].join("\n\n")}
    />
  );

  try {
    const tasks = await waitFor(() => {
      const found = Array.from(
        view.container.querySelectorAll<HTMLLIElement>('li[data-item-type="task"]')
      );
      expect(found).toHaveLength(8);
      return found;
    });
    const taskStartingWith = (text: string): HTMLLIElement => {
      const item = tasks.find((task) => task.textContent?.startsWith(text));
      if (item === undefined) {
        throw new Error(`No task item starts with "${text}".`);
      }
      return item;
    };
    // A box belongs to the item it sits closest to, so a parent's count leaves out its child's.
    const ownBoxes = (item: HTMLLIElement): HTMLInputElement[] =>
      Array.from(item.querySelectorAll<HTMLInputElement>("input")).filter(
        (box) => box.closest("li") === item
      );

    for (const item of tasks) {
      const boxes = ownBoxes(item);
      expect(boxes).toHaveLength(1);
      const [box] = boxes;
      expect(box?.type).toBe("checkbox");
      expect(box?.disabled).toBe(true);
      expect(box?.checked).toBe(item.getAttribute("data-checked") === "true");
    }
    expect(tasks.map((item) => ownBoxes(item)[0]?.checked)).toEqual([
      true,
      false,
      true,
      true,
      false,
      true,
      false,
      true,
    ]);

    const plain = Array.from(view.container.querySelectorAll("li")).find(
      (item) => item.textContent === "plain"
    );
    expect(plain?.hasAttribute("data-item-type")).toBe(false);
    expect(plain?.querySelector("input")).toBeNull();

    // A tight item's paragraph is unwrapped with its box in it: box and words sit in the item.
    for (const text of ["done", "todo", "one", "two", "child", "mixed"]) {
      const item = taskStartingWith(text);
      expect(item.querySelector("p")).toBeNull();
      expect(Array.from(item.childNodes, (node) => node.nodeName)).toEqual(["INPUT", "#text"]);
      expect(item.lastChild?.textContent).toBe(text);
    }

    // A loose or nested item keeps its paragraphs; the box leads the first one.
    for (const text of ["loose", "parent"]) {
      const item = taskStartingWith(text);
      const paragraph = item.firstElementChild;
      expect(paragraph?.tagName).toBe("P");
      expect(paragraph?.firstChild).toBe(ownBoxes(item)[0] ?? null);
      expect(paragraph?.textContent).toBe(text);
    }
  } finally {
    view.unmount();
  }
});

/** The element a body is painted into (`markdownClassName`), which carries `data-markdown-pending`
 *  while the worker owes its formatting and `data-markdown-fallback` when the parser refused it. */
function markdownElement(container: HTMLElement): HTMLElement {
  return container.firstElementChild as HTMLElement;
}

function pendingMark(container: HTMLElement): string | null {
  return markdownElement(container).getAttribute("data-markdown-pending");
}

function fallbackMark(container: HTMLElement): string | null {
  return markdownElement(container).getAttribute("data-markdown-fallback");
}

/** The body's own list items, not an `ask` block's options, so a row counts what it wrote. */
function topItems(container: HTMLElement): HTMLElement[] {
  return Array.from(markdownElement(container).querySelectorAll<HTMLElement>(":scope > ul > li"));
}

/** A list longer than the threshold, under a lead line that is the row's own, so no row reads
 *  the answer the engine cached for another row's body. */
const longItems = Math.ceil(markdownWorkerThreshold / "- x\n".length) + 200;

function longBody(lead: string): string {
  return `${lead}\n\n${"- x\n".repeat(longItems)}`;
}

/**
 * A body longer than `markdownWorkerThreshold` is parsed in a Web Worker (sjawhar/legion#2403),
 * so the reader's own thread is free while the heaviest body on the page parses. These rows are
 * what a reader sees of that: the text at once, the formatting when the answer lands, their
 * place in the body kept across it, and - whatever becomes of the worker - a formatted body in
 * the end. `data-markdown-fallback` keeps its one meaning, a body the parser itself refused.
 */
describe("A long body parsed in the worker", () => {
  beforeEach(async () => {
    await warmMarkdownRenderer();
  });

  afterEach(restoreParseWorker);

  test("paints its text at once, marked pending, then its formatted body", async () => {
    const worker = installFakeParseWorker();
    const markdown = longBody("A list the worker is parsing");
    const view = render(<MarkdownBody markdown={markdown} />);

    try {
      // The reader has the body's own text while the worker parses it: readable, selectable,
      // and never the failure marker.
      await waitFor(() => expect(pendingMark(view.container)).toBe("true"));
      expect(view.container.textContent).toBe(markdown);
      expect(fallbackMark(view.container)).toBeNull();
      expect(topItems(view.container)).toHaveLength(0);

      await act(() => worker.answer(0));
      await waitFor(() => expect(topItems(view.container)).toHaveLength(longItems));
      expect(pendingMark(view.container)).toBeNull();
      expect(fallbackMark(view.container)).toBeNull();
      expect(view.container.textContent).not.toContain("- x");
    } finally {
      view.unmount();
    }
  });

  // The worker answers with the document as plain JSON, and the page builds a document of its own
  // schema from it. `Schema.nodeFromJSON` cannot: it checks each attribute against its type, and
  // the Markdown parser writes `title: null` for a picture with no title where the schema's
  // `image.title` is a string, so every body holding a picture would be refused. This row is what
  // fails if the page reaches for `nodeFromJSON` again.
  test("rebuilds a body holding a picture, which its own parser writes a null title for", async () => {
    const worker = installFakeParseWorker();
    const markdown = longBody(
      "A picture over a list\n\n![shot.png](dispatch://CORE-1/artifact/shot-png@v1)"
    );
    const view = render(withQueries(<MarkdownBody markdown={markdown} />));

    try {
      await act(() => worker.answer(0));
      await waitFor(() => expect(topItems(view.container)).toHaveLength(longItems));
      const shot = view.container.querySelector("[data-dispatch-picture]");
      expect(shot?.getAttribute("data-dispatch-picture")).toBe(
        "dispatch://CORE-1/artifact/shot-png@v1"
      );
      expect(shot?.getAttribute("data-picture-caption")).toBe("shot.png");
      expect(pendingMark(view.container)).toBeNull();
      expect(fallbackMark(view.container)).toBeNull();
    } finally {
      view.unmount();
    }
  });

  // The live view hands each streamed delta to the same body as a longer `markdown`. Between the
  // delta and the worker's answer the reader keeps the paint they were reading: a turn that
  // flipped back to its source text on every delta would be unreadable while it streamed.
  test("keeps the formatted body it painted when its text grows, until the new answer lands", async () => {
    const worker = installFakeParseWorker();
    const first = longBody("A turn still streaming");
    const grown = `${first}${"- y\n".repeat(50)}`;
    const view = render(<MarkdownBody markdown={first} />);

    try {
      await act(() => worker.answer(0));
      await waitFor(() => expect(topItems(view.container)).toHaveLength(longItems));

      view.rerender(<MarkdownBody markdown={grown} />);
      await waitFor(() => expect(pendingMark(view.container)).toBe("true"));
      expect(topItems(view.container)).toHaveLength(longItems);
      expect(view.container.textContent).not.toContain("- x");
      expect(fallbackMark(view.container)).toBeNull();

      await act(() => worker.answer(1));
      await waitFor(() => expect(topItems(view.container)).toHaveLength(longItems + 50));
      expect(topItems(view.container).at(-1)?.textContent).toBe("y");
      expect(pendingMark(view.container)).toBeNull();
      expect(fallbackMark(view.container)).toBeNull();
    } finally {
      view.unmount();
    }
  });

  test("drops an older answer that arrives after a newer one, so only the newest source paints", async () => {
    const worker = installFakeParseWorker();
    const first = longBody("A turn answered out of order");
    const grown = `${first}${"- y\n".repeat(50)}`;
    const view = render(<MarkdownBody markdown={first} />);

    try {
      await worker.waitForRequests(1);
      view.rerender(<MarkdownBody markdown={grown} />);
      await worker.waitForRequests(2);
      expect(worker.requests.map(({ markdown }) => markdown)).toEqual([first, grown]);

      // The newer body is answered first, the older one after it.
      await act(() => worker.answer(1));
      await waitFor(() => expect(topItems(view.container)).toHaveLength(longItems + 50));
      await act(() => worker.answer(0));
      await flushTasks();

      expect(topItems(view.container)).toHaveLength(longItems + 50);
      expect(topItems(view.container).at(-1)?.textContent).toBe("y");
      expect(pendingMark(view.container)).toBeNull();
      expect(fallbackMark(view.container)).toBeNull();
    } finally {
      view.unmount();
    }
  });

  // The parser refusing a body is the one final failure, and the only thing
  // `data-markdown-fallback` says: pending before it was interim, never a failure.
  test("ends as its literal text, marked as the fallback, where the parser refuses it", async () => {
    const worker = installFakeParseWorker();
    const refused =
      'A body with <span data-dispatch="bogus">a span</span> this schema has no node for';
    const markdown = longBody(refused);
    const view = render(<MarkdownBody markdown={markdown} />);

    try {
      await waitFor(() => expect(pendingMark(view.container)).toBe("true"));
      expect(fallbackMark(view.container)).toBeNull();

      await act(() => worker.answer(0));
      await waitFor(() => expect(fallbackMark(view.container)).toBe("true"));
      expect(pendingMark(view.container)).toBeNull();
      expect(view.container.textContent).toBe(markdown);
      expect(topItems(view.container)).toHaveLength(0);
    } finally {
      view.unmount();
    }
  });

  test("ends formatted on the page's own thread when no worker can be created", async () => {
    const worker = installFakeParseWorker({ refuseToCreate: true });
    const view = render(<MarkdownBody markdown={longBody("A page that allows no worker")} />);

    try {
      // The page parses it itself: the reader waits, and then reads the body as a list. A
      // worker that cannot be created is never the parser refusing the body.
      await waitFor(() => expect(topItems(view.container)).toHaveLength(longItems));
      expect(worker.created).toBeGreaterThan(0);
      expect(pendingMark(view.container)).toBeNull();
      expect(fallbackMark(view.container)).toBeNull();
    } finally {
      view.unmount();
    }
  });

  test("ends formatted on the page's own thread when the worker crashes", async () => {
    const worker = installFakeParseWorker();
    const view = render(<MarkdownBody markdown={longBody("A worker that crashes")} />);

    try {
      await act(() => worker.crash(0));
      await waitFor(() => expect(topItems(view.container)).toHaveLength(longItems));
      expect(pendingMark(view.container)).toBeNull();
      expect(fallbackMark(view.container)).toBeNull();
    } finally {
      view.unmount();
    }
  });

  // Nothing ever answers this body. The engine's deadline ends with the body parsed on the page;
  // this row shortens it through `setParseDeadline`, the seam `resetMermaidDiagrams` is for the
  // diagram renderer's own limit, so it proves what the deadline ends in without waiting out the
  // page's own 10 s. The 25 s below is room for a loaded runner, not a second bound.
  test("ends formatted on the page's own thread when the worker misses its deadline", async () => {
    const worker = installFakeParseWorker();
    setParseDeadline(50);
    const view = render(<MarkdownBody markdown={longBody("A worker that never answers")} />);

    try {
      await worker.waitForRequests(1);
      await waitFor(() => expect(topItems(view.container)).toHaveLength(longItems), {
        interval: 250,
        timeout: 25_000,
      });
      expect(pendingMark(view.container)).toBeNull();
      expect(fallbackMark(view.container)).toBeNull();
    } finally {
      setParseDeadline();
      view.unmount();
    }
  }, 40_000);

  // Every paint recomputes the ask hosts, so a body whose decision card arrives late draws it
  // from the answer it already has: the card is a dependency of the render, never a new parse.
  test("repaints for a new decision card without sending the body a second time", async () => {
    const worker = installFakeParseWorker();
    const markdown = `:::ask{urgency="high"}\n${askBody}\n${longBody("A decision above a list")}`;
    const view = render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <TextAskCard.Provider value={undefined}>
          <MarkdownBody markdown={markdown} />
        </TextAskCard.Provider>
      </MemoryRouter>
    );

    try {
      await act(() => worker.answer(0));
      await waitFor(() => expect(topItems(view.container)).toHaveLength(longItems));
      const shell = await findAskShell(view.container);
      expect(within(shell).queryByText("Decision")).toBeNull();
      expect(worker.requests).toHaveLength(1);

      view.rerender(withAskCard(<MarkdownBody markdown={markdown} />));
      const carded = await findAskShell(view.container);
      expect(await within(carded).findByText("Decision")).toBeDefined();
      await flushTasks();

      expect(worker.requests).toHaveLength(1);
      expect(topItems(view.container)).toHaveLength(longItems);
      expect(pendingMark(view.container)).toBeNull();
      expect(fallbackMark(view.container)).toBeNull();
    } finally {
      view.unmount();
    }
  });

  // One body on two surfaces is one request with two waiters (the Conversation's card and the
  // hover card of a reference to it both draw the same comment). A surface that goes away takes
  // its own waiter and nothing else: the body stays with the worker, and the answer reaches the
  // surface the reader is still looking at.
  test("answers the surface still mounted when another drops the same body first", async () => {
    const worker = installFakeParseWorker();
    const markdown = longBody("A body two surfaces draw");
    const leaving = render(<MarkdownBody markdown={markdown} />);
    const staying = render(<MarkdownBody markdown={markdown} />);

    try {
      await waitFor(() => expect(pendingMark(staying.container)).toBe("true"));
      await worker.waitForRequests(1);
      expect(worker.requests).toHaveLength(1);

      leaving.unmount();
      await flushTasks();
      expect(worker.requests).toHaveLength(1);

      await act(() => worker.answer(0));
      await waitFor(() => expect(topItems(staying.container)).toHaveLength(longItems));
      expect(pendingMark(staying.container)).toBeNull();
      expect(fallbackMark(staying.container)).toBeNull();
    } finally {
      staying.unmount();
    }
  });

  // A surface that starts drawing a body the worker has already answered - the hover card of a
  // reference the reader points at after the card below it formatted - reads the answer the
  // engine kept, so it is formatted from its first paint and the worker hears nothing.
  test("paints a body whose answer already landed, without asking the worker again", async () => {
    const worker = installFakeParseWorker();
    const markdown = longBody("A body answered before the second surface drew it");
    const first = render(<MarkdownBody markdown={markdown} />);

    try {
      await act(() => worker.answer(0));
      await waitFor(() => expect(topItems(first.container)).toHaveLength(longItems));
    } finally {
      first.unmount();
    }
    await flushTasks();

    const later = render(<MarkdownBody markdown={markdown} />);
    try {
      await waitFor(() => expect(topItems(later.container)).toHaveLength(longItems));
      expect(pendingMark(later.container)).toBeNull();
      expect(fallbackMark(later.container)).toBeNull();
      await flushTasks();
      expect(worker.requests).toHaveLength(1);
    } finally {
      later.unmount();
    }
  });
});
