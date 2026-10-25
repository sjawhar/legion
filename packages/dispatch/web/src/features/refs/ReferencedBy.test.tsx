import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { GraphEdge, GraphReferences } from "../../api/types";
import { ReferencedBy } from "./ReferencedBy";

const createdAt = "2026-10-20T10:00:00Z";

function blockEdge(
  document: string,
  issueKey: string,
  blockId: string,
  encoded: string,
  text: string
) {
  return {
    created_at: createdAt,
    direction: "in",
    excerpt: { block_id: blockId, text },
    kind: "mentions",
    node: {
      block_id: blockId,
      document,
      id: `${document.slice("dispatch://".length)}#${blockId}`,
      issue_key: issueKey,
      kind: "block",
      project: issueKey.split("-")[0],
      ref: `${document}#b-${encoded}`,
    },
    source_seq: 7,
  } satisfies GraphEdge;
}

function renderPanel(read: GraphReferences) {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(read);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <ReferencedBy reference="dispatch://CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getReferences, view };
}

let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

test("two blocks of one document version citing the node are two rows under that document, each linked to its block", async () => {
  const notes = "dispatch://OPS-1/artifact/notes-md";
  const consoleError = spyOn(console, "error");
  const { getReferences, view } = renderPanel({
    edges: [
      blockEdge(notes, "OPS-1", "ship_it", "ship%5Fit", "Ship it as dispatch://CORE-1 says."),
      blockEdge(notes, "OPS-1", "p2", "p2", "And hold the rest for dispatch://CORE-1."),
    ],
    node: { id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: "dispatch://CORE-1" },
  });
  restore = () => {
    getReferences.mockRestore();
    consoleError.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByText("Ship it as dispatch://CORE-1 says.");
  expect(within(panel).getByRole("heading").textContent).toBe("Referenced by (2)");
  // One source: the document, its header linked to the document itself.
  const sources = panel.querySelectorAll(":scope > ul > li");
  expect(sources).toHaveLength(1);
  const source = within(sources[0] as HTMLElement);
  expect(source.getByRole("link", { name: "Artifact · OPS-1" }).getAttribute("href")).toBe(
    "/issues/OPS-1/artifacts/notes-md"
  );
  // Each block its own row, with its own excerpt and a link to that block, its id encoded as
  // encodeBlockId writes it.
  const rows = (sources[0] as HTMLElement).querySelectorAll(":scope > ul > li");
  expect(rows).toHaveLength(2);
  expect(
    [...rows].map((row) => [
      row.querySelector("p")?.textContent,
      row.querySelector("a")?.getAttribute("href"),
    ])
  ).toEqual([
    ["Ship it as dispatch://CORE-1 says.", "/issues/OPS-1/artifacts/notes-md#b-ship%5Fit"],
    ["And hold the rest for dispatch://CORE-1.", "/issues/OPS-1/artifacts/notes-md#b-p2"],
  ]);
  // Rows written in one version share their time, and still have keys of their own.
  expect(consoleError.mock.calls.some((call) => String(call[0]).includes("same key"))).toBeFalse();
});

test("a source of one citing node links to where its mention is: a block at the block, a document at its excerpt's block", async () => {
  const { getReferences, view } = renderPanel({
    edges: [
      blockEdge("dispatch://OPS-1/spec", "OPS-1", "ship_it", "ship%5Fit", "Ship it."),
      {
        created_at: createdAt,
        direction: "in",
        excerpt: { block_id: "hold_it", text: "Hold it." },
        kind: "mentions",
        node: {
          id: "CORE/handbook-md",
          kind: "artifact",
          project: "CORE",
          ref: "dispatch://CORE/artifact/handbook-md",
        },
        source_seq: 8,
      },
    ],
    node: { id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: "dispatch://CORE-1" },
  });
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByText("Hold it.");
  expect(within(panel).getByRole("link", { name: "Artifact · OPS-1" }).getAttribute("href")).toBe(
    "/issues/OPS-1/spec#b-ship%5Fit"
  );
  expect(within(panel).getByRole("link", { name: "Artifact · CORE" }).getAttribute("href")).toBe(
    "/projects/CORE/documents/handbook-md#b-hold%5Fit"
  );
  // A one-node source's header is its only link.
  expect(within(panel).getAllByRole("link")).toHaveLength(2);
});
