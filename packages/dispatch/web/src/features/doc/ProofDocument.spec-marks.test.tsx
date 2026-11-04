import { expect, spyOn, test } from "bun:test";
import { cleanup, waitFor, within } from "@testing-library/react";

import {
  answerTextReadsWithTheSeededText,
  artifact,
  createQueryClient,
  renderProofDocument,
} from "../../__tests__/proof-document";
import { answerRoute } from "../../__tests__/send-answers";
import { api, type WriteBounds } from "../../api/client";
import type {
  Artifact,
  ArtifactBlock,
  DocumentRead,
  GraphReferences,
  IssueDecisions,
  Version,
} from "../../api/types";
import { changedSinceReadMark, staleBlockMark } from "../../theme/classes";
import { STALE_BLOCK_TITLE } from "./spec-marks";

answerTextReadsWithTheSeededText();

// ---------------------------------------------------------------------------------------------
// The Spec tab's marks: each block a decision shaped that a later change of the decision left
// out of date, and each block that changed since the version the person last read, set through
// the editor handle (never written into the editor's DOM), the one line naming the changed
// sections above the document, and the read mark the tab then records.
// ---------------------------------------------------------------------------------------------

function version(number: number): Version {
  return {
    authors: [],
    created_at: `2026-10-0${number}T00:00:00Z`,
    named: false,
    number,
    summary: null,
  };
}

const spec: Artifact = { ...artifact, versions: [1, 2, 3, 4].map(version) };

const decisions: IssueDecisions = {
  decisions: [
    {
      asks: [],
      block_id: "d-1",
      changed_at: "2026-10-04T00:00:00Z",
      changed_version: 3,
      heading: "Ship in two steps",
      kept: null,
      kind: "agent",
      links: [
        {
          created_at: "2026-10-01T00:00:00Z",
          created_version: 1,
          from_block: "d-1-para",
          stale: true,
          to: { block_id: "p-plan", id: "CORE-1/spec#p-plan", kind: "block" },
        },
      ],
      ref: "dispatch://CORE-1/spec#b-d-1",
      replaced: false,
      text: "Ship it in two steps.",
      text_sha256: "sum-d-1",
      unsure: false,
      waiting_review: true,
    },
  ],
  document: "dispatch://CORE-1/spec",
  open_questions: [],
};

function part(blockId: string, changedVersion: number, text: string) {
  return {
    created_at: "2026-10-01T00:00:00Z",
    direction: "in" as const,
    excerpt: { block_id: blockId, text },
    kind: "part_of",
    node: {
      block_id: blockId,
      changed_version: changedVersion,
      id: `CORE-1/spec#${blockId}`,
      kind: "block" as const,
    },
    source_seq: null,
  };
}

const parts: GraphReferences = {
  edges: [
    part("h-plan", 1, "## Plan"),
    part("p-plan", 2, "Ship it in two steps."),
    part("h-rollout", 1, "## Rollout"),
    part("p-rollout", 4, "Behind a flag."),
  ],
  node: { id: "artifact-1", kind: "artifact" },
};

function block(id: string, type: string, from: number): ArtifactBlock {
  return { from, id, references: { asks: 0, comments: 0 }, to: from + 10, type };
}

const blocks = [
  block("h-plan", "heading", 0),
  block("p-plan", "paragraph", 10),
  block("h-rollout", "heading", 20),
  block("p-rollout", "paragraph", 30),
];

function readOf(version: number | null): DocumentRead {
  return {
    artifact_id: "artifact-1",
    read_at: version === null ? null : "2026-10-02T00:00:00Z",
    version,
  };
}

function stubReads(readVersion: number | null) {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(spec);
  const getIssueDecisions = spyOn(api, "getIssueDecisions").mockResolvedValue(decisions);
  const getDocumentRead = spyOn(api, "getDocumentRead").mockResolvedValue(readOf(readVersion));
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(parts);
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue(blocks);
  const putRead = answerRoute<
    (id: string, body: { version: number }, bounds: WriteBounds) => Promise<unknown>
  >("PUT", "/api/v1/me/documents/*/read");
  putRead.mockImplementation(async (_id, body) => readOf(body.version));
  return {
    getDocumentRead,
    getIssueDecisions,
    getReferences,
    putRead,
    restore() {
      for (const spy of [
        getArtifact,
        getIssueDecisions,
        getDocumentRead,
        getReferences,
        getArtifactBlocks,
      ]) {
        spy.mockRestore();
      }
    },
  };
}

test("the Spec tab marks stale and changed blocks through the editor, names the changed sections, then records the version it shows", async () => {
  const reads = stubReads(2);
  const { editors, sync, view } = renderProofDocument({
    document: spec,
    queryClient: createQueryClient(),
  });
  const screen = within(view.container);

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");

    // The read version is the one the person had before this visit: the blocks above it are
    // marked, and the line names their sections.
    const line = await screen.findByText("Changed since you last read version 2: Rollout");
    expect(line.getAttribute("role")).toBe("status");
    await waitFor(() =>
      expect(editor.blockMarks.at(-1)).toEqual([
        { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
        { blockId: "p-rollout", className: changedSinceReadMark },
      ])
    );
    expect(reads.getIssueDecisions).toHaveBeenCalledWith("CORE-1");
    expect(reads.getDocumentRead).toHaveBeenCalledWith("artifact-1");
    expect(reads.getReferences).toHaveBeenCalledWith("dispatch://CORE-1/spec", {
      kind: "part_of",
    });
    // No class is written into the editor's DOM: every mark goes through the handle.
    expect(editor.root.querySelector(".dispatch-block-stale")).toBeNull();

    // Then the tab records the version it shows, once.
    await waitFor(() => expect(reads.putRead).toHaveBeenCalledTimes(1));
    expect(reads.putRead.mock.calls[0]?.slice(0, 2)).toEqual(["artifact-1", { version: 4 }]);

    // The marks stay for the visit, though the person's read version is now the latest.
    await Bun.sleep(20);
    expect(screen.getByText("Changed since you last read version 2: Rollout")).toBeDefined();
    expect(editor.blockMarks.at(-1)).toEqual([
      { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
      { blockId: "p-rollout", className: changedSinceReadMark },
    ]);
  } finally {
    cleanup();
    reads.restore();
  }
});

test("with no read version the Spec tab shows no line and marks no changed block, and still records its read", async () => {
  const reads = stubReads(null);
  const { editors, sync, view } = renderProofDocument({
    document: spec,
    queryClient: createQueryClient(),
  });
  const screen = within(view.container);

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");

    await waitFor(() => expect(reads.putRead).toHaveBeenCalledTimes(1));
    expect(reads.putRead.mock.calls[0]?.slice(0, 2)).toEqual(["artifact-1", { version: 4 }]);
    await waitFor(() =>
      expect(editor.blockMarks.at(-1)).toEqual([
        { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
      ])
    );
    expect(screen.queryByText(/Changed since you last read/)).toBeNull();
  } finally {
    cleanup();
    reads.restore();
  }
});
