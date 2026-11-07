/**
 * What a `view` block gives its page, case by case (dispatch://INFRA-682, "Data grants", R9 and
 * R6): every coverage case of the spec's list - a grant without a version, a grant pinned to
 * one, a reference pinned with `@vN` against each, a file no grant covers, and a page file with
 * no grants at all - and the 16 MiB cap, which only granted files count toward.
 *
 * These are the rules a browser row could only reach through a whole document, a frame and a
 * socket; they are tested here because `planViewData` is pure (`view-data-plan.ts`).
 */
import { expect, test } from "bun:test";

import { ApiError } from "../../api/client";
import type { ArtifactGrant, IssueReferenceSummary } from "../../api/types";
import type { ViewFile } from "./view-attributes";
import {
  type FileState,
  MAX_DATA_BYTES,
  planViewData,
  resolveFile,
  type SizeResult,
} from "./view-data-plan";

const PAGE: FileState = {
  id: "page-1",
  latest: 1,
  name: "chart.html",
  ref: "dispatch://CORE-1/artifact/chart-html",
  size: 100,
  version: 1,
};

/** One data file whose summary read answered: three versions, the newest 10 bytes. */
function dataFile(over: Partial<FileState> = {}): FileState {
  return {
    id: "data-1",
    latest: 3,
    name: "loop.json",
    ref: "dispatch://CORE-1/artifact/loop-json",
    size: 10,
    version: 3,
    ...over,
  };
}

/** A grant of `data-1` to the page file: every version of it, or the one `version` pins. */
function grantOf(version: number | null, id = "grant-1", artifact = "data-1"): ArtifactGrant {
  return {
    artifact_id: artifact,
    created_at: "2026-11-01T00:00:00Z",
    granted_by: "alice",
    id,
    name: "loop.json",
    ref: "dispatch://CORE-1/artifact/loop-json",
    version,
  };
}

/** A size read that was never asked for, which is every file the summary carries the size of. */
const unasked: SizeResult = { data: undefined, error: null, isError: false };

function sizes(...answers: SizeResult[]): SizeResult[] {
  return [unasked, ...answers];
}

const noReadCauses: ReadonlyMap<string, string> = new Map();

function plan(
  states: FileState[],
  grants: ArtifactGrant[] | undefined,
  sizing: SizeResult[] = sizes(unasked),
  readCauses: ReadonlyMap<string, string> = noReadCauses
) {
  return planViewData(states, grants, sizing, readCauses);
}

test("a reference without @vN under a grant without a version gets the newest version", () => {
  const { entries, holdData, lines } = plan([PAGE, dataFile()], [grantOf(null)]);

  expect(entries).toEqual([
    {
      id: "data-1",
      name: "loop.json",
      ref: "dispatch://CORE-1/artifact/loop-json",
      version: 3,
    },
  ]);
  expect(lines).toEqual([]);
  expect(holdData).toBe(false);
});

test("a reference without @vN under a grant pinned to a version gets that version", () => {
  const { cover, entries, sendSignature } = plan(
    [PAGE, dataFile()],
    [grantOf(1)],
    // Version 1 is not the latest, so its size is the one the `HEAD` answered.
    sizes({ data: 40, error: null, isError: false })
  );

  expect(entries[0].version).toBe(1);
  expect(cover).toBe(" grant-1");
  expect(sendSignature).toBe(" dispatch://CORE-1/artifact/loop-json@1");
});

test("a reference without @vN under two pinned grants gets the newest version they cover", () => {
  const { entries } = plan(
    [PAGE, dataFile()],
    [grantOf(1), grantOf(2, "grant-2")],
    sizes({ data: 40, error: null, isError: false })
  );

  expect(entries[0].version).toBe(2);
});

test("a reference pinned @v2 under a grant without a version gets version 2", () => {
  const { entries } = plan(
    [PAGE, dataFile({ pinned: 2, version: 2 })],
    [grantOf(null)],
    sizes({ data: 40, error: null, isError: false })
  );

  expect(entries[0].version).toBe(2);
  expect(entries[0].error).toBeUndefined();
});

test("a reference pinned @v2 under a grant pinned to the same version gets version 2", () => {
  const { entries } = plan(
    [PAGE, dataFile({ pinned: 2, version: 2 })],
    [grantOf(2)],
    sizes({ data: 40, error: null, isError: false })
  );

  expect(entries[0].version).toBe(2);
});

test("a reference pinned @v2 under a grant pinned to another version is not granted", () => {
  const { cover, entries, lines } = plan([PAGE, dataFile({ pinned: 2, version: 2 })], [grantOf(1)]);

  expect(entries).toEqual([{ error: "not-granted", ref: "dispatch://CORE-1/artifact/loop-json" }]);
  expect(lines).toEqual([
    {
      cause: "not granted to this page",
      panel: true,
      ref: "dispatch://CORE-1/artifact/loop-json",
    },
  ]);
  expect(cover).toBe(" none");
});

test("a file a grant of another file does not cover is not granted", () => {
  const { entries, lines } = plan([PAGE, dataFile()], [grantOf(null, "grant-1", "data-9")]);

  expect(entries[0].error).toBe("not-granted");
  expect(entries[0].text).toBeUndefined();
  expect(lines[0].panel).toBe(true);
});

