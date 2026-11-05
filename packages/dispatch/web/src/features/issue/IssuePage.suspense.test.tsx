// Each seam of the issue page's mount: the part that is not the document lands in a commit of
// its own, after the commit that holds the document, and it is there, as it is today, when that
// commit lands (dispatch://INFRA-341, R11).
//
// The page's mount used to be one commit - the header, the tab strip, the side lists and the
// document surface together - and a commit is the one thing a transition cannot break up, so
// that commit held the main thread for about 0.7 s on a phone at 4x CPU throttling. Each
// `Suspense` boundary in `IssuePage.tsx` and `ArtifactRoutePanel.tsx` makes a commit of its own
// out of one part, with the document's in front.
//
// Which commit a part landed in is read from React itself: a `Profiler` around the page records
// what the DOM held at each commit, in that commit's own layout phase. Reading the DOM after
// `render` returns would not do it - by then every boundary has resolved and every commit has
// landed, which is why the page looks the same to the reader either way.
//
// The page is mounted twice for the whole file, once per address, and every row reads those two
// mounts. `lazy` keeps a chunk it has loaded for the life of the page, so only the first mount
// of each part suspends at all - which is the mount a reader makes, and the one the targets are
// about.
import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { Profiler, type ProfilerOnRenderCallback } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import type { Artifact, IssueCoverage, IssueDetails } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { IssuePage } from "./IssuePage";

const specArtifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  slug: "spec",
  versions: [],
};

const notesArtifact: Artifact = {
  ...specArtifact,
  id: "artifact-2",
  name: "notes.md",
  primary: false,
  slug: "notes",
};

const issue: IssueDetails = {
  artifacts: [specArtifact, notesArtifact],
  assignee: null,
  blocked_by: [],
  children: [],
  claim: null,
  closed_at: null,
  components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 250,
  number: 1,
  open_asks: [],
  parent: null,
  primary_artifact_id: "artifact-1",
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
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
};

const requirementText = "The ledger settles every lease.";

const coverage: IssueCoverage = {
  gaps: [],
  has_plan: false,
  issue: "CORE-1",
  requirements: [
    {
      block_id: "block-1",
      checks: 1,
      gaps: [],
      plan_tasks: 1,
      ref: "dispatch://CORE-1/spec#b-block-1",
      text: requirementText,
    },
  ],
  spec_artifact_id: "artifact-1",
};

/** What each part of the page leaves in the DOM, so a commit can be read for it. A part is
 *  named by what the reader sees, never by a class the build renames. */
const partsOfThePage = {
  /** The artifact page's side list: an artifact's versions and its compare. */
  artifactDetails: () => document.querySelector('[aria-label="Versions for notes.md"]') !== null,
  /** The part the reader came for: the panel the document is drawn into. */
  document: () => document.getElementById("issue-spec-panel") !== null,
  /** The header itself rather than its box: the box's `h1` holds no title to read. */
  header: () => screen.queryByRole("heading", { level: 1, name: issue.title }) !== null,
  /** The spec's side list: the requirements under the document. */
  requirements: () => screen.queryByTestId("spec-requirements") !== null,
  /** The tab strip itself rather than its box: the box's labels are spans and answer no click. */
  tabs: () => screen.queryAllByRole("tab").length > 0,
} as const;

type PartName = keyof typeof partsOfThePage;

/** What one commit of the page held. */
type Commit = Record<PartName, boolean>;

function commitNow(): Commit {
  return Object.fromEntries(
    Object.entries(partsOfThePage).map(([part, inTheDom]) => [part, inTheDom()])
  ) as Commit;
}

/** What the reader has in front of them once every commit of the mount has landed: each part as
 *  it is drawn today, read while the page is still mounted. */
interface Settled {
  readonly headingText: string | undefined;
  readonly pinDisabled: boolean;
  readonly requirementsText: string | undefined;
  readonly requirementsUnderTheDocument: boolean;
  readonly specTabId: string | undefined;
  readonly tabLabels: readonly string[];
  readonly versionsText: string | undefined;
}

interface Mount {
  /** Every commit the page made, in order; the first is the commit that mounts it. */
  readonly commits: readonly Commit[];
  /** What the two boxes held in the first commit, before the parts replaced them. */
  readonly firstCommitHadTheBoxes: boolean;
  readonly settled: Settled;
}

/** Which commit a part first landed in, counting the page's first commit as 0; `undefined` for
 *  a part no commit of the mount held. */
function landedIn(mount: Mount, part: PartName): number | undefined {
  const index = mount.commits.findIndex((commit) => commit[part]);
  return index === -1 ? undefined : index;
}

/** Whatever the page reads beyond the four this file seeds, answered at once so no read holds a
 *  commit up. `preconnect` is on it because `typeof fetch` names it and the page never calls
 *  it. */
function notFound() {
  const respond = async () =>
    new Response("{}", { headers: { "content-type": "application/json" }, status: 404 });
  respond.preconnect = () => {};
  return respond;
}

/**
 * Mounts the page at `path` with every read it makes answered - the issue, the reader's state
 * and the coverage from the cache, anything else from a `fetch` that answers 404 at once - so no
 * read holds a commit up and every commit recorded is one a boundary made.
 *
 * `parts` names every part that address draws, and the mount is read once each of them is on
 * the page.
 */
