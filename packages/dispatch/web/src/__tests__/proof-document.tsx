import { afterEach, beforeEach, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../api/client";
import type { Artifact, ArtifactText } from "../api/types";
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
  queryClient = createQueryClient(),
  showDiff = false,
  version,
}: {
  document?: Artifact;
  fake?: FakeDocumentRuntime;
  highlightTerm?: string;
  isClosed?: boolean;
  queryClient?: QueryClient;
  showDiff?: boolean;
  version?: number;
} = {}) {
  const margin = {
    current: undefined as
      | {
          blockFilterId: string | undefined;
          focusBlock(blockId: string): void;
          documentBridge:
            | { focusMark(markId: string): void; setActiveMarks(markIds: string[]): void }
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
  const onToolbarChange = (next: DocumentToolbar) => {
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
              onToolbarChange={onToolbarChange}
              owner={{ key: "CORE-1", kind: "issue" }}
              onVersionChange={onVersionChange}
              showDiff={next.showDiff ?? showDiff}
              user={{ kind: "user", login: "alice" }}
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
