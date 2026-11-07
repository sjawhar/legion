import { afterEach, beforeEach, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { http, type RequestHandler } from "msw";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../api/client";
import type {
  Artifact,
  ArtifactText,
  DocumentRead,
  IssueAskRow,
  IssueDecisions,
} from "../api/types";
import { type DocumentToolbar, ProofDocument } from "../features/doc/ProofDocument";
import { DocumentRuntime } from "../features/doc/runtime";
import { MarginProvider, useMargin } from "../features/margin/margin-context";
import type { MarkPlacement } from "../features/margin/useMarginItems";
import { RefPreviewHost } from "../features/refs/RefPreview";
import { type FakeDocumentRuntime, fakeDocumentRuntime } from "./document-runtime";

/** The ProofDocument suites' shared page: an issue's spec, rendered with a fake document runtime,
 * the margin, and the reference preview host, as the issue page renders it. */

export const artifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  project: "CORE",
  kind: "doc",
  name: "spec.md",
  primary: true,
  slug: "spec",
  versions: [],
};

const blockSchema = {
  types: [
    {
      attributes: {
        kind: { choices: ["note", "warning"], default: "note", kind: "enum" as const },
        title: { default: "", kind: "string" as const },
      },
      content: "paragraph+" as const,
      name: "callout",
      render: "host" as const,
    },
  ],
  version: 1,
};

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/**
 * The issue's decisions and the viewer's read mark on a document, answered with the API's empty
 * shape so the page renders its loaded-empty state rather than its error state. A test that
 * asserts one of these spies the `api` method that fetches it and never reaches these handlers.
 *
 * Built afresh on each call rather than held in a module constant: an msw `RequestHandler`
 * carries the state of its own run, and `answerRequests` registers the instance it is handed.
 */
export function decisionsAndReadMarkHandlers(): RequestHandler[] {
  return [
    http.get("/api/v1/issues/:key/decisions", () =>
      Response.json({
        decisions: [],
        document: null,
        open_questions: [],
      } satisfies IssueDecisions)
    ),
    http.get("/api/v1/me/documents/:id/read", ({ params }) =>
      Response.json({
        artifact_id: String(params.id),
        read_at: null,
        version: null,
      } satisfies DocumentRead)
    ),
    // Opening a document the viewer has never read records the version they are looking at: the
    // page's own write of the route above.
    http.put("/api/v1/me/documents/:id/read", async ({ params, request }) => {
      const { version } = (await request.json()) as { version: number };
      return Response.json({
        artifact_id: String(params.id),
        read_at: "2026-09-09T00:00:00Z",
        version,
      } satisfies DocumentRead);
    }),
  ];
}

/**
 * `GET /api/v1/artifacts/:id` answered from `recordOf`, with the API's own answer for an artifact
 * nobody has where that finds none. Each file keeps its own lookup - this module's one artifact,
 * or the set a stubbed issue carries - and the not-found answer stays in one place.
 */
export function artifactRecordHandler(
  recordOf: (id: string) => Artifact | undefined
): RequestHandler {
  return http.get("/api/v1/artifacts/:id", ({ params }) => {
    const record = recordOf(String(params.id));
    return record === undefined
      ? Response.json(
          { code: "ARTIFACT_NOT_FOUND", error: `no artifact ${String(params.id)}` },
          { status: 404 }
        )
      : Response.json(record);
  });
}

/**
 * The reads the document surface makes of its own record, blocks and read mark, and of the
 * issue's asks and decisions: the three handlers above, and the three this adds to them. An id
 * this module's artifact does not carry is answered as the API answers an artifact nobody has.
 *
 * `asks` is read on each request, so a file that reassigns the rows it seeds between tests is
 * answered the rows it holds then. A caller that answers one of these routes itself passes its
 * own handler before the spread of these: `answerRequests` prepends, so of one call's handlers
 * the first given a path wins it.
 */
export function documentSurfaceHandlers({
  asks = () => [],
}: {
  asks?: () => IssueAskRow[];
} = {}): RequestHandler[] {
  return [
    artifactRecordHandler((id) => (id === artifact.id ? artifact : undefined)),
    http.get("/api/v1/artifacts/:id/blocks", () => Response.json([])),
    http.get("/api/v1/issues/:key/asks", () => Response.json(asks())),
    ...decisionsAndReadMarkHandlers(),
  ];
}

let defaultText = "";

/** Each test of the calling file starts with the server's text read answering the text its render
 * seeded. A test's own `spyOn(api, "getArtifactText")` returns this same mock and replaces what it
 * answers. */
