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
