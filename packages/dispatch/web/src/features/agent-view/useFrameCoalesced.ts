import { useEffect, useState } from "react";

/**
 * `value` as of the last animation frame: a value that changes several times between two
 * paints (a streamed turn's text, growing by a delta per message) is read once per frame, so
 * work keyed on it (a Markdown re-parse of the whole turn, which `MarkdownBody` does in the
 * layout phase) runs at most once per painted frame rather than once per delta. The first value
 * is taken at once, so a mounted turn renders in its mounting commit; a change settles within a
 * frame, which is before anyone can read it. Where `requestAnimationFrame` does not exist (a
 * test runtime without a document), the value passes straight through.
 */
export function useFrameCoalesced<T>(value: T): T {
  const hasFrames = typeof requestAnimationFrame === "function";
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    // Where `requestAnimationFrame` does not exist (a test runtime without a document) the value
    // passes straight through, so there is nothing to settle and no re-render to force.
    if (!hasFrames || Object.is(settled, value)) {
      return;
    }
    const frame = requestAnimationFrame(() => setSettled(value));
    return () => cancelAnimationFrame(frame);
  }, [hasFrames, settled, value]);
  return hasFrames ? settled : value;
}
