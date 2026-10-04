import { expect, spyOn, test } from "bun:test";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  createQueryClient,
  flushLoadFailure,
  renderProofDocument,
} from "../../__tests__/proof-document";
import { ApiError, api } from "../../api/client";
import type { ArtifactText } from "../../api/types";

// Whether the live editor may connect, and what the page shows when it may not or cannot: the
// admission's own `/text` read, a socket the server refuses as outside the Proof schema, the repair
// and rebuild it offers, and a transport or editor that fails to load.

answerTextReadsWithTheSeededText();

// The repair replaces this document, so it uploads under the document's own name whatever the
// picked file is called (a second `spec (1).md` would leave `spec.md` unreadable), and the read
// that follows the upload admits the editor again.
test("ProofDocument repairs a document outside the schema by an upload under its own name", async () => {
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, {
      code: "DOC_SCHEMA",
      error:
        "document is outside the Proof schema; replace the document from markdown to repair it",
    })
  );
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact,
    version: {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-10-02T00:00:00Z",
      named: true,
      number: 2,
      summary: null,
    },
  });
  try {
    const { connections, view } = renderProofDocument();

    expect(
      await screen.findByText(
        "document is outside the Proof schema; replace the document from markdown to repair it"
      )
    ).not.toBeNull();
    expect(connections).toHaveLength(0);
    const picked = new File(["repaired\n"], "spec (1).md", { type: "text/markdown" });
    fireEvent.change(screen.getByLabelText("Upload artifact"), { target: { files: [picked] } });
    getArtifactText.mockResolvedValue({ markdown: "repaired\n", version: 2 });
    fireEvent.click(await screen.findByRole("button", { name: "Upload" }));
    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    expect(uploadArtifact.mock.calls[0]).toEqual([
      { issue: "CORE-1" },
      { file: picked, name: "spec.md", summary: undefined },
    ]);
    await waitFor(() => expect(connections).toHaveLength(1));
    expect(screen.queryByText("Upload markdown to replace and repair this document.")).toBeNull();
    view.unmount();
  } finally {
    getArtifactText.mockRestore();
    uploadArtifact.mockRestore();
  }
});

test("ProofDocument waits for a fresh text read before reconnecting from cached text", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "cached document",
    version: null,
  });
  let resolveText: (value: ArtifactText) => void;
  const textRead = new Promise<ArtifactText>((resolve) => {
    resolveText = resolve;
  });
  const getArtifactText = spyOn(api, "getArtifactText").mockImplementation(() => textRead);
  try {
    const { connections, view } = renderProofDocument({ queryClient });

    await waitFor(() => expect(getArtifactText).toHaveBeenCalledTimes(1));
    expect(connections).toHaveLength(0);

    await act(async () => {
      resolveText({ markdown: "fresh document", version: null });
      await textRead;
    });
    await waitFor(() => expect(connections).toHaveLength(1));
    view.unmount();
  } finally {
    getArtifactText.mockRestore();
  }
});

// An artifact event invalidates the document's queries, its text read among them, every few
// seconds while anyone edits. The fresh read gates admission only: a refetch after the editor
// connects leaves that editor and its socket alone.
test("ProofDocument keeps its connected editor through a later text refetch", async () => {
  const queryClient = createQueryClient();
  const { connections, editors, sync, view } = renderProofDocument({ queryClient });
  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const getArtifactText = spyOn(api, "getArtifactText");
    const readsBefore = getArtifactText.mock.calls.length;
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id, "text"] });
    });
    expect(getArtifactText.mock.calls.length).toBeGreaterThan(readsBefore);
    expect(connections).toHaveLength(1);
    expect(connections[0]?.destroyed).toBe(false);
    expect(editors[0]?.destroyed).toBe(false);
  } finally {
    view.unmount();
  }
});

