import { describe, expect, test } from "bun:test";
import {
  SETUP_REPORT_MESSAGE,
  setupReportMessage,
  setupReportNotice,
  setupReportPath,
} from "./setup-report";

const environment = {
  LEGION_STATE_DIR: "/state",
  LEGION_ISSUE: "REPO-43",
  LEGION_ROLE: "implementer",
  LEGION_GENERATION: "2",
};

/** A report of this environment's own install, as scripts/legion-setup.sh writes it. */
const report = {
  schemaVersion: 1,
  kind: "install-failed",
  issue: "REPO-43",
  role: "implementer",
  generation: "2",
  at: "2026-10-21T09:12:44Z",
  reason: "install-timeout",
  exitStatus: 124,
  detail: "error: failed to resolve @oh-my-pi/pi-utils",
};

/** `readFile` as the notice is handed it: one file, whose text is `text`. */
const reads = (text: string) => {
  const files: string[] = [];
  return {
    files,
    readFile: (file: string) => {
      files.push(file);
      return text;
    },
  };
};
const noticeFor = (
  overrides: Readonly<Record<string, unknown>>,
  environmentOverrides: Readonly<Record<string, string | undefined>> = {}
) =>
  setupReportNotice(
    { ...environment, ...environmentOverrides },
    reads(JSON.stringify({ ...report, ...overrides })).readFile
  );

test("the report is the state directory's, and nothing without one", () => {
  expect(setupReportPath(environment)).toBe("/state/legion-setup-report.json");
  for (const stateDirectory of [undefined, ""]) {
    expect(setupReportPath({ ...environment, LEGION_STATE_DIR: stateDirectory })).toBeUndefined();
  }
});

test("an install this role's own generation failed is read from the state directory", () => {
  const { files, readFile } = reads(JSON.stringify(report));

  const notice = setupReportNotice(environment, readFile);

  expect(files).toEqual(["/state/legion-setup-report.json"]);
  expect(notice?.id).toMatch(/^[0-9a-f-]{36}$/);
  // The failure as the report carries it, and the install log's tail — introduced as a log only
  // because an install ran to leave one.
  expect(notice?.content).toContain('reason "install-timeout", exit status 124');
  expect(notice?.content).toContain(
    "The install log ended: error: failed to resolve @oh-my-pi/pi-utils"
  );
  // What the agent is to do about it: no check command of this repository can run here, CI's
  // required jobs are the authority meanwhile, and the retry is the setup script rather than a
  // bare `bun install`, which takes neither the lock the roles share nor the marker — the reason
  // the notice gives, since a role container's own BUN_INSTALL_CACHE_DIR already names the
  // issue's volume and the cache is not what makes the bare command wrong there.
  expect(notice?.content).toContain("check commands cannot run here");
  expect(notice?.content).toContain("CI's required jobs are the authority");
  expect(notice?.content).toContain("sh scripts/legion-setup.sh");
  expect(notice?.content).toContain("takes the lock every role of this issue shares");
  expect(notice?.content).toMatch(/A bare `bun install` does none of those three/);
});

test.each([
  [
    "lock-timeout",
    199,
    "another install held /legion/scratch/bun/install/cache for this run's whole 270s budget",
  ],
  [
    "no-install-root",
    0,
    "no writable install root: neither /legion nor the workspace can be written under",
  ],
])("a %s report, whose install never ran, introduces its detail as what it is", (reason, exitStatus, detail) => {
  const { readFile } = reads(JSON.stringify({ ...report, reason, exitStatus, detail }));

  const notice = setupReportNotice(environment, readFile);

  // The script writes these two from a run that attempted no install: one never held the lock,
  // the other had no directory to write in. Calling their detail an install log is a report
  // asserting something that did not happen.
  expect(notice?.content).toContain(`It reported: ${detail}`);
  expect(notice?.content).not.toContain("The install log ended");
});

test("a state directory this environment does not name is never read", () => {
  const { files, readFile } = reads(JSON.stringify(report));

  expect(
    setupReportNotice({ ...environment, LEGION_STATE_DIR: undefined }, readFile)
  ).toBeUndefined();
  expect(files).toEqual([]);
});

test("each process's notice carries an id of its own", () => {
  const first = setupReportNotice(environment, reads(JSON.stringify(report)).readFile);
  const second = setupReportNotice(environment, reads(JSON.stringify(report)).readFile);

  expect(first?.id).not.toBe(second?.id);
});

// LEGION_STATE_DIR outlives a generation in a pod and, under the tmux runtime, is shared by every
// pane of the host: only a report of this issue, this role and this generation is this agent's.
describe("a report of another run", () => {
  test("is no notice, whichever of the three names another run", () => {
    expect(noticeFor({ generation: "1" })).toBeUndefined();
    expect(noticeFor({ role: "tester" })).toBeUndefined();
    expect(noticeFor({ issue: "REPO-44" })).toBeUndefined();
  });

  test("is no notice when it names none of them, or names them as something other than strings", () => {
    expect(noticeFor({ generation: undefined })).toBeUndefined();
    expect(noticeFor({ role: undefined })).toBeUndefined();
    expect(noticeFor({ issue: undefined })).toBeUndefined();
    expect(noticeFor({ generation: 2 })).toBeUndefined();
  });

  test("is no notice for an environment that names none of them either", () => {
    expect(noticeFor({ generation: undefined }, { LEGION_GENERATION: undefined })).toBeUndefined();
    expect(noticeFor({}, { LEGION_ROLE: undefined })).toBeUndefined();
  });
});

describe("a file that tells this agent nothing", () => {
  test("is no notice and no exception", () => {
    const unreadable = (file: string): string => {
      throw new Error(`ENOENT: no such file or directory, open '${file}'`);
    };

    expect(setupReportNotice(environment, unreadable)).toBeUndefined();
    expect(setupReportNotice(environment, reads("").readFile)).toBeUndefined();
    expect(setupReportNotice(environment, reads("{not json").readFile)).toBeUndefined();
    expect(setupReportNotice(environment, reads("[]").readFile)).toBeUndefined();
    expect(setupReportNotice(environment, reads("null").readFile)).toBeUndefined();
    expect(setupReportNotice(environment, reads('"install-failed"').readFile)).toBeUndefined();
  });

  test("of another kind is no notice", () => {
    expect(noticeFor({ kind: "install-skipped" })).toBeUndefined();
    expect(noticeFor({ kind: undefined })).toBeUndefined();
  });
});

describe("a report whose fields are not what the schema says", () => {
  test("names what it can and leaves out what it cannot", () => {
    const notice = noticeFor({ reason: undefined, exitStatus: "124", detail: undefined });

    expect(notice?.content).toContain('reason "unknown", exit status unknown');
    expect(notice?.content).not.toContain("The install log ended");
  });

  test("quotes the detail's tail alone when it is longer than the notice carries", () => {
    const head = "the install started here";
    const detail = `${head} ${"noise ".repeat(200)}error: the last thing bun said`;
    const notice = noticeFor({ detail });

    expect(notice?.content).toContain("The install log ended: …");
    expect(notice?.content).toContain("error: the last thing bun said");
    expect(notice?.content).not.toContain(head);
  });
});

test("the message carries the notice under the type the session saves it as", () => {
  const notice = { id: "n-1", content: "the install failed" };

  expect(setupReportMessage(notice)).toEqual({
    customType: SETUP_REPORT_MESSAGE,
    content: "the install failed",
    display: true,
    details: { id: "n-1" },
  });
  expect(SETUP_REPORT_MESSAGE).toBe("legion-setup-report");
});
