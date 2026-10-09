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

/**
 * The notice for a Legion session on `issue` whose environment is `environment`, undefined when its
 * launcher said nothing was recreated.
 */
export function workspaceRecreatedNotice(
  environment: Readonly<Record<string, string | undefined>>,
  issue: string
): string | undefined {
  if (environment[WORKSPACE_RECREATED_VARIABLE] !== "true") return undefined;
  return (
    `Your workspace was recreated since your last turn: it holds what was pushed to legion/${issue} ` +
    "(main if nothing was), and anything you had not pushed is gone. Before you continue, check " +
    "the workspace against what you remember doing and re-read your last handoff."
  );
}

/**
 * Whether `branch`, a session's active branch, holds the notice: a recovery of a failed last turn
 * that moves the branch back past it (Oh My Pi drops an empty `length` stop that way) leaves the
 * notice on no branch the session reads.
 */
export function branchHoldsWorkspaceRecreatedNotice(branch: readonly unknown[]): boolean {
  return branch.some(
    (entry) =>
      typeof entry === "object" &&
      entry !== null &&
      "type" in entry &&
      entry.type === "custom_message" &&
      "customType" in entry &&
      entry.customType === WORKSPACE_RECREATED_MESSAGE
  );
}

/**
 * `messages`, one provider request's, with every copy of the notice but the first left out, or
 * undefined when it holds at most one. A recovery that took the saved notice off the branch leaves
 * it in the process's live context, so the copy before_agent_start sends again for the stored
 * branch would otherwise reach the model twice in every later request of that process.
 */
export function withoutRepeatedWorkspaceRecreatedNotice(
  messages: readonly unknown[]
): unknown[] | undefined {
  const isNotice = (message: unknown): boolean =>
    typeof message === "object" &&
    message !== null &&
    "role" in message &&
    message.role === "custom" &&
    "customType" in message &&
    message.customType === WORKSPACE_RECREATED_MESSAGE;
  const first = messages.findIndex(isNotice);
  if (first === -1 || messages.findIndex((m, i) => i > first && isNotice(m)) === -1) {
    return undefined;
  }
  return messages.filter((message, index) => index <= first || !isNotice(message));
}