// A refetch of the text that fails - a server error, a store outage the server answers 503, or a
// network that drops - says nothing about the connection the editor already holds, which may be
// carrying edits made while its socket was down. The editor and its socket stay; only an
// admission's own read decides whether to connect.
test.each([
  ["a server error", new ApiError(500, { code: "INTERNAL", error: "internal server error" })],
  [
    "a store outage",
    new ApiError(503, {
      code: "DOC_SERVICE_UNAVAILABLE",
      error: "document service is unavailable",
    }),
  ],
  ["a network that drops", new TypeError("Failed to fetch")],
])("ProofDocument keeps its connected editor when a later text refetch meets %s", async (_name, failure) => {
  const queryClient = createQueryClient();
  const { connections, editors, sync, toolbar, view } = renderProofDocument({ queryClient });
  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(failure);
    const readsBefore = getArtifactText.mock.calls.length;
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id, "text"] });
    });
    expect(getArtifactText.mock.calls.length).toBeGreaterThan(readsBefore);
    expect(queryClient.getQueryState(["artifact", artifact.id, "text"])?.status).toBe("error");
    expect(connections).toHaveLength(1);
    expect(connections[0]?.destroyed).toBe(false);
    expect(editors[0]?.destroyed).toBe(false);
    expect(within(view.container).queryByRole("alert")).toBeNull();
    expect(within(view.container).getByRole("article").hidden).toBe(false);
    expect(toolbar.current?.connection).not.toBe("failed");
  } finally {
    view.unmount();
  }
});

// The server refuses a socket into a room outside the Proof schema. The mounted editor closes, and
// a fresh read decides what follows: the repair it names, or a new connection once it reads.
test("ProofDocument closes a refused socket and shows the repair a fresh read names", async () => {
  const { connections, editors, refuse, sync, view } = renderProofDocument();
  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    spyOn(api, "getArtifactText").mockRejectedValue(
      new ApiError(409, {
        code: "DOC_SCHEMA",
        error:
          "document is outside the Proof schema; replace the document from markdown to repair it",
      })
    );
    act(() => refuse());

    expect(
      await screen.findByText(
        "document is outside the Proof schema; replace the document from markdown to repair it"
      )
    ).not.toBeNull();
    expect(connections).toHaveLength(1);
    expect(connections[0]?.destroyed).toBe(true);
    expect(editors[0]?.destroyed).toBe(true);
  } finally {
    view.unmount();
  }
});

test("ProofDocument reconnects after a refused socket when the fresh read succeeds", async () => {
  const { connections, editors, refuse, sync, view } = renderProofDocument();
  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => refuse());

    await waitFor(() => expect(connections).toHaveLength(2));
    expect(connections[0]?.destroyed).toBe(true);
    expect(editors[0]?.destroyed).toBe(true);
    expect(connections[1]?.destroyed).toBe(false);
  } finally {
    view.unmount();
  }
});

/** The timers a refused socket schedules, held rather than run: `refuse` closes the socket the
 * server refused, and the page decides then, synchronously, when it reads the text again. */
function refuseHoldingTimers(refuse: () => void): { delays: number[]; release(): void } {
  const held: Array<() => void> = [];
  const delays: number[] = [];
  const setTimeoutSpy = spyOn(window, "setTimeout").mockImplementation(((
    handler: TimerHandler,
    delay?: number
  ) => {
    if (typeof handler === "function") {
      held.push(handler as () => void);
    }
    delays.push(delay ?? 0);
    return 0;
  }) as typeof window.setTimeout);
  try {
    act(() => refuse());
  } finally {
    setTimeoutSpy.mockRestore();
  }
  return {
    delays,
    release() {
      for (const handler of held) {
        handler();
      }
    },
  };
}

// A socket the server keeps refusing while the text reads is retried after 0, 1, 2, 4 and 8
// seconds, and the sixth refusal in a row stops reconnecting and says so.
test("ProofDocument backs off from a socket the server keeps refusing and stops at the sixth", async () => {
  const { connections, refuse, sync, toolbar, view } = renderProofDocument();
  try {
    await sync();
    for (const [index, delay] of [0, 1_000, 2_000, 4_000, 8_000].entries()) {
      const refusal = refuseHoldingTimers(refuse);
      expect(refusal.delays).toEqual([delay]);
      expect(connections).toHaveLength(index + 1);
      act(() => refusal.release());
      await waitFor(() => expect(connections).toHaveLength(index + 2));
    }
    expect(refuseHoldingTimers(refuse).delays).toEqual([]);
    expect(
      await within(view.container).findByText(
        "This document could not load: the server keeps refusing its connection; reload the page to try again"
      )
    ).not.toBeNull();
    expect(connections).toHaveLength(6);
    expect(connections.every((connection) => connection.destroyed)).toBe(true);
    await waitFor(() => expect(toolbar.current?.connection).toBe("failed"));
  } finally {
    view.unmount();
  }
});