test("a page file with no grants gets an entry for every file, all not-granted and with no text", () => {
  const other = dataFile({ id: "data-2", ref: "dispatch://CORE-1/artifact/more-json" });
  const { entries, lines, sendSignature } = plan(
    [PAGE, dataFile(), other],
    [],
    sizes(unasked, unasked)
  );

  expect(entries).toEqual([
    { error: "not-granted", ref: "dispatch://CORE-1/artifact/loop-json" },
    { error: "not-granted", ref: "dispatch://CORE-1/artifact/more-json" },
  ]);
  expect(lines).toHaveLength(2);
  expect(sendSignature).toBe(
    " dispatch://CORE-1/artifact/loop-json@not-granted" +
      " dispatch://CORE-1/artifact/more-json@not-granted"
  );
});

test("a block whose grants read has not answered shows no line and names no grant", () => {
  const { cover, entries, holdData, lines } = plan([PAGE, dataFile()], undefined);

  expect(entries[0].error).toBe("not-granted");
  expect(lines).toEqual([]);
  expect(cover).toBe(" none");
  expect(holdData).toBe(false);
});

test("granted data over 16 MiB holds every file back and says so", () => {
  const second = dataFile({ id: "data-2", ref: "dispatch://CORE-1/artifact/huge-json" });
  const grants = [grantOf(null), grantOf(null, "grant-2", "data-2")];
  // Both files' newest versions are what the summary carries, so both are weighed from there.
  const under = plan([PAGE, dataFile(), second], grants, sizes(unasked, unasked));
  expect(under.holdData).toBe(false);
  expect(under.lines).toEqual([]);

  const over = plan(
    [PAGE, dataFile(), { ...second, size: MAX_DATA_BYTES }],
    grants,
    sizes(unasked, unasked)
  );
  expect(over.holdData).toBe(true);
  expect(over.lines).toEqual([
    {
      cause: "the data files together are larger than 16 MiB, so Dispatch sends no data",
      ref: "data",
    },
  ]);
  // The entries are built all the same; the hold is what keeps them from going out.
  expect(over.entries.map((entry) => entry.version)).toEqual([3, 3]);
});

test("a file no grant covers does not count toward 16 MiB", () => {
  const huge = dataFile({
    id: "data-2",
    ref: "dispatch://CORE-1/artifact/huge-json",
    size: MAX_DATA_BYTES + 1,
  });
  const { entries, holdData } = plan(
    [PAGE, dataFile(), huge],
    [grantOf(null)],
    sizes(unasked, unasked)
  );

  expect(entries[1].error).toBe("not-granted");
  expect(holdData).toBe(false);
});

test("the version a grant pins is weighed by its own size read, not the newest version's", () => {
  // The newest version is 10 bytes; version 1, which the grant pins, is over the cap.
  const { holdData, lines } = plan(
    [PAGE, dataFile()],
    [grantOf(1)],
    sizes({ data: MAX_DATA_BYTES + 1, error: null, isError: false })
  );

  expect(holdData).toBe(true);
  expect(lines[0].ref).toBe("data");
});

test("a granted file whose size has not answered holds the data back with no line", () => {
  const { holdData, lines } = plan([PAGE, dataFile()], [grantOf(1)], sizes(unasked));

  expect(holdData).toBe(true);
  expect(lines).toEqual([]);
});

test("a size read that failed names its status and the file goes as unreadable", () => {
  const { entries, lines } = plan(
    [PAGE, dataFile()],
    [grantOf(1)],
    sizes({ data: undefined, error: new ApiError(503, { error: "no" }), isError: true })
  );

  expect(entries[0].error).toBe("unreadable");
  expect(lines).toEqual([
    {
      cause: "Dispatch answered 503 for this file",
      ref: "dispatch://CORE-1/artifact/loop-json",
    },
  ]);
});

test("a read of a granted file that failed is one line beside the file's own entry", () => {
  const { entries, lines } = plan(
    [PAGE, dataFile()],
    [grantOf(null)],
    sizes(unasked),
    new Map([["dispatch://CORE-1/artifact/loop-json", "this file is not UTF-8 text"]])
  );

  expect(entries[0].version).toBe(3);
  expect(lines).toEqual([
    {
      cause: "this file is not UTF-8 text",
      ref: "dispatch://CORE-1/artifact/loop-json",
    },
  ]);
});

test("resolveFile carries the version a reference pins, which is what a grant must cover", () => {
  const file: ViewFile = {
    owner: { issue: "CORE-1" },
    reference: "dispatch://CORE-1/artifact/loop-json@v2",
    route: { kind: "artifact", key: "CORE-1", slug: "loop-json", version: 2 },
    slug: "loop-json",
    version: 2,
  };
  const summary: IssueReferenceSummary = {
    documents: [
      {
        id: "data-1",
        kind: "file",
        latest_version: 3,
        name: "loop.json",
        primary: false,
        size: 10,
        slug: "loop-json",
      },
    ],
    key: "CORE-1",
    project: "CORE",
    status: "todo",
    title: "Kernel boot loop",
  };

  const state = resolveFile(file, { data: summary, error: null, isError: false });

  expect(state.pinned).toBe(2);
  expect(state.version).toBe(2);
  expect(state.latest).toBe(3);

  // A grant pinned to the newest version does not cover a reference pinned to an older one.
  expect(plan([PAGE, state], [grantOf(3)]).entries[0].error).toBe("not-granted");
  expect(
    plan([PAGE, state], [grantOf(2)], sizes({ data: 7, error: null, isError: false })).entries[0]
      .version
  ).toBe(2);
});
