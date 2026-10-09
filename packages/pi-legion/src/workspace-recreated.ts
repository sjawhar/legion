/**
 * The notice a resumed agent gets on its process's first turn when its role launcher found its
 * workspace recreated since its session was last written: `LEGION_WORKSPACE_RECREATED=true`, which
 * the launcher sets on every generation it starts (packages/daemon/internal/launcher,
 * `WorkspaceRecreatedVariable`). Provisioning built that workspace from the issue's branch as
 * pushed, main when nothing was, so what the session remembers doing there may be gone.
 */
export const WORKSPACE_RECREATED_VARIABLE = "LEGION_WORKSPACE_RECREATED";

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

/**
 * The messages one provider request carries, with `notice` as a user message right after any
 * leading compaction summaries. Oh My Pi hands a `context` handler a copy made for that one request
 * and stores nothing it returns, so the notice is inserted on every request of the turn it is owed
 * to, at one place, and the request's bytes up to and through it repeat across the turn's requests.
 */
export function withWorkspaceRecreatedNotice(
  messages: readonly unknown[],
  notice: string
): unknown[] {
  let index = 0;
  for (const message of messages) {
    const summary =
      typeof message === "object" &&
      message !== null &&
      "role" in message &&
      message.role === "compactionSummary";
    if (!summary) break;
    index += 1;
  }
  return [
    ...messages.slice(0, index),
    { role: "user", content: [{ type: "text", text: notice }], timestamp: 0 },
    ...messages.slice(index),
  ];
}
