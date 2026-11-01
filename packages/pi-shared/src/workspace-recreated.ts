/**
 * The notice a resumed agent gets when its role launcher found its workspace recreated since its
 * session was last written: `LEGION_WORKSPACE_RECREATED=true`, which the launcher sets on every
 * generation it starts (packages/daemon/internal/launcher, `WorkspaceRecreatedVariable`).
 * Provisioning built that workspace from the issue's branch as pushed, the repository's default
 * branch when nothing was, so what the session remembers doing there may be gone.
 *
 * The text and the identity of one process's notice live here because both harnesses tell a
 * worker the same thing (INFRA-220 D29): the Oh My Pi entry saves it as a custom message ahead of
 * the next turn, and the Claude Code pane's Legion server writes it into the session over the
 * bridge's inbox once its claim has booted. The helpers that read an Oh My Pi transcript branch
 * stay with that host (`packages/pi-legion/src/workspace-recreated.ts`), so neither host reaches
 * into the other's history model.
 */
export const WORKSPACE_RECREATED_VARIABLE = "LEGION_WORKSPACE_RECREATED";

/**
 * How the same launcher says whether the pod's one workspace-bundle restore put the released
 * workspace's unpushed work back (`LEGION_WORKSPACE_RESTORED`, `launcher.WorkspaceRestoredVariable`).
 * A workspace recreated because the daemon released an idle pod holds the work its session
 * remembers; one recreated by anything else holds only what was pushed, and the agent is told
 * which of the two it has.
 */
export const WORKSPACE_RESTORED_VARIABLE = "LEGION_WORKSPACE_RESTORED";

/**
 * One process's notice: its text, and the id every copy it sends carries (`details.id`), which
 * tells its copies from a notice an earlier recreation of the same session left in the history.
 */
export interface WorkspaceRecreatedNotice {
  readonly id: string;
  readonly content: string;
}

/** Whether the launcher of the process whose environment is `environment` recreated its
 * workspace. It decides this afresh for every launch. */
export function workspaceRecreated(
  environment: Readonly<Record<string, string | undefined>>
): boolean {
  return environment[WORKSPACE_RECREATED_VARIABLE] === "true";
}

/**
 * The notice for a Legion session on `issue` whose environment is `environment`, undefined when its
 * launcher said nothing was recreated.
 */
export function workspaceRecreatedNotice(
  environment: Readonly<Record<string, string | undefined>>,
  issue: string
): WorkspaceRecreatedNotice | undefined {
  if (!workspaceRecreated(environment)) return undefined;
  if (environment[WORKSPACE_RESTORED_VARIABLE] === "true") {
    return {
      id: crypto.randomUUID(),
      content:
        `Your workspace was made again since your last turn: the daemon released this issue's idle ` +
        `pod and gave its CPU and memory back. It holds what was pushed to legion/${issue} and the ` +
        "tracked work you had not pushed, which the daemon saved and put back. What is gone is what " +
        "no commit held: the files your scratch directory held, files git ignores, and anything you " +
        "started that was running. Check the workspace against what you remember doing before you " +
        "continue.",
    };
  }
  return {
    id: crypto.randomUUID(),
    content:
      `Your workspace was recreated since your last turn: it holds what was pushed to legion/${issue} ` +
      "(the repository's default branch if nothing was), and anything you had not pushed is gone. " +
      "Before you continue, check the workspace against what you remember doing and re-read your " +
      "last handoff.",
  };
}
