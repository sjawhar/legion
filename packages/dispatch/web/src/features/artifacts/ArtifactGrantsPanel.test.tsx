/**
 * The gate in front of "Data this page may read" (dispatch://INFRA-682, R9): it shows for an
 * issue's or a project's page file and for nothing else, and it tests the file the way the
 * server's `isPageFile` does - over the **latest** version's stored type, whichever version the
 * reader is looking at (packages/envoy/internal/dispatch/api/artifacts.go).
 */
import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, Version } from "../../api/types";
import { ArtifactGrantsPanel } from "./ArtifactGrantsPanel";

function version(number: number, mime: string): Version {
  return {
    authors: [{ id: "alice", kind: "user" }],
    created_at: `2026-11-0${number}T00:00:00Z`,
    mime,
    named: false,
    number,
    sha256: `sha-${number}`,
    size: 10,
    summary: null,
  };
}

function pageFile(over: Partial<Artifact> = {}): Artifact {
  return {
    created_at: "2026-11-01T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: "CORE-1",
    kind: "file",
    name: "chart.bin",
    primary: false,
    project: "CORE",
    slug: "chart-bin",
    versions: [version(1, "text/html; charset=utf-8")],
    ...over,
  };
}

// Every row renders into the one document this file shares with every other test file of the
// run, so each render is taken out again: a panel left behind is another file's extra heading.
afterEach(cleanup);

/** The gate rendered over `artifact`. A person is signed in unless `human` says otherwise, and
 *  the list a shown panel then loads is empty. A hidden panel renders nothing at all, now and
 *  for ever, so its row reads the container as it stands; a shown one arrives with its own
 *  chunk, so its row waits for `shownPanel`. */
function renderPanel(artifact: Artifact, human = true) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["whoami"], {
    event_head: "0",
    kind: human ? "user" : "session",
    login: "alice",
  });
  spyOn(api, "listArtifactGrants").mockResolvedValue([]);
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ArtifactGrantsPanel artifact={artifact} />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The panel inside one render's own container, once its chunk is in and the list it read has
 *  landed. Scoped to that container, since nothing clears the document between rows here and a
 *  page-wide query would find every panel a row before it drew. */
async function shownPanel(container: HTMLElement): Promise<HTMLElement> {
  const panel = await within(container).findByRole("region", {
    name: "Data this page may read",
  });
  await within(container).findByText("No data file is granted to this page.");
  return panel;
}

test("the panel shows for a file whose latest version is stored as HTML", async () => {
  const { container } = renderPanel(pageFile());

  expect(await shownPanel(container)).toBeTruthy();
});

test("the panel shows on an older version of a file whose latest version is HTML", async () => {
  // The reader may be looking at version 1, a JSON upload; the newest version is the page. The
  // panel is the file's, as the grants are, so it shows whichever version is on screen.
  const { container } = renderPanel(
    pageFile({ versions: [version(1, "application/json"), version(2, "text/html")] })
  );

  expect(await shownPanel(container)).toBeTruthy();
});

test("the panel is hidden for a file whose latest version is no longer HTML", () => {
  // The rule this row holds: the panel used to show here, because version 1 on screen was HTML.
  // The server answers `415 NOT_A_PAGE` for this file now, from version 2's type, and the grants
  // routes would refuse the list the panel asks for, so the panel is hidden.
  const { container } = renderPanel(
    pageFile({ versions: [version(1, "text/html"), version(2, "application/json")] })
  );

  expect(container.innerHTML).toBe("");
});

test("the panel shows for a file named .html whatever its latest version is stored as", async () => {
  const { container } = renderPanel(
    pageFile({ name: "chart.HTML", slug: "chart-html", versions: [version(1, "text/plain")] })
  );

  expect(await shownPanel(container)).toBeTruthy();
});

test("the panel is hidden for a document, an agent conversation's file and a reader who is no person", () => {
  expect(renderPanel(pageFile({ kind: "doc", name: "spec.md" })).container.innerHTML).toBe("");
  expect(
    renderPanel(pageFile({ issue_key: null, project: "", session_id: "session-1" })).container
      .innerHTML
  ).toBe("");
  expect(renderPanel(pageFile(), false).container.innerHTML).toBe("");
});
