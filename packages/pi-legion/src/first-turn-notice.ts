/**
 * What a first-turn notice is made of, shared by every notice a Legion session is owed before its
 * next turn: the recreated workspace (`workspace-recreated.ts`) and the setup command that could
 * not install this repository's packages (`setup-report.ts`). Each notice is saved to the session
 * under a custom message type of its own, and every helper here takes the type it judges, so one
 * notice's copy never stands in for another's.
 */

/**
 * One process's notice: its text, and the id every copy it sends carries (`details.id`), which
 * tells its copies from a notice an earlier process of the same session left in the history.
 */
export interface FirstTurnNotice {
  readonly id: string;
  readonly content: string;
}

/** The custom message a notice is saved in the session as. */
export interface FirstTurnNoticeMessage {
  readonly customType: string;
  readonly content: string;
  readonly display: true;
  readonly details: { readonly id: string };
}

/** The custom message that carries `notice` under `customType`, as the session saves it. */
export function firstTurnNoticeMessage(
  notice: FirstTurnNotice,
  customType: string
): FirstTurnNoticeMessage {
  return {
    customType,
    content: notice.content,
    display: true,
    details: { id: notice.id },
  };
}

/**
 * Whether `value`, a branch entry or a request's message, is a copy of the notice `customType` and
 * `id` name.
 */
export function isNoticeCopy(
  value: unknown,
  kind: "entry" | "message",
  customType: string,
  id: string
): boolean {
  if (typeof value !== "object" || value === null) return false;
  const typed =
    kind === "entry"
      ? "type" in value && value.type === "custom_message"
      : "role" in value && value.role === "custom";
  return (
    typed &&
    "customType" in value &&
    value.customType === customType &&
    "details" in value &&
    typeof value.details === "object" &&
    value.details !== null &&
    "id" in value.details &&
    value.details.id === id
  );
}

/**
 * Whether `branch`, a session's active branch, holds a copy of the notice `customType` and `id`
 * name. A recovery of a failed last turn that moves the branch back past it (Oh My Pi drops an
 * empty `length` stop that way) leaves it on no branch the session reads; a notice an earlier
 * process saved is not it.
 */
export function branchHoldsNotice(
  branch: readonly unknown[],
  customType: string,
  id: string
): boolean {
  return branch.some((entry) => isNoticeCopy(entry, "entry", customType, id));
}

/**
 * `messages`, one provider request's, with every copy of the notice `customType` and `id` name but
 * the first left out, or undefined when it holds at most one. A recovery that took the saved copy
 * off the branch leaves it in the process's live context, so the copy sent again for the stored
 * branch would otherwise reach the model twice in every later request of that process. A notice an
 * earlier process left in the history is history, and stays.
 */
export function withoutRepeatedNotice(
  messages: readonly unknown[],
  customType: string,
  id: string
): unknown[] | undefined {
  const first = messages.findIndex((message) => isNoticeCopy(message, "message", customType, id));
  if (first === -1) return undefined;
  const kept = messages.filter(
    (message, index) => index <= first || !isNoticeCopy(message, "message", customType, id)
  );
  return kept.length === messages.length ? undefined : kept;
}
