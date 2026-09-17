import { useState } from "react";

/** A reader preference kept in localStorage under `key`. With `defaultOn`, anything but a stored
 *  `"false"` reads as on; without it, only a stored `"true"` reads as on. */
function usePersistedFlag(key: string, defaultOn: boolean): [boolean, (next: boolean) => void] {
  const [value, setValue] = useState(() => {
    const stored = window.localStorage.getItem(key);
    return defaultOn ? stored !== "false" : stored === "true";
  });
  const setAndPersist = (next: boolean) => {
    setValue(next);
    window.localStorage.setItem(key, String(next));
  };

  return [value, setAndPersist];
}

export function useShowActivity(): [boolean, (next: boolean) => void] {
  return usePersistedFlag("dispatch.conversation.showActivity", true);
}

/** Default off: a retracted ask is withdrawn history, shown only when a reader asks. */
export function useShowRetracted(): [boolean, (next: boolean) => void] {
  return usePersistedFlag("dispatch.conversation.showRetracted", false);
}
