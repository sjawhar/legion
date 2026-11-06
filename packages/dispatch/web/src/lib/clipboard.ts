export async function copyText(text: string): Promise<boolean> {
  if (typeof navigator !== "undefined" && navigator.clipboard !== undefined) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Fall through to the legacy path, which also works on non-secure origins.
    }
  }

  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.left = "-9999px";
  textarea.style.position = "fixed";
  textarea.style.top = "0";
  document.body.append(textarea);
  textarea.select();

  try {
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    textarea.remove();
  }
}

/** Copies text that is still being read when the click lands, such as a document fetched again
 *  for the copy. The clipboard item holds the read itself, so the click's permission covers the
 *  copy while the read runs: Safari needs that, and the other engines allow it. Without such an
 *  item, or when the browser refuses it, the text is copied as `copyText` copies it once it has
 *  arrived. False when the read fails or neither path copies. */
export async function copyTextWhenRead(text: Promise<string>): Promise<boolean> {
  if (
    typeof ClipboardItem !== "undefined" &&
    typeof navigator !== "undefined" &&
    typeof navigator.clipboard?.write === "function"
  ) {
    try {
      const blob = text.then((value) => new Blob([value], { type: "text/plain" }));
      await navigator.clipboard.write([new ClipboardItem({ "text/plain": blob })]);
      return true;
    } catch {
      // Fall through to copying the text once it has arrived.
    }
  }
  try {
    return await copyText(await text);
  } catch {
    return false;
  }
}
