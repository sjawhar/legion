import type { WorkspaceRecreatedNotice } from "@legion/pi-shared/workspace-recreated";

/**
 * The Oh My Pi half of the recreated-workspace notice: how this host saves it in a session, and
 * how it tells its own copies apart in a transcript branch. The notice itself — the variable the
 * launcher sets and the text it earns — is shared with the Claude Code host
 * (`@legion/pi-shared/workspace-recreated`), and is re-exported here so this host's entry reads
 * one module.
 */
export {
  type WorkspaceRecreatedNotice,
  workspaceRecreatedNotice,
} from "@legion/pi-shared/workspace-recreated";

/** The custom message type the notice is saved in the session under. */
export const WORKSPACE_RECREATED_MESSAGE = "legion-workspace-recreated";

/** The custom message that carries `notice`, as the session saves it. */
export function workspaceRecreatedMessage(notice: WorkspaceRecreatedNotice): {
  readonly customType: string;
  readonly content: string;
  readonly display: true;
  readonly details: { readonly id: string };
} {
  return {
    customType: WORKSPACE_RECREATED_MESSAGE,
    content: notice.content,
    display: true,
    details: { id: notice.id },
  };
}

/** Whether `value`, a branch entry or a request's message, is a copy of the notice `id` names. */
function isNoticeCopy(value: unknown, kind: "entry" | "message", id: string): boolean {
  if (typeof value !== "object" || value === null) return false;
  const typed =
    kind === "entry"
      ? "type" in value && value.type === "custom_message"
      : "role" in value && value.role === "custom";
  return (
    typed &&
    "customType" in value &&
    value.customType === WORKSPACE_RECREATED_MESSAGE &&
    "details" in value &&
    typeof value.details === "object" &&
    value.details !== null &&
    "id" in value.details &&
    value.details.id === id
  );
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
  return branch.some((entry) => isNoticeCopy(entry, "entry", id));
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
  const first = messages.findIndex((message) => isNoticeCopy(message, "message", id));
  if (first === -1) return undefined;
  const kept = messages.filter(
    (message, index) => index <= first || !isNoticeCopy(message, "message", id)
  );
  return kept.length === messages.length ? undefined : kept;
}
