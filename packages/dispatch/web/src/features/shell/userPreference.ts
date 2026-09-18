import { useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";

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

type UserPreferenceIdentity = "failed" | "pending" | "resolved";

interface UserPreferenceOptions<T> {
  failed?: T;
  pending?: T;
  refreshOn?: unknown;
}

function preferenceValue<T>(
  identity: UserPreferenceIdentity,
  login: string | undefined,
  preference: UserPreference,
  read: (stored: string | null) => T,
  options: UserPreferenceOptions<T> | undefined
): T {
  const fallback = read(
    identity === "resolved" && login !== undefined
      ? window.localStorage.getItem(userPreferenceStorageKey(login, preference))
      : null
  );
  if (identity === "resolved") return fallback;
  return identity === "pending" ? (options?.pending ?? fallback) : (options?.failed ?? fallback);
}

/**
 * Reads a browser preference for a resolved signed-in identity. Pending and failed identity use
 * the caller's explicit fallback when one differs from `read(null)`; neither state persists.
 */
export function useUserPreference<T>(
  preference: UserPreference,
  read: (stored: string | null) => T,
  write: (value: T) => string,
  options?: UserPreferenceOptions<T>
): [T, (next: T) => void] {
  const whoAmI = useQuery(whoAmIQuery());
  const login = whoAmI.data?.login;
  const identity: UserPreferenceIdentity = whoAmI.isPending
    ? "pending"
    : whoAmI.isError || login === undefined
      ? "failed"
      : "resolved";
  const readRef = useRef(read);
  readRef.current = read;
  const optionsRef = useRef(options);
  optionsRef.current = options;
  const refreshOn = identity === "resolved" ? options?.refreshOn : undefined;
  const [value, setValue] = useState(() =>
    preferenceValue(identity, login, preference, readRef.current, optionsRef.current)
  );
  useEffect(() => {
    // A caller can opt into a re-read when an authenticated default changes.
    void refreshOn;
    setValue(preferenceValue(identity, login, preference, readRef.current, optionsRef.current));
  }, [identity, login, preference, refreshOn]);
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
