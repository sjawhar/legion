import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import type { Artifact } from "../../api/types";
import { MarginProvider } from "../margin/Margin";
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
              user={{ kind: "user", login: "alice" }}
            />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    runtime.sync();
    await waitFor(() => expect(runtime.editors).toHaveLength(1));
    expect(runtime.connections).toHaveLength(1);
    expect(runtime.editors[0]?.options.awareness).toBeNull();
  } finally {
    view.unmount();
    if (clientWidth === undefined) {
      Reflect.deleteProperty(document.documentElement, "clientWidth");
    } else {
      Object.defineProperty(document.documentElement, "clientWidth", clientWidth);
    }
  }
});
