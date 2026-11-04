import { expect, spyOn, test } from "bun:test";
import { ID_AMBIGUOUS, itemFromSearch } from "@legion/contracts";
import type { QueryClient } from "@tanstack/react-query";
import { QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import {
  answerTextReadsWithTheSeededText,
  createQueryClient,
} from "../../__tests__/proof-document";
import { ApiError, api } from "../../api/client";
import type { Artifact, Ask, AskRead, Comment } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { Margin } from "../margin/Margin";
import { MarginProvider } from "../margin/margin-context";
import { issue, SelectedItemLabel, specArtifact } from "../margin/margin-fixture";
import type { MarginOwner } from "../margin/useMarginItems";
import { ArtifactDocument } from "./ArtifactDocument";

answerTextReadsWithTheSeededText();

const WHOLE_COMMENT = "11111111-1111-4111-8111-111111111111";
const OTHER_COMMENT = "1111dddd-dddd-4ddd-8ddd-dddddddddddd";
const SHORT_COMMENT = "11111111";
const WHOLE_ASK = "22222222-2222-4222-8222-222222222222";
const SHORT_ASK = "22222222";
/** A whole id of a comment that is not on this document: it names nothing, as a short id that
 *  resolves to nothing does. */
const UNKNOWN_COMMENT = "33333333-3333-4333-8333-333333333333";

/** The project document the second owner's tests stand on, the one `?ask=` is written beside. */
const projectDocument: Artifact = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-9",
  issue_key: null,
  kind: "doc",
  name: "Plan",
  primary: false,
  project: "CORE",
  slug: "plan-md",
  versions: [],
};

const issueOwner: MarginOwner = { key: "CORE-1", kind: "issue" };
const documentOwner: MarginOwner = {
  artifactId: projectDocument.id,
  kind: "document",
  project: projectDocument.project,
  slug: projectDocument.slug,
};

function anchoredComment(id: string): Comment {
  return {
    anchor: {
      artifact_id: specArtifact.id,
      block_id: null,
      mark_id: "m-1",
      orphaned: false,
      quote: "The live",
      version: 1,
    },
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: "Why this way?",
    created_at: "2026-09-09T00:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...commentDeliveryFields(),
  };
}

function documentAsk(id: string): Ask {
  return {
    anchor: {
      artifact_id: projectDocument.id,
      block_id: null,
      mark_id: "m-2",
      orphaned: false,
      quote: "The live",
      version: 1,
    },
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id,
    issue_key: null,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which way round?",
    state: "open",
    urgency: "med",
  };
}

function askRead(ask: Ask): AskRead {
  return {
    answers: [],
    ask,
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
  };
}

/** The document page as both of its hosts render it: the item the address names comes from the
 *  URL's `?comment=` or `?ask=` (`useIssueDetail`, `DocumentPage`), so the page reads whatever
 *  the address holds after a rewrite. */
function Page({ document, owner }: { document: Artifact; owner: MarginOwner }): ReactNode {
  const { search } = useLocation();
  const item = itemFromSearch(search);

  return (
    <ArtifactDocument
      artifact={document}
      askId={item?.kind === "ask" ? item.id : undefined}
      commentId={item?.kind === "comment" ? item.id : undefined}
      isClosed={false}
      onVersionChange={() => {}}
      owner={owner}
      showDiff={false}
      version={undefined}
    />
  );
}

function CurrentRoute(): ReactNode {
  const { pathname, search } = useLocation();

  return <output data-testid="current-route">{`${pathname}${search}`}</output>;
}

/** Back over the page's own history, so a test can see what the rewrite left behind it. */
function GoBack(): ReactNode {
  const navigate = useNavigate();

  return (
    <button onClick={() => navigate(-1)} type="button">
      Go back
    </button>
  );
}

/** The query cache a document page opens with: the reader, the schema and the reads
 *  `ProofDocument` makes of the document itself, so the only reads a test sees are the ones the
 *  address's item asks for. */