export function answerTextReadsWithTheSeededText(): void {
  let restore: (() => void) | undefined;
  beforeEach(() => {
    const getArtifactText = spyOn(api, "getArtifactText").mockImplementation(async () => ({
      markdown: defaultText,
      version: null,
    }));
    restore = () => getArtifactText.mockRestore();
  });
  afterEach(() => restore?.());
}

function CurrentRoute() {
  const location = useLocation();
  return <div data-testid="current-route">{location.pathname}</div>;
}

export function renderProofDocument({
  document = artifact,
  fake = fakeDocumentRuntime({ text: "The live document" }),
  highlightTerm,
  isClosed = false,
  onAskAnswered,
  onUnavailable,
  queryClient = createQueryClient(),
  showDiff = false,
  surface,
  version,
}: {
  document?: Artifact;
  fake?: FakeDocumentRuntime;
  highlightTerm?: string;
  isClosed?: boolean;
  onAskAnswered?(askId: string): void;
  onUnavailable?(reason: string): void;
  queryClient?: QueryClient;
  showDiff?: boolean;
  surface?: "page" | "pane";
  version?: number;
} = {}) {
  const margin = {
    current: undefined as
      | {
          blockFilterId: string | undefined;
          focusBlock(blockId: string): void;
          documentBridge:
            | {
                focusMark(markId: string): void;
                setActiveMarks(markIds: string[]): void;
                whenSent(timeoutMs: number): Promise<void>;
              }
            | undefined;
          focusRequest: { markId: string; seq: number } | undefined;
          hoveredMarkId: string | undefined;
          markPlacements: ReadonlyMap<string, MarkPlacement>;
          pendingCompose:
            | {
                anchor: { artifact: string; mark_id: string; quote: string };
                kind: "ask" | "comment" | "suggestion";
                seq: number;
              }
            | undefined;
          cancelCompose(seq: number): void;
        }
      | undefined,
  };
  queryClient.setQueryData(["block-schema"], blockSchema);
  const toolbar: { current: DocumentToolbar | undefined } = { current: undefined };
  /** Every value the document reported upward, so a suite can assert it reported none at all. */
  const toolbarChanges: (DocumentToolbar | undefined)[] = [];
  const onToolbarChange = (next: DocumentToolbar | undefined) => {
    toolbarChanges.push(next);
    toolbar.current = next;
  };
  const onVersionChange = (_version: number | null) => {};
  const MarginProbe = () => {
    margin.current = useMargin();
    return null;
  };
  const renderDocument = (
    next: { highlightTerm?: string; isClosed?: boolean; showDiff?: boolean; version?: number } = {}
  ) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <CurrentRoute />
            <MarginProbe />
            <ProofDocument
              artifact={document}
              highlight={undefined}
              highlightTerm={next.highlightTerm ?? highlightTerm}
              isClosed={next.isClosed ?? isClosed}
              onAskAnswered={onAskAnswered}
              onToolbarChange={onToolbarChange}
              onUnavailable={onUnavailable}
              owner={{ key: "CORE-1", kind: "issue" }}
              onVersionChange={onVersionChange}
              showDiff={next.showDiff ?? showDiff}
              surface={surface}
              user={{ event_head: "0", kind: "user", login: "alice" }}
              version={next.version ?? version}
            />
            <RefPreviewHost />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const cachedText = queryClient.getQueryData<Partial<ArtifactText>>([
    "artifact",
    document.id,
    "text",
  ]);
  defaultText = cachedText?.markdown ?? fake.text;
  const view = render(renderDocument());
  return {
    ...fake,
    margin,
    rerender(next: {
      highlightTerm?: string;
      isClosed?: boolean;
      showDiff?: boolean;
      version?: number;
    }) {
      view.rerender(renderDocument(next));
    },
    // The server's sync can only follow the connection, which exists once the lazy transport
    // has resolved.
    async sync() {
      await waitFor(() => expect(fake.connections).toHaveLength(1));
      fake.sync();
    },
    toolbar,
    toolbarChanges,
    view,
  };
}

// `reportLoadFailure`'s `setConnection`/`setLoadError` land through a passive effect
// (`onToolbarChange`, ProofDocument.tsx) that React flushes on its own schedule, one tick behind
// the DOM commit that shows the alert. Under load, a bare `findByRole` can observe the alert
// once it commits without that effect having run yet, so `toolbar.current` still reads
// "connecting" ("expected failed, received connecting" -- a real flake on this box, not a wall-
// clock timeout). Mirroring the component's own `<promise>.then(...).catch(reportLoadFailure)`
// chain shape here, inside `act`, drives React to the same state the component reaches and
// flushes the resulting effect before either assertion below runs -- no retry, no widened
// timeout, no relaxed assertion.
export async function flushLoadFailure(rejection: Promise<unknown>): Promise<void> {
  await act(async () => {
    await rejection.then(() => undefined).catch(() => undefined);
  });
}
