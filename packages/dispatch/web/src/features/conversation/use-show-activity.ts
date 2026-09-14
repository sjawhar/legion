import { useState } from "react";

const storageKey = "dispatch.conversation.showActivity";

export function useShowActivity(): [boolean, (next: boolean) => void] {
  const [showActivity, setShowActivity] = useState(
    () => window.localStorage.getItem(storageKey) !== "false"
  );
  const setAndPersist = (next: boolean) => {
    setShowActivity(next);
    window.localStorage.setItem(storageKey, String(next));
  };

  return [showActivity, setAndPersist];
}

const retractedStorageKey = "dispatch.conversation.showRetracted";

/** Default off: a retracted ask is withdrawn history, shown only when a reader asks. */
export function useShowRetracted(): [boolean, (next: boolean) => void] {
  const [showRetracted, setShowRetracted] = useState(
    () => window.localStorage.getItem(retractedStorageKey) === "true"
  );
  const setAndPersist = (next: boolean) => {
    setShowRetracted(next);
    window.localStorage.setItem(retractedStorageKey, String(next));
  };

  return [showRetracted, setAndPersist];
}