async function mountPage(path: string, ...parts: readonly PartName[]): Promise<Mount> {
  const fetched = spyOn(globalThis, "fetch").mockImplementation(notFound());
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(["issue", "CORE-1"], issue);
  queryClient.setQueryData(["user-state"], { "CORE-1": { pins: [], read_seq: 250 } });
  queryClient.setQueryData(["issue", "CORE-1", "coverage"], coverage);
  const runtime = fakeDocumentRuntime({ text: "# The lease ledger\n" });
  const commits: Commit[] = [];
  const record: ProfilerOnRenderCallback = () => {
    commits.push(commitNow());
  };
  render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DocumentRuntime.Provider value={runtime.runtime}>
            <MarginProvider>
              <Profiler id="issue-page" onRender={record}>
                <Routes>
                  <Route element={<IssuePage />} path="/issues/:key/*" />
                </Routes>
              </Profiler>
            </MarginProvider>
          </DocumentRuntime.Provider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  // The boxes `issue-page-skeleton.tsx` draws, which the reader already had in front of them:
  // a Pin that answers nothing and a strip of labels that is no tab.
  const firstCommitHadTheBoxes =
    screen.queryByRole("button", { name: "Pin issue" })?.hasAttribute("disabled") === true &&
    screen.queryByRole("tablist") !== null &&
    screen.queryAllByRole("tab").length === 0;

  await waitFor(() => {
    for (const part of parts) expect(partsOfThePage[part]()).toBe(true);
  });
  const requirements = screen.queryByTestId("spec-requirements");
  const settled: Settled = {
    headingText: screen.queryByRole("heading", { level: 1, name: issue.title })?.textContent,
    pinDisabled: screen.getByRole("button", { name: "Pin issue" }).hasAttribute("disabled"),
    requirementsText: requirements?.textContent ?? undefined,
    requirementsUnderTheDocument:
      requirements !== null &&
      document.getElementById("issue-spec-panel")?.contains(requirements) === true,
    specTabId: screen.queryByRole("tab", { name: /^Spec$/ })?.id,
    tabLabels: screen.queryAllByRole("tab").map((tab) => tab.textContent ?? ""),
    versionsText:
      screen.queryByRole("region", { name: "Versions for notes.md" })?.textContent ?? undefined,
  };
  cleanup();
  fetched.mockRestore();
  return { commits, firstCommitHadTheBoxes, settled };
}

let mounted: { artifact: Mount; issue: Mount } | undefined;

/** The two mounts every row reads, made once and in this order: the issue's address first, so
 *  each part of it suspends there rather than in a mount that already holds its chunk. */
async function theMounts(): Promise<{ artifact: Mount; issue: Mount }> {
  if (mounted === undefined) {
    const issueMount = await mountPage("/issues/CORE-1", "header", "tabs", "requirements");
    mounted = {
      artifact: await mountPage("/issues/CORE-1/artifacts/notes", "header", "artifactDetails"),
      issue: issueMount,
    };
  }
  return mounted;
}

test("the document's own part of the page is in the first commit, and nothing else is", async () => {
  const { issue: mount } = await theMounts();

  expect(mount.commits[0]).toEqual({
    artifactDetails: false,
    document: true,
    header: false,
    requirements: false,
    tabs: false,
  });
  expect(mount.firstCommitHadTheBoxes).toBe(true);
  // And the mount is several commits rather than one: every part that is not the document is
  // in a commit after the first, where a commit of its own may hold it or a boundary that
  // resolved in the same task may share one with it.
  expect(mount.commits.length).toBeGreaterThan(1);
});

test("the header lands in a commit after the document's, and is the header the reader has today", async () => {
  const { issue: mount } = await theMounts();

  expect(mount.settled.headingText).toBe(issue.title);
  // Its Pin takes a press, where the box's answered nothing.
  expect(mount.settled.pinDisabled).toBe(false);
  expect(landedIn(mount, "header")).toBeGreaterThan(0);
});

test("the tab strip lands in a commit after the document's, with the tabs it has today", async () => {
  const { issue: mount } = await theMounts();

  expect(mount.settled.specTabId).toBe("issue-spec-tab");
  expect(mount.settled.tabLabels).toEqual([
    "Spec",
    "Verification",
    "Plan",
    "Conversation",
    "Children",
    "Artifacts2",
    "Activity",
    "Decisions",
  ]);
  expect(landedIn(mount, "tabs")).toBeGreaterThan(0);
});

test("the spec's requirements land in a commit after the document's, under the document", async () => {
  const { issue: mount } = await theMounts();

  expect(mount.settled.requirementsText).toContain(requirementText);
  // Under the document, so nothing above them moves as they land.
  expect(mount.settled.requirementsUnderTheDocument).toBe(true);
  expect(landedIn(mount, "requirements")).toBeGreaterThan(0);
});

test("an artifact's details land in a commit after the document's, under the document", async () => {
  const { artifact: mount } = await theMounts();

  expect(mount.settled.versionsText).toContain("Versions");
  expect(landedIn(mount, "artifactDetails")).toBeGreaterThan(0);
});
