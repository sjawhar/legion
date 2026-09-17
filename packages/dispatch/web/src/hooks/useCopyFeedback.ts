import { useEffect, useState } from "react";

import { copyText } from "../lib/clipboard";

export type CopyStatus = "idle" | "copied" | { failed: string };

/**
 * The state behind every "copy to clipboard" control: `copy` writes the text and reports the
 * outcome; a success shows as `copied` for 1.5 s and then clears, a failure stays (carrying the
 * text that could not be copied) until the next attempt or a `reset`.
 */
export function useCopyFeedback(): {
  copy: (text: string) => void;
  reset: () => void;
  status: CopyStatus;
} {
  const [status, setStatus] = useState<CopyStatus>("idle");
  useEffect(() => {
    if (status !== "copied") {
      return;
    }
    const timeout = window.setTimeout(() => setStatus("idle"), 1500);
    return () => window.clearTimeout(timeout);
  }, [status]);
  const copy = (text: string) => {
    void copyText(text).then((copied) => setStatus(copied ? "copied" : { failed: text }));
  };
  const reset = () => setStatus("idle");
  return { copy, reset, status };
}
