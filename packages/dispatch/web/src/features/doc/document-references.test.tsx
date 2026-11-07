import { expect, test } from "bun:test";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  createQueryClient,
  renderProofDocument,
} from "../../__tests__/proof-document";
import type { Ask, AskRead, IssueReferenceSummary } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { DocumentRuntime } from "./runtime";
import { VersionView } from "./VersionView";

answerTextReadsWithTheSeededText();

/** The text every editor below holds: a bare issue key, a `#id` and a bare `dispatch://`
 *  reference, the three forms a document's own text writes. */
const TEXT = "See CORE-2 and #q2 and dispatch://CORE-1/artifact/design here.";

/** The address of a document, written bare above and, below, as the autolink a document stores
 *  for a link whose text is its own destination. */
const AUTOLINK = "dispatch://CORE-1/artifact/design";

const ask: Ask = {
  anchor: null,
  answer: null,
  artifact_id: null,
  author: { id: "alice", kind: "user" },
  block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
  block_id: "q2",
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Which way round?",
  state: "open",
  urgency: "med",
};

const askRead: AskRead = {
  answers: [],
  ask,
  claimant: null,
  edits: [],
  followers: [],
  replies: [],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
};

const summary: IssueReferenceSummary = {
  documents: [
    {
      id: "artifact-1",
      kind: "doc",
      latest_version: 1,
      name: "spec.md",
      primary: true,
      slug: "spec",
    },
    {
      id: "artifact-2",
      kind: "doc",
      latest_version: 2,
      name: "design.md",
      primary: false,
      slug: "design",
    },
  ],
  key: "CORE-1",
  project: "CORE",
  status: "testing",
  title: "Ship the release",
};

const referenced: IssueReferenceSummary = {
  documents: [],
  key: "CORE-2",
  project: "CORE",
  status: "open",
  title: "Fix the thing",
};

/** A query client holding everything the three references resolve through: the issue a bare key
 *  names, the question map the document's own source answers with and the ask a `#id` declares,
 *  and the issue whose document the bare reference names. */
function seededQueryClient(): QueryClient {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["issue-summary", "CORE-1"], summary);
  queryClient.setQueryData(["issue-summary", "CORE-2"], referenced);
  // The question map of this document, under the key the route's read is cached by: the
  // document is the source of its own text (`refs.ScopeOf`).
  queryClient.setQueryData(["references", "questions", "artifact", artifact.id], {
    document: artifact.id,
    questions: [{ ask: ask.id, block_id: "q2", reference: `dispatch://CORE-1/ask/${ask.id}` }],
  });
  queryClient.setQueryData(["ask", "ask-1"], askRead);
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  return queryClient;
}

/** What the host last told the editor to draw: the words of each candidate to the title drawn
 *  over them, or null while it resolved none. */
function drawnTitles(
  references: { targets: ReadonlyMap<string, { title: string } | null> } | null | undefined
): Record<string, string | null> {
  const drawn: Record<string, string | null> = {};
  for (const [words, target] of references?.targets ?? []) {
    drawn[words] = target?.title ?? null;
  }
  return drawn;
}

test("the live editor is told to draw a bare key's, a #id's and a bare reference's title", async () => {
  await warmMarkdownRenderer();
  const queryClient = seededQueryClient();
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    const references = editors[0]?.references;
    if (references === undefined || references === null) throw new Error("references missing");

    // The editor hands the host its own text and the host reads what it cites: the one reader
    // decides, so the words it answers are the words the candidates are keyed by.
    expect(references.scan(TEXT)).toEqual([
      { end: 10, key: "CORE-2", start: 4 },
      { end: 18, key: "#q2", start: 15 },
      { end: 56, key: "dispatch://CORE-1/artifact/design", start: 23 },
    ]);
    act(() => {
      references.onKeys(["#q2", "CORE-2", "dispatch://CORE-1/artifact/design"]);
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references)).toEqual({
        "#q2": "Which way round?",
        "CORE-2": "CORE-2 Fix the thing",
        "dispatch://CORE-1/artifact/design": "design.md",
      })
    );
    // A bare key reads as its key and its title, as a rendered body's link does, and clicking it
    // opens the issue it names.
    expect(editors[0]?.references?.targets.get("CORE-2")).toEqual({
      href: "dispatch://CORE-2",
      length: undefined,
      title: "CORE-2 Fix the thing",
      titleKey: "CORE-2",
    });
  } finally {
    view.unmount();
  }
});

