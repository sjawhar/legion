import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { Artifact, Ask } from "../../api/types";
import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";
import type { TextSource } from "./text-references";

/**
 * A `#id` resolves against the question blocks of the document its text belongs to, and the asks
 * of a document are read on the route its owner serves: an issue's document answers its asks on
 * the issue's route alone (`/artifacts/{id}/asks` refuses it, `ARTIFACT_LINKED`), a project's on
 * its own. A document source carries no issue key, so the owner is read from the document.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

let restore = () => {};

afterEach(() => {
  restore();
});

function documentOf(id: string, issueKey: string | null, slug: string): Artifact {
  return {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: issueKey,
    kind: "doc",
    name: `${slug}.md`,
    primary: false,
    project: "CORE",
    slug,
    versions: [],
  };
}

/** One question block of `document`, with the ask settled for it. */
function questionAsk(id: string, issueKey: string | null, document: string): Ask {
  return {
    anchor: null,
    answer: null,
    author: { id: "planner", kind: "session" },
    block_artifact: { id: document, primary: false, slug: "notes" },
    block_id: "q2",
    created_at: "2026-09-09T00:00:00Z",
    edited_at: null,
    id,
    issue_key: issueKey,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which region?",
    state: "open",
    urgency: "med",
  };
}

/** Both asks routes and the document read behind them, each spied so a test sees which ran. */
function spyAsks(document: Artifact, asks: Ask[]) {
  const spies = {
    getArtifact: spyOn(api, "getArtifact").mockResolvedValue(document),
    listArtifactAsks: spyOn(api, "listArtifactAsks").mockResolvedValue(asks),
    listIssueAsks: spyOn(api, "listIssueAsks").mockResolvedValue(
      asks.map((ask) => ({ ...ask, last_reply: null }))
    ),
  };
  // A document an issue owns refuses the artifact route, as the server does.
  if (document.issue_key !== null) {
    spies.listArtifactAsks.mockRejectedValue(
      new ApiError(400, {
        code: "ARTIFACT_LINKED",
        error: `artifact belongs to issue ${document.issue_key}; use /api/v1/issues/${document.issue_key}/...`,
      })
    );
  }
  restore = () => {
    for (const spy of Object.values(spies)) spy.mockRestore();
  };
  return spies;
}

function withQueries(
  children: ReactNode,
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
): ReactNode {
  return (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{children}</MemoryRouter>
    </QueryClientProvider>
  );
}

/** The `dispatch://` targets a rendered body links, in the order it draws them. */
function linked(container: HTMLElement): string[] {
  return [...container.querySelectorAll("a[data-dispatch-ref]")].map(
    (anchor) => anchor.getAttribute("data-dispatch-ref") ?? ""
  );
}

test("a document an issue owns reads its asks on the issue's route, and its text links the `#q2` it declares", async () => {
  const document = documentOf("doc-1", "CORE-1", "notes");
  const spies = spyAsks(document, [questionAsk("ask-2", "CORE-1", "doc-1")]);
  const source: TextSource = { artifactId: "doc-1", kind: "document" };
  const view = render(
    withQueries(<MarkdownBody markdown="See #q2 for the rollout." source={source} />)
  );

  try {
    await waitFor(() => expect(linked(view.container)).toEqual(["dispatch://CORE-1/ask/ask-2"]));
    expect(spies.listIssueAsks).toHaveBeenCalledWith("CORE-1", "all");
    expect(spies.listArtifactAsks).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("a project document's source reads its own asks route", async () => {
  const document = documentOf("doc-2", null, "handbook");
  const spies = spyAsks(document, [questionAsk("ask-3", null, "doc-2")]);
  const source: TextSource = {
    artifactId: "doc-2",
    kind: "document",
    project: "CORE",
    slug: "handbook",
  };
  const view = render(
    withQueries(<MarkdownBody markdown="See #q2 for the rollout." source={source} />)
  );

  try {
    await waitFor(() =>
      expect(linked(view.container)).toEqual(["dispatch://CORE/artifact/handbook/ask/ask-3"])
    );
    expect(spies.listArtifactAsks).toHaveBeenCalledWith("doc-2", "all");
    expect(spies.listIssueAsks).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("two texts of one document read its asks once", async () => {
  const document = documentOf("doc-1", "CORE-1", "notes");
  const spies = spyAsks(document, [questionAsk("ask-2", "CORE-1", "doc-1")]);
  const source: TextSource = { artifactId: "doc-1", kind: "document" };
  const view = render(
    withQueries(
      <>
        <MarkdownBody markdown="See #q2 for the rollout." source={source} />
        <MarkdownBody markdown="And #q2 again." source={source} />
      </>
    )
  );

  try {
    await waitFor(() =>
      expect(linked(view.container)).toEqual([
        "dispatch://CORE-1/ask/ask-2",
        "dispatch://CORE-1/ask/ask-2",
      ])
    );
    expect(spies.listIssueAsks).toHaveBeenCalledTimes(1);
    expect(spies.getArtifact).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("an issue's asks are cached under the issue, leaving the document's own asks key to its readers", async () => {
  const document = documentOf("doc-1", "CORE-1", "notes");
  spyAsks(document, [questionAsk("ask-2", "CORE-1", "doc-1")]);
  const source: TextSource = { artifactId: "doc-1", kind: "document" };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // What `["artifact", id, "asks"]` means to every other reader of it (`useAnsweredAsks`,
  // `ProofDocument`, the refresh an ask event pushes): the asks of that document alone.
  queryClient.setQueryData(["artifact", "doc-1", "asks"], []);
  const view = render(
    withQueries(<MarkdownBody markdown="See #q2 for the rollout." source={source} />, queryClient)
  );

  try {
    await waitFor(() => expect(linked(view.container)).toEqual(["dispatch://CORE-1/ask/ask-2"]));
    // The issue's whole ask list is the issue's, under the key its readers and its
    // invalidations already mean.
    expect(queryClient.getQueryData<Ask[]>(["asks", "CORE-1"])).toHaveLength(1);
    expect(queryClient.getQueryData<Ask[]>(["artifact", "doc-1", "asks"])).toEqual([]);
  } finally {
    view.unmount();
  }
});
