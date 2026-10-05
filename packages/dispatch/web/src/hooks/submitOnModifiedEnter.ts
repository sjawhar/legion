import type { KeyboardEvent } from "react";

/**
 * Leaves Enter to a textarea while using Ctrl/Cmd+Enter for an explicit submission. A field whose
 * draft is still taking a picture passes `disabled` while the upload is out, and the key then
 * submits nothing: a draft sent before its upload lands would go without the picture.
 */
export function submitOnModifiedEnter(
  event: KeyboardEvent<HTMLElement>,
  { disabled = false, onFormlessSubmit }: { disabled?: boolean; onFormlessSubmit?: () => void } = {}
): boolean {
  if (
    disabled ||
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