test("a #id the author left a hyphen on draws its title over the id alone", async () => {
  await warmMarkdownRenderer();
  const queryClient = seededQueryClient();
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    act(() => {
      editors[0]?.references?.onKeys(["#q2-"]);
    });

    // The question is q2, so the title stands for `#q2`, the three characters the reference
    // names, and the `-` the author typed stays their own text.
    await waitFor(() =>
      expect(editors[0]?.references?.targets.get("#q2-")).toEqual({
        href: "dispatch://CORE-1/ask/ask-1",
        length: 3,
        title: "Which way round?",
        titleKey: undefined,
      })
    );
  } finally {
    view.unmount();
  }
});

test("a key no issue holds and a #id no question declares are left as the author wrote them", async () => {
  await warmMarkdownRenderer();
  const queryClient = seededQueryClient();
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    act(() => {
      editors[0]?.references?.onKeys(["#q9", "CORE-404"]);
    });

    await Bun.sleep(20);
    expect(drawnTitles(editors[0]?.references)).toEqual({ "#q9": null, "CORE-404": null });
  } finally {
    view.unmount();
  }
});

test("the live editor is told to draw the title for a link whose text is its own address", async () => {
  const queryClient = seededQueryClient();
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    const references = editors[0]?.references;
    if (references === undefined || references === null) throw new Error("references missing");

    // What the editor asks of a link whose text is its destination: the reader must read that
    // destination as one reference running end to end, which is then the candidate's key.
    expect(references.scan(AUTOLINK)).toEqual([{ end: AUTOLINK.length, key: AUTOLINK, start: 0 }]);
    act(() => {
      references.onKeys([AUTOLINK]);
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references)).toEqual({ [AUTOLINK]: "design.md" })
    );
  } finally {
    view.unmount();
  }
});

function CurrentRoute(): ReactNode {
  const location = useLocation();
  return <div data-testid="current-route">{location.pathname}</div>;
}

/** The past version of the document, rendered as the document page renders it. */
function renderVersion(markdown: string = TEXT) {
  const fake = fakeDocumentRuntime();
  const queryClient = seededQueryClient();
  queryClient.setQueryData(["artifact", "artifact-1", "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown,
    named: false,
    number: 1,
    summary: null,
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <CurrentRoute />
          <VersionView
            artifactId="artifact-1"
            asks={[]}
            blockSchema={fake.runtime.blockSchema}
            createdAt="2026-09-09T00:00:00Z"
            highlight={undefined}
            owner={undefined}
            version={1}
          />
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { ...fake, view };
}

test("a past version is told to draw the same three titles over its own text", async () => {
  await warmMarkdownRenderer();
  const { editors, view } = renderVersion();

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    const references = editors[0]?.references;
    if (references === undefined || references === null) throw new Error("references missing");
    act(() => {
      references.onKeys(references.scan(TEXT).map((candidate) => candidate.key));
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references)).toEqual({
        "#q2": "Which way round?",
        "CORE-2": "CORE-2 Fix the thing",
        "dispatch://CORE-1/artifact/design": "design.md",
      })
    );
  } finally {
    view.unmount();
  }
});

test("a past version is told to draw the title for a link whose text is its own address", async () => {
  const { editors, view } = renderVersion(`See [${AUTOLINK}](${AUTOLINK}) here.`);

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    const references = editors[0]?.references;
    if (references === undefined || references === null) throw new Error("references missing");
    // The editor reads a link whose text is its destination as one candidate of that address.
    expect(references.scan(AUTOLINK)).toEqual([{ end: AUTOLINK.length, key: AUTOLINK, start: 0 }]);
    act(() => {
      references.onKeys([AUTOLINK]);
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references)).toEqual({ [AUTOLINK]: "design.md" })
    );
  } finally {
    view.unmount();
  }
});

test("a click on a reference in a past version opens what it names", async () => {
  const { editors, view } = renderVersion();

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    const root = editors[0]?.root;
    if (root === undefined) throw new Error("editor root missing");
    // What the editor draws over a resolved reference: a link carrying its target, which is
    // also what its Markdown serializer leaves on a `dispatch://` link mark.
    act(() => {
      root.innerHTML =
        '<p><a class="dispatch-reference" role="link" data-dispatch-href="dispatch://CORE-2">CORE-2 Fix the thing</a></p>';
    });
    const anchor = root.querySelector("a");
    if (anchor === null) throw new Error("anchor missing");

    fireEvent.click(anchor);
    expect(within(view.container).getByTestId("current-route").textContent).toBe("/issues/CORE-2");
  } finally {
    view.unmount();
  }
});
