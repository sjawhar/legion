import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import type { ArtifactText, IssueDetails } from "../../api/types";
import * as connection from "./connection";
import * as editor from "./editor";
import { afterTheFirstBatch, theFirstBatchIsAwaited } from "./first-batch";
import { openDocumentEarly, takeEarlyTextRead } from "./open-document-early";

// The first-batch gate is one page's state, so each row leaves it as it found it: nothing
// awaited, which is what every page with no document on it reads.
afterEach(() => theFirstBatchIsAwaited(false));

const artifact: IssueDetails["artifacts"][number] = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-early",
  issue_key: "CORE-77",
  kind: "doc",
  name: "Notes",
  primary: true,
  project: "CORE",
  slug: "notes",
  versions: [],
};

const issue: IssueDetails = {
  artifacts: [artifact],
  assignee: null,
  blocked_by: [],
  children: [],
  claim: null,
  closed_at: null,
  components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  holder_state: null,
  key: "CORE-77",
  labels: [],
  last_seen: null,
  last_seq: 0,
  number: 77,
  open_asks: [],
  parent: null,
  primary_artifact_id: artifact.id,
  priority: null,
  progress: { children: null, tasks: null },
  project: "CORE",
  rank: "U",
  referenced_by_count: 0,
  repo: null,
  resolved_repo: null,
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  status: "todo",
  takeable_at: null,
  title: "The lease ledger",
  updated_at: "2026-09-10T00:00:00Z",
};

const text: ArtifactText = { markdown: "# The lease ledger\n", version: null };

// What a document address starts before any page has rendered: the two chunks the document's
// editor is built from, and the schema its surface draws with and its socket is admitted
// under. Each is bytes that arrive while the page renders, and each used to wait for
// `ProofDocument`'s admission - the editor's half of the load began 5,920 ms into a throttled
// phone's (dispatch://INFRA-341, R11). The socket is not among them, by measurement: opened
// from here it put the sync's apply and the editor's build in front of the page's own draw of
// the document, which is the draw the reader reads while they wait (`openDocumentEarly`).
test("a document address asks for the transport chunk, the editor chunk and the block schema", async () => {
  // The asks themselves are what this row reads, so each is held where it is made: the chunks
  // behind them are the browser's business, and the transport's connect is never called here.
  const transport = spyOn(connection, "loadDocumentTransport").mockImplementation(() =>
    Promise.resolve(() => {
      throw new Error("the boot task connects nothing");
    })
  );
  const editorChunk = spyOn(editor, "loadEditor").mockImplementation(() => undefined);
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "getIssueArtifactText").mockResolvedValue(text),
    spyOn(api, "getArtifactText").mockResolvedValue(text),
    spyOn(api, "getMyState").mockResolvedValue({}),
  ];
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    openDocumentEarly(client, "/issues/CORE-77/artifacts/notes", "");

    expect(transport).toHaveBeenCalled();
    await waitFor(() => expect(editorChunk).toHaveBeenCalled());
    await waitFor(() => expect(client.getQueryData(["block-schema"])).toBeDefined());
    // And the read the document's own admission takes, which is the schema admission check the
    // socket it opens waits for: made here, so the reader's text is downloaded once.
    await waitFor(() => expect(takeEarlyTextRead(artifact.id)).toBeDefined());
  } finally {
    for (const spy of [transport, editorChunk, ...reads]) {
      spy.mockRestore();
    }
    client.clear();
  }
});

// Which reads the page holds behind the editor's first batch is decided from the address, here,
// before React has rendered anything: the panels that make them - the margin above the router,
// the issue header inside the page - render before the document surface exists
// (`first-batch.ts`).
test("a document address holds the reads the first editable block needs none of", async () => {
  const transport = spyOn(connection, "loadDocumentTransport").mockImplementation(() =>
    Promise.resolve(() => {
      throw new Error("the boot task connects nothing");
    })
  );
  const editorChunk = spyOn(editor, "loadEditor").mockImplementation(() => undefined);
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "getIssueArtifactText").mockResolvedValue(text),
    spyOn(api, "getArtifactText").mockResolvedValue(text),
    spyOn(api, "getMyState").mockResolvedValue({}),
  ];
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    openDocumentEarly(client, "/issues/CORE-77/spec", "");

    expect(afterTheFirstBatch()).toBe(false);
    await waitFor(() => expect(takeEarlyTextRead(artifact.id)).toBeDefined());
    expect(afterTheFirstBatch()).toBe(false);
  } finally {
    for (const spy of [transport, editorChunk, ...reads]) {
      spy.mockRestore();
    }
    client.clear();
  }
});

// And a page with no document on it holds nothing: there is no first batch to wait for, so
// every read goes out as it always did.
test("an address that opens no document holds no read", () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    theFirstBatchIsAwaited(true);
    openDocumentEarly(client, "/issues/CORE-77/conversation", "");

    expect(afterTheFirstBatch()).toBe(true);
  } finally {
    client.clear();
  }
});

// An issue whose document cannot be opened holds nothing either: a slug this issue has no
// artifact for names nothing to build an editor from, so the reads stop waiting for one.
test("a document address whose issue names no document lets the held reads go", async () => {
  const transport = spyOn(connection, "loadDocumentTransport").mockImplementation(() =>
    Promise.resolve(() => {
      throw new Error("the boot task connects nothing");
    })
  );
  const editorChunk = spyOn(editor, "loadEditor").mockImplementation(() => undefined);
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "getIssueArtifactText").mockRejectedValue(new Error("no such artifact")),
    spyOn(api, "getMyState").mockResolvedValue({}),
  ];
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    openDocumentEarly(client, "/issues/CORE-77/artifacts/gone", "");

    expect(afterTheFirstBatch()).toBe(false);
    await waitFor(() => expect(afterTheFirstBatch()).toBe(true));
  } finally {
    for (const spy of [transport, editorChunk, ...reads]) {
      spy.mockRestore();
    }
    client.clear();
  }
});
