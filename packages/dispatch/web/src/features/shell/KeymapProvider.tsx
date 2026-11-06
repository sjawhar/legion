import { type ReactNode, useEffect } from "react";

import { replayEarlyKeys } from "../../early-keys";
import { ChordIndicator } from "./ChordIndicator";
import { appKeymap } from "./keymap";

/** Binds the application registry to `window` keydown and renders the chord indicator. */
export function KeymapProvider({ children }: { children: ReactNode }): ReactNode {
  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => appKeymap.handleKeyDown(event);
    window.addEventListener("keydown", handleKeyDown);
    // A shortcut the reader pressed while the skeleton was on the screen, before this listener
    // existed: the entry held it and this is the first moment it can be answered
    // (`early-keys.ts`, dispatch://INFRA-341, R11).
    replayEarlyKeys(handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, []);

  return (
    <>
      {children}
      <ChordIndicator />
    </>
  );
}
