import { afterAll, describe, expect, it, mock } from "bun:test";
import { mkdtempSync, realpathSync, rmSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { hostAgentRegistryMock } from "@legion/pi-shared/test/host-registry";

// The module reaches the Oh My Pi host through `@legion/pi-shared/subagent-session`, which only
// the binary resolves; the import below must come after the mock.
mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
  ...hostAgentRegistryMock,
}));

const { attributionInputs } = await import("./claim-session");

const ROOT_TRANSCRIPT =
  "/sessions/-legion-default/2026-08-30T03-47-40-329Z_01a050c7-b4a9-7000-a539-240e20271441.jsonl";
const ROOT_ID = "01a050c7-b4a9-7000-a539-240e20271441";

// The pane's workspace reaches `attributionInputs` as the daemon set it; jj matches a scope
// against the root it resolved, so a link's own path must never reach the overlay.
const SCRATCH = mkdtempSync(path.join(tmpdir(), "claim-session-ws-"));
const REAL_WORKSPACE = realpathSync(SCRATCH);
const WORKSPACE = path.join(REAL_WORKSPACE, "link");
symlinkSync(REAL_WORKSPACE, WORKSPACE);
afterAll(() => rmSync(SCRATCH, { recursive: true, force: true }));

describe("attributionInputs", () => {
  it("reads the session id, the state directory, the trailer key and the workspace", () => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, {
        LEGION_STATE_DIR: "/state",
        LEGION_ISSUE_TRAILER: "ACME-7",
        LEGION_WORKSPACE: WORKSPACE,
      })
    ).toEqual({
      sessionId: ROOT_ID,
      stateDir: "/state",
      issueKey: "ACME-7",
      workspace: REAL_WORKSPACE,
    });
  });

  // jj matches a `[[--scope]]` against the workspace root it resolved, and a path that does not
  // resolve matches nothing and says nothing: the trailer is then the session line alone.
  it.each([
    ["no LEGION_WORKSPACE", undefined],
    ["an empty LEGION_WORKSPACE", ""],
    ["a workspace that does not exist", "/no/such/workspace"],
  ])("takes no workspace from %s", (_name, workspace) => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, {
        LEGION_STATE_DIR: "/state",
        LEGION_ISSUE_TRAILER: "ACME-7",
        ...(workspace === undefined ? {} : { LEGION_WORKSPACE: workspace }),
      }).workspace
    ).toBeUndefined();
  });

  // The daemon sets LEGION_ISSUE for every pane; it sets LEGION_ISSUE_TRAILER only for an issue
  // whose repository the deployment names. A caller reading LEGION_ISSUE would stamp a Dispatch
  // key into the commits of a repository it never named (dispatch://INFRA-472).
  it("takes no key from LEGION_ISSUE", () => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, { LEGION_STATE_DIR: "/state", LEGION_ISSUE: "ACME-7" })
        .issueKey
    ).toBeUndefined();
  });

  it("takes no key from an empty LEGION_ISSUE_TRAILER", () => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, { LEGION_STATE_DIR: "/state", LEGION_ISSUE_TRAILER: "" })
        .issueKey
    ).toBeUndefined();
  });

  it("yields no session id for a subagent transcript", () => {
    expect(
      attributionInputs("/sessions/-x/2026-08-30T03-47-40-329Z_abc/Worker.jsonl", {
        LEGION_STATE_DIR: "/state",
        LEGION_ISSUE_TRAILER: "ACME-7",
      }).sessionId
    ).toBeUndefined();
  });

  it("fails the pane that has no state directory", () => {
    expect(() => attributionInputs(ROOT_TRANSCRIPT, {})).toThrow(
      "LEGION_STATE_DIR is required for Legion"
    );
  });
});
