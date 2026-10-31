import { expect, test } from "bun:test";
import { WORKSPACE_RECREATED_VARIABLE, workspaceRecreatedNotice } from "./workspace-recreated";

test("a launcher that recreated the workspace earns a notice naming the issue's branch", () => {
  const notice = workspaceRecreatedNotice({ [WORKSPACE_RECREATED_VARIABLE]: "true" }, "REPO-43");
  expect(notice?.content).toContain("it holds what was pushed to legion/REPO-43");
  expect(notice?.content).toContain("anything you had not pushed is gone");
  expect(notice?.id).toMatch(/^[0-9a-f-]{36}$/);
});

// The id is what a host tells this process's copies of the notice by, so two notices never share
// one.
test("each notice carries an id of its own", () => {
  const environment = { [WORKSPACE_RECREATED_VARIABLE]: "true" };
  const first = workspaceRecreatedNotice(environment, "REPO-43");
  const second = workspaceRecreatedNotice(environment, "REPO-43");
  expect(first?.id).not.toBe(second?.id);
});

test("a launcher that recreated nothing says nothing, and only `true` is recreated", () => {
  expect(workspaceRecreatedNotice({}, "REPO-43")).toBeUndefined();
  for (const value of ["", "false", "TRUE", "1"]) {
    expect(
      workspaceRecreatedNotice({ [WORKSPACE_RECREATED_VARIABLE]: value }, "REPO-43")
    ).toBeUndefined();
  }
});
