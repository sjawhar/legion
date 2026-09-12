import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";

import { api } from "../../api/client";
import type { ArtifactDetails, AskRead } from "../../api/types";
import { MarkdownBody } from "./MarkdownBody";

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
    edits: [],
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
    edits: [],
    replies: [],
  };
  const projectArtifact: ArtifactDetails = {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: null,
    kind: "doc",
    name: "Runbook.md",
    primary: false,
    project: "CORE",
    ref_key: "CORE/runbook-md",
    referenced_by: [],
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
    edits: [],
    replies: [],
  };
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // @sjawhar/proof-editor's Markdown link serializer sanitizes a dispatch:// href to "" (Milkdown
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
