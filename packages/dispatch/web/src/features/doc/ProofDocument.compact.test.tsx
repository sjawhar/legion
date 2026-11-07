import { beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { answerRequests } from "../../__tests__/network";
import {
  artifact,
  createQueryClient,
  documentSurfaceHandlers,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import { MarginProvider } from "../margin/margin-context";
import { ProofDocument } from "./ProofDocument";
import { DocumentRuntime } from "./runtime";

beforeEach(() => {
  answerRequests(...documentSurfaceHandlers());
});

test("ProofDocument keeps transport active without cursor decorations in the compact layout", async () => {
  const clientWidth = Object.getOwnPropertyDescriptor(document.documentElement, "clientWidth");
  Object.defineProperty(document.documentElement, "clientWidth", {
    configurable: true,
    value: 390,
  });
  const runtime = fakeDocumentRuntime();
  const queryClient = createQueryClient();
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
