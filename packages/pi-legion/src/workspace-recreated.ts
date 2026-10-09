/**
 * The notice a resumed agent gets when its role launcher found its workspace recreated since its
 * session was last written: `LEGION_WORKSPACE_RECREATED=true`, which the launcher sets on every
 * generation it starts (packages/daemon/internal/launcher, `WorkspaceRecreatedVariable`).
 * Provisioning built that workspace from the issue's branch as pushed, main when nothing was, so
 * what the session remembers doing there may be gone.
 */
export const WORKSPACE_RECREATED_VARIABLE = "LEGION_WORKSPACE_RECREATED";

/** The custom message type the notice is saved in the session under. */
export const WORKSPACE_RECREATED_MESSAGE = "legion-workspace-recreated";

/** The notice for `environment`'s pane, undefined when its launcher said nothing was recreated. */
export function workspaceRecreatedNotice(
  environment: Readonly<Record<string, string | undefined>>
): string | undefined {
  if (environment[WORKSPACE_RECREATED_VARIABLE] !== "true") return undefined;
  const issue = environment.LEGION_ISSUE;
  const branch = issue === undefined || issue === "" ? "the issue's branch" : `legion/${issue}`;
  return (
    `Your workspace was recreated since your last turn: it holds what was pushed to ${branch} ` +
    "(main if nothing was), and anything you had not pushed is gone. Before you continue, check " +
    "the workspace against what you remember doing and re-read your last handoff."
  );
}
