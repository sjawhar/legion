import { useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useState } from "react";

import { whoAmIQuery } from "../../api/queries";

export type UserPreference =
  | "agents.pinned"
  | "inbox.view"
  | "project.board-edges"
  | "project.issue-filters"
  | "project.issue-view"
  | "shell.margin"
  | "shell.margin-width"
  | "shell.sidebar";

/** Browser-only preferences are private to the signed-in Dispatch identity. */
export function userPreferenceStorageKey(login: string, preference: UserPreference): string {
  return `dispatch.${preference}:${login}`;
}

/** Reads a browser preference for the signed-in identity and resets to its caller's default otherwise. */
export function useUserPreference<T>(
  preference: UserPreference,
  read: (stored: string | null) => T,
  write: (value: T) => string
): [T, (next: T) => void] {
  const whoAmI = useQuery(whoAmIQuery());
  const login = whoAmI.data?.login;
  const [value, setValue] = useState(() =>
    read(
      login === undefined
        ? null
        : window.localStorage.getItem(userPreferenceStorageKey(login, preference))
    )
  );
  useEffect(() => {
    setValue(
      read(
        login === undefined
          ? null
          : window.localStorage.getItem(userPreferenceStorageKey(login, preference))
      )
    );
  }, [login, preference, read]);
  const setAndPersist = useCallback(
    (next: T) => {
      setValue(next);
      if (login !== undefined) {
        window.localStorage.setItem(userPreferenceStorageKey(login, preference), write(next));
      }
    },
    [login, preference, write]
  );
  return [value, setAndPersist];
}
