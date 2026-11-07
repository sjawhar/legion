import { beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import { http } from "msw";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { answerRequests } from "../../__tests__/network";
import { api } from "../../api/client";
import type { Artifact, DocumentRead, IssueAskRow, IssueDecisions } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { ProofDocument } from "./ProofDocument";
import { DocumentRuntime } from "./runtime";

const artifact: Artifact = {
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

/**
 * The reads the document surface makes of its own record, blocks and read mark, and of the
 * issue's asks and decisions, each answered with the API's empty shape so the page renders its
 * loaded-empty state rather than its error state.
 */
beforeEach(() => {
  answerRequests(
    http.get("/api/v1/artifacts/:id", ({ params }) =>
      params.id === artifact.id
        ? Response.json(artifact)
        : Response.json(
            { code: "ARTIFACT_NOT_FOUND", error: `no artifact ${String(params.id)}` },
            { status: 404 }
          )
    ),
    http.get("/api/v1/artifacts/:id/blocks", () => Response.json([])),
    http.get("/api/v1/issues/:key/asks", () => Response.json([] satisfies IssueAskRow[])),
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
    // Opening a document the viewer has never read records the version they are looking at.
    http.put("/api/v1/me/documents/:id/read", async ({ params, request }) => {
      const { version } = (await request.json()) as { version: number };
      return Response.json({
        artifact_id: String(params.id),
        read_at: "2026-09-09T00:00:00Z",
        version,
      } satisfies DocumentRead);
    })
  );
});

test("ProofDocument keeps transport active without cursor decorations in the compact layout", async () => {
  const clientWidth = Object.getOwnPropertyDescriptor(document.documentElement, "clientWidth");
  Object.defineProperty(document.documentElement, "clientWidth", {
    configurable: true,
    value: 390,
  });
  const runtime = fakeDocumentRuntime();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "",
    version: null,
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={runtime.runtime}>
          <MarginProvider>
            <ProofDocument
              artifact={artifact}
              highlight={undefined}
              isClosed={false}
              owner={{ key: "CORE-1", kind: "issue" }}
              onVersionChange={() => {}}
              showDiff={false}
              user={{ event_head: "0", kind: "user", login: "alice" }}
            />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();
    await waitFor(() => expect(runtime.editors).toHaveLength(1));
    expect(runtime.editors[0]?.options.awareness).toBeNull();
  } finally {
    view.unmount();
    getArtifactText.mockRestore();
    if (clientWidth === undefined) {
      Reflect.deleteProperty(document.documentElement, "clientWidth");
    } else {
      Object.defineProperty(document.documentElement, "clientWidth", clientWidth);
    }
  }
});