function documentClient(document: Artifact): QueryClient {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(["block-schema"], { types: [], version: 1 });
  queryClient.setQueryData(["artifact", document.id], document);
  queryClient.setQueryData(["artifact", document.id, "blocks"], []);
  queryClient.setQueryData(["artifact", document.id, "text"], {
    markdown: "The live document",
    version: null,
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["user-state"], {});
  return queryClient;
}

function renderDocument({
  document = specArtifact,
  margin = false,
  owner = issueOwner,
  path,
  queryClient = documentClient(document),
}: {
  document?: Artifact;
  margin?: boolean;
  owner?: MarginOwner;
  path: string;
  queryClient?: QueryClient;
}) {
  const runtime = fakeDocumentRuntime({ text: "The live document" });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1", path]} initialIndex={1}>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={runtime.runtime}>
          <MarginProvider>
            <CurrentRoute />
            <GoBack />
            {margin ? <SelectedItemLabel /> : null}
            <Page document={document} owner={owner} />
            {margin ? <Margin /> : null}
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { runtime, view };
}

function route(): string {
  return screen.getByTestId("current-route").textContent ?? "";
}

test("a `?comment=` that writes its id whole selects the comment with nothing resolved", async () => {
  const comment = anchoredComment(WHOLE_COMMENT);
  const getReferences = spyOn(api, "getReferences");
  const listComments = spyOn(api, "listComments").mockResolvedValue([comment]);
  const { view } = renderDocument({ path: `/issues/CORE-1/spec?comment=${WHOLE_COMMENT}` });

  try {
    await waitFor(() => expect(listComments).toHaveBeenCalledWith("CORE-1", specArtifact.id));
    await screen.findByLabelText("Document editor");
    // The one read the whole id makes is the document's own comment list: nothing is resolved
    // for it and the address is left as the reader wrote it.
    expect(listComments).toHaveBeenCalledTimes(1);
    expect(getReferences).not.toHaveBeenCalled();
    expect(route()).toBe(`/issues/CORE-1/spec?comment=${WHOLE_COMMENT}`);
  } finally {
    view.unmount();
    listComments.mockRestore();
    getReferences.mockRestore();
  }
});

test("a `?ask=` that writes its id whole reads the ask under it with nothing resolved", async () => {
  const ask = documentAsk(WHOLE_ASK);
  const getReferences = spyOn(api, "getReferences");
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead(ask));
  const { view } = renderDocument({
    document: projectDocument,
    owner: documentOwner,
    path: `/projects/CORE/documents/plan-md?ask=${WHOLE_ASK}`,
  });

  try {
    await waitFor(() => expect(getAsk.mock.calls).toEqual([[WHOLE_ASK]]));
    expect(getReferences).not.toHaveBeenCalled();
    expect(route()).toBe(`/projects/CORE/documents/plan-md?ask=${WHOLE_ASK}`);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getReferences.mockRestore();
  }
});

test("a `?comment=` written short resolves, selects the whole-id comment and replaces the address", async () => {
  const comment = anchoredComment(WHOLE_COMMENT);
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: { id: WHOLE_COMMENT, kind: "comment", ref: `dispatch://CORE-1/comment/${WHOLE_COMMENT}` },
  });
  const listComments = spyOn(api, "listComments").mockResolvedValue([comment]);
  const queryClient = documentClient(specArtifact);
  queryClient.setQueryData(["comments", issue.key], [comment]);
  const { view } = renderDocument({
    margin: true,
    path: `/issues/CORE-1/spec?comment=${SHORT_COMMENT}`,
    queryClient,
  });

  try {
    await waitFor(() => expect(route()).toBe(`/issues/CORE-1/spec?comment=${WHOLE_COMMENT}`));
    // The reference the address names, resolved within the issue that owns the comment.
    expect(getReferences).toHaveBeenCalledWith(`dispatch://CORE-1/comment/${SHORT_COMMENT}`);
    expect(getReferences).toHaveBeenCalledTimes(1);
    // The margin reads the same address, so its card opens on the whole id, never the prefix.
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe(WHOLE_COMMENT)
    );
    // The rewrite replaced the address rather than pushing one, so Back leaves the page the
    // reader came from instead of returning to the short address.
    fireEvent.click(screen.getByRole("button", { name: "Go back" }));
    await waitFor(() => expect(route()).toBe("/issues/CORE-1"));
  } finally {
    view.unmount();
    listComments.mockRestore();
    getReferences.mockRestore();
  }
});