// A socket that syncs ends the run of refusals, so a later refusal starts the backoff again rather
// than counting toward the stop.
test("ProofDocument starts the refusal backoff again once a socket syncs", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const { connections, refuse, sync, view } = renderProofDocument({ fake });
  try {
    await sync();
    for (const [index, delay] of [0, 1_000].entries()) {
      const refusal = refuseHoldingTimers(refuse);
      expect(refusal.delays).toEqual([delay]);
      act(() => refusal.release());
      await waitFor(() => expect(connections).toHaveLength(index + 2));
    }
    act(() => fake.sync());
    expect(refuseHoldingTimers(refuse).delays).toEqual([0]);
  } finally {
    view.unmount();
  }
});

// Only a stored history that cannot load is rebuilt: the page offers the rebuild for the code the
// server answers that state with, and the read after the rebuild admits the editor again.
test("ProofDocument offers a rebuild for a history that cannot load and reconnects after it", async () => {
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, {
      code: "DOCUMENT_UNLOADABLE",
      error: "the document's stored history cannot load; rebuild it from its latest saved version",
    })
  );
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockResolvedValue({
    head: 1,
    removed_checkpoints: 0,
    removed_snapshots: 0,
    removed_updates: 3,
    source_version: 1,
    validation_error: "decode live document: unexpected end of input",
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const { connections, view } = renderProofDocument();
  try {
    expect(
      await screen.findByText(
        "the document's stored history cannot load; rebuild it from its latest saved version"
      )
    ).not.toBeNull();
    expect(connections).toHaveLength(0);
    getArtifactText.mockResolvedValue({ markdown: "The live document", version: 1 });
    fireEvent.click(screen.getByRole("button", { name: "Rebuild from the latest version" }));
    await waitFor(() => expect(rebuildArtifact).toHaveBeenCalledTimes(1));
    expect(rebuildArtifact.mock.calls[0]).toEqual([artifact.id]);
    await waitFor(() => expect(connections).toHaveLength(1));
    expect(screen.queryByRole("button", { name: "Rebuild from the latest version" })).toBeNull();
  } finally {
    view.unmount();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
  }
});

// A room or store that could not serve the document is not a history to discard: the page says
// the document could not load, and offers no rebuild.
test("ProofDocument offers no rebuild for a document service that is unavailable", async () => {
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(503, {
      code: "DOC_SERVICE_UNAVAILABLE",
      error: "document service is unavailable",
    })
  );
  const { connections, view } = renderProofDocument();
  try {
    expect(
      await within(view.container).findByText(
        "This document could not load: document service is unavailable"
      )
    ).not.toBeNull();
    expect(connections).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "Rebuild from the latest version" })).toBeNull();
  } finally {
    view.unmount();
    getArtifactText.mockRestore();
  }
});

test("a transport that fails to load is reported instead of connecting forever", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const rejection = Promise.reject(new TypeError("Failed to fetch"));
  rejection.catch(() => undefined); // avoid an unhandled-rejection warning before the fake below is read
  fake.runtime.loadTransport = () => rejection;
  const { toolbar, view } = renderProofDocument({ fake });

  try {
    await flushLoadFailure(rejection);
    const alert = within(view.container).getByRole("alert");
    expect(alert.textContent).toBe("This document could not load: Failed to fetch");
    expect(toolbar.current?.connection).toBe("failed");
  } finally {
    view.unmount();
  }
});

test("an editor that fails to load after sync is reported the same way, and later provider status stays out of the way", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const rejection = Promise.reject(new Error("editor chunk missing"));
  rejection.catch(() => undefined); // avoid an unhandled-rejection warning before the fake below is read
  fake.runtime.createEditor = () => rejection;
  const { status, sync, toolbar, view } = renderProofDocument({ fake });

  try {
    await sync();
    await flushLoadFailure(rejection);
    const alert = within(view.container).getByRole("alert");
    expect(alert.textContent).toBe("This document could not load: editor chunk missing");
    expect(toolbar.current?.connection).toBe("failed");
    // The live connection is still up and may reconnect; its dot must not contradict the alert.
    act(() => status("connected"));
    expect(toolbar.current?.connection).toBe("failed");
    expect(within(view.container).getByRole("alert").textContent).toContain("editor chunk missing");
  } finally {
    view.unmount();
  }
});

test("ProofDocument makes a schema-read-only admission non-editable and reloadable", async () => {
  const { admit, connections, editors, sync, view } = renderProofDocument();

  try {
    await waitFor(() => expect(connections).toHaveLength(1));
    act(() => admit(true));
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.readOnly).toBe(true);
    expect(within(view.container).getByRole("article").getAttribute("data-read-only")).toBe("true");
    expect(within(view.container).getByText("Reload to edit.")).toBeDefined();
  } finally {
    view.unmount();
  }
});
