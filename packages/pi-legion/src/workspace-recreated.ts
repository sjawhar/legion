import {
  branchHoldsNotice,
  type FirstTurnNotice,
  type FirstTurnNoticeMessage,
  firstTurnNoticeMessage,
  withoutRepeatedNotice,
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

/**
 * Whether `branch`, a session's active branch, holds a copy of the notice `id` names. A recovery of
 * a failed last turn that moves the branch back past it (Oh My Pi drops an empty `length` stop that
 * way) leaves it on no branch the session reads; a notice an earlier recreation saved is not it.
 */
export function branchHoldsWorkspaceRecreatedNotice(
  branch: readonly unknown[],
  id: string
): boolean {
  return branchHoldsNotice(branch, WORKSPACE_RECREATED_MESSAGE, id);
}

/**
 * `messages`, one provider request's, with every copy of the notice `id` names but the first left
 * out, or undefined when it holds at most one. A recovery that took the saved copy off the branch
 * leaves it in the process's live context, so the copy sent again for the stored branch would
 * otherwise reach the model twice in every later request of that process. A notice an earlier
 * recreation left in the history is history, and stays.
 */
export function withoutRepeatedWorkspaceRecreatedNotice(
  messages: readonly unknown[],
  id: string
): unknown[] | undefined {
  return withoutRepeatedNotice(messages, WORKSPACE_RECREATED_MESSAGE, id);
}