test("a project document's `?ask=` written short reads the ask under its whole id alone", async () => {
  const ask = documentAsk(WHOLE_ASK);
  const reference = `dispatch://CORE/artifact/plan-md/ask/${SHORT_ASK}`;
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: { id: WHOLE_ASK, kind: "ask", ref: `dispatch://CORE/artifact/plan-md/ask/${WHOLE_ASK}` },
  });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead(ask));
  const { view } = renderDocument({
    document: projectDocument,
    owner: documentOwner,
    path: `/projects/CORE/documents/plan-md?ask=${SHORT_ASK}`,
  });

  try {
    await waitFor(() => expect(route()).toBe(`/projects/CORE/documents/plan-md?ask=${WHOLE_ASK}`));
    expect(getReferences).toHaveBeenCalledWith(reference);
    // The ask is read under the whole id alone: a prefix is no key anything is cached under.
    await waitFor(() => expect(getAsk.mock.calls).toEqual([[WHOLE_ASK]]));
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getReferences.mockRestore();
  }
});

test("an ambiguous `?comment=` lists the matches instead of the document", async () => {
  const getReferences = spyOn(api, "getReferences").mockRejectedValue(
    new ApiError(409, {
      candidates: [
        {
          first_line: "The first note",
          id: WHOLE_COMMENT,
          issue_key: "CORE-1",
          kind: "comment",
          ref: `dispatch://CORE-1/comment/${WHOLE_COMMENT}`,
        },
        {
          first_line: "The other note",
          id: OTHER_COMMENT,
          issue_key: "CORE-1",
          kind: "comment",
          ref: `dispatch://CORE-1/comment/${OTHER_COMMENT}`,
        },
      ],
      code: ID_AMBIGUOUS,
      error: `the id prefix "${SHORT_COMMENT}" names 2 items; write more of the id`,
    })
  );
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const { view } = renderDocument({ path: `/issues/CORE-1/spec?comment=${SHORT_COMMENT}` });

  try {
    await screen.findByText("Several items match that link");
    const first = screen.getByRole("link", { name: "The first note" });
    expect(first.getAttribute("href")).toBe(`/issues/CORE-1/comments/${WHOLE_COMMENT}`);
    expect(screen.getByRole("link", { name: "The other note" }).getAttribute("href")).toBe(
      `/issues/CORE-1/comments/${OTHER_COMMENT}`
    );
    // The list is a view of the page at the address the reader followed, not a route of its own.
    expect(route()).toBe(`/issues/CORE-1/spec?comment=${SHORT_COMMENT}`);
    expect(screen.queryByLabelText("Document editor")).toBeNull();
  } finally {
    view.unmount();
    listComments.mockRestore();
    getReferences.mockRestore();
  }
});

test.each([
  ["written short", SHORT_COMMENT, true],
  ["written whole", UNKNOWN_COMMENT, false],
])("a `?comment=` %s that names nothing leaves the document as an unknown id does", async (_name, id, resolves) => {
  const getReferences = spyOn(api, "getReferences").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: `no comment at ${id}` })
  );
  const listComments = spyOn(api, "listComments").mockResolvedValue([
    anchoredComment(OTHER_COMMENT),
  ]);
  const { view } = renderDocument({ path: `/issues/CORE-1/spec?comment=${id}` });

  try {
    await screen.findByLabelText("Document editor");
    await waitFor(() => expect(listComments).toHaveBeenCalled());
    expect(screen.queryByText("Several items match that link")).toBeNull();
    // The address is left as written: there is no whole id to replace it with.
    expect(route()).toBe(`/issues/CORE-1/spec?comment=${id}`);
    expect(getReferences.mock.calls.length > 0).toBe(resolves);
  } finally {
    view.unmount();
    listComments.mockRestore();
    getReferences.mockRestore();
  }
});
