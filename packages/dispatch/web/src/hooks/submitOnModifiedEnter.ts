import type { KeyboardEvent } from "react";

/** Leaves Enter to a textarea while using Ctrl/Cmd+Enter for an explicit submission. */
export function submitOnModifiedEnter(
  event: KeyboardEvent<HTMLElement>,
  onFormlessSubmit?: () => void
): boolean {
  if (
    event.key !== "Enter" ||
    event.nativeEvent.isComposing ||
    (!event.ctrlKey && !event.metaKey)
  ) {
    return false;
  }
  event.preventDefault();
  const form = event.currentTarget instanceof HTMLTextAreaElement ? event.currentTarget.form : null;
  if (form === null) {
    onFormlessSubmit?.();
  } else {
    form.requestSubmit();
  }
  return true;
}
