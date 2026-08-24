import { beforeEach, describe, expect, it } from "bun:test";
import type { CommandRunner } from "../../state/fetch";
import { type ApprovalCheckDeps, computeApprovalState, setApprovalStatus } from "../approval-check";

describe("Legion human-approval status backstop", () => {
  const appLogins = ["legion-implement", "legion-review"];
  let commands: string[][];
  let tokenCalls: Array<{ role: string; owner: string }>;
  let runner: CommandRunner;

  beforeEach(() => {
    commands = [];
    tokenCalls = [];
    runner = async (command) => {
      commands.push(command);
      if (command[2]?.includes("/reviews")) {
        return {
          stdout: JSON.stringify([
            { user: { login: "human" }, state: "APPROVED", commit_id: "head-sha" },
          ]),
          stderr: "",
          exitCode: 0,
        };
      }
      return { stdout: "{}", stderr: "", exitCode: 0 };
    };
  });

  function deps(gatesMerge: "human" | "off" = "human"): ApprovalCheckDeps {
    return {
      runner,
      tokenManager: {
        getToken: async (role, owner) => {
          tokenCalls.push({ role, owner });
          return {
            token: `minted-${role}-${owner}`,
            expiresAt: "2099-01-01T00:00:00.000Z",
            gitIdentity: {
              name: "legion-implement[bot]",
              email: "42+legion-implement[bot]@users.noreply.github.com",
            },
          };
        },
      },
      appLogins,
      gatesMerge,
    };
  }

  it("keeps App-only approvals pending", () => {
    expect(
      computeApprovalState(
        [{ author: "legion-review", state: "APPROVED", commitId: "head-sha" }],
        "head-sha",
        appLogins
      )
    ).toBe("pending");
  });

  it("accepts a human approval on the current head", () => {
    expect(
      computeApprovalState(
        [{ author: "human", state: "APPROVED", commitId: "head-sha" }],
        "head-sha",
        appLogins
      )
    ).toBe("success");
  });

  it("keeps a human approval on a superseded commit pending", () => {
    expect(
      computeApprovalState(
        [{ author: "human", state: "APPROVED", commitId: "old-sha" }],
        "head-sha",
        appLogins
      )
    ).toBe("pending");
  });

  it("reads reviews using the effect PR and writes the exact success status argv", async () => {
    await setApprovalStatus({ repo: "acme/widgets", pr: 42, sha: "head-sha" }, deps());

    expect(tokenCalls).toEqual([{ role: "implement", owner: "acme" }]);
    expect(commands).toEqual([
      ["gh", "api", "repos/acme/widgets/pulls/42/reviews"],
      [
        "gh",
        "api",
        "-X",
        "POST",
        "repos/acme/widgets/statuses/head-sha",
        "-f",
        "context=legion-human-approval",
        "-f",
        "state=success",
        "-f",
        "description=Approved by a human on the current head",
      ],
    ]);
  });

  it("posts pending for a synchronize effect on a new head", async () => {
    await setApprovalStatus({ repo: "acme/widgets", pr: 42, sha: "new-head" }, deps());

    expect(commands[1]).toEqual([
      "gh",
      "api",
      "-X",
      "POST",
      "repos/acme/widgets/statuses/new-head",
      "-f",
      "context=legion-human-approval",
      "-f",
      "state=pending",
      "-f",
      "description=Awaiting human approval on the current head",
    ]);
  });

  it("does not invoke GitHub when the merge gate is off", async () => {
    await setApprovalStatus({ repo: "acme/widgets", pr: 42, sha: "head-sha" }, deps("off"));

    expect(commands).toEqual([]);
    expect(tokenCalls).toEqual([]);
  });
});
