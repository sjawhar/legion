import { type ReactNode, useEffect } from "react";

import { ChordIndicator } from "./ChordIndicator";
import { appKeymap } from "./keymap";

/** Binds the application registry to `window` keydown and renders the chord indicator. */
export function KeymapProvider({ children }: { children: ReactNode }): ReactNode {
  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => appKeymap.handleKeyDown(event);
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, []);

  return (
    <>
      {children}
      <ChordIndicator />
    </>
  );
}
