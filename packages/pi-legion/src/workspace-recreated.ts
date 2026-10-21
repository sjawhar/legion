import {
  type FirstTurnNotice,
  type FirstTurnNoticeMessage,
  firstTurnNoticeMessage,
} from "./first-turn-notice";

/**
 * The notice a resumed agent gets when its role launcher found its workspace recreated since its
 * session was last written: `LEGION_WORKSPACE_RECREATED=true`, which the launcher sets on every
 * generation it starts (packages/daemon/internal/launcher, `WorkspaceRecreatedVariable`).
 * Provisioning built that workspace from the issue's branch as pushed, the repository's default
 * branch when nothing was, so what the session remembers doing there may be gone.
 */
export const WORKSPACE_RECREATED_VARIABLE = "LEGION_WORKSPACE_RECREATED";

/** The custom message type the notice is saved in the session under. */
export const WORKSPACE_RECREATED_MESSAGE = "legion-workspace-recreated";

/**
 * One process's notice: its text, and the id every copy it sends carries (`details.id`), which
 * tells its copies from a notice an earlier recreation of the same session left in the history.
 */
export type WorkspaceRecreatedNotice = FirstTurnNotice;

/**
 * The notice for a Legion session on `issue` whose environment is `environment`, undefined when its
 * launcher said nothing was recreated.
 */
export function workspaceRecreatedNotice(
  environment: Readonly<Record<string, string | undefined>>,
  issue: string
): WorkspaceRecreatedNotice | undefined {
  if (environment[WORKSPACE_RECREATED_VARIABLE] !== "true") return undefined;
  return {
    id: crypto.randomUUID(),
    content:
      `Your workspace was recreated since your last turn: it holds what was pushed to legion/${issue} ` +
      "(the repository's default branch if nothing was), and anything you had not pushed is gone. " +
      "Before you continue, check the workspace against what you remember doing and re-read your " +
      "last handoff.",
  };
}

/** The custom message that carries `notice`, as the session saves it. */
export function workspaceRecreatedMessage(
  notice: WorkspaceRecreatedNotice
): FirstTurnNoticeMessage {
  return firstTurnNoticeMessage(notice, WORKSPACE_RECREATED_MESSAGE);
}
